import Doc, { Animation, clamp } from './doc'
import OrderBook from './orderbook'
import State from './state'
import { UnitInfo, Candle } from './registry'

const bind = Doc.bind
const PIPI = 2 * Math.PI
const plusChar = String.fromCharCode(59914)
const minusChar = String.fromCharCode(59915)

interface Point {
  x: number
  y: number
}

interface MinMax {
  min: number
  max: number
}

interface Label {
  val: number
  txt: string
}

interface LabelSet {
  widest?: number
  lbls: Label[]
}

export interface Translator {
    x: (x: number) => number
    y: (y: number) => number
    unx: (x: number) => number
    uny: (y: number) => number
    w: (w: number) => number
    h: (h: number) => number
}

export interface MouseReport {
  rate: number
  depth: number
  dotColor: string
  hoverMarkers: number[]
}

export interface VolumeReport {
  buyBase: number
  buyQuote: number
  sellBase: number
  sellQuote: number
}

export interface DepthReporters {
  mouse: (r: MouseReport | null) => void
  click: (x: number) => void
  volume: (r: VolumeReport) => void
  zoom: (z: number) => void
}

export interface ChartReporters {
  resize: () => void,
  click: (e: MouseEvent) => void,
  zoom: (bigger: boolean) => void
}

export interface DepthLine {
  rate: number
  color: string
}

export interface DepthMarker {
  rate: number
  active: boolean
}

interface DepthMark extends DepthMarker {
  qty: number
  sell: boolean
}

interface Theme {
  body: string
  axisLabel: string
  gridBorder: string
  gridLines: string
  gapLine: string
  value: string
  zoom: string
  zoomHover: string
  sellLine: string
  buyLine: string
  sellFill: string
  buyFill: string
  crosshairs: string
  legendFill: string
  legendText: string
}

const darkTheme: Theme = {
  body: '#0b2031',
  axisLabel: '#b1b1b1',
  gridBorder: '#383f4b',
  gridLines: '#383f4b',
  gapLine: '#6b6b6b',
  value: '#9a9a9a',
  zoom: '#5b5b5b',
  zoomHover: '#aaa',
  sellLine: '#ae3333',
  buyLine: '#05a35a',
  sellFill: '#591a1a',
  buyFill: '#02572f',
  crosshairs: '#888',
  legendFill: 'black',
  legendText: '#d5d5d5'
}

const lightTheme: Theme = {
  body: '#f4f4f4',
  axisLabel: '#1b1b1b',
  gridBorder: '#ddd',
  gridLines: '#ddd',
  gapLine: '#595959',
  value: '#4d4d4d',
  zoom: '#777',
  zoomHover: '#333',
  sellLine: '#99302b',
  buyLine: '#207a46',
  sellFill: '#bd5959',
  buyFill: '#4cad75',
  crosshairs: '#595959',
  legendFill: '#e6e6e6',
  legendText: '#1b1b1b'
}

// Chart is the base class for charts.
export class Chart {
  parent: HTMLElement
  report: ChartReporters
  theme: Theme
  canvas: HTMLCanvasElement
  visible: boolean
  renderScheduled: boolean
  ctx: CanvasRenderingContext2D
  mousePos: Point | null
  rect: DOMRect
  wheelLimiter: number | null
  boundResizer: () => void
  plotRegion: Region
  xRegion: Region
  yRegion: Region
  dataExtents: Extents
  unattachers: (() => void)[]

  constructor (parent: HTMLElement, reporters: ChartReporters) {
    this.parent = parent
    this.report = reporters
    this.theme = State.isDark() ? darkTheme : lightTheme
    this.canvas = document.createElement('canvas')
    this.visible = true
    parent.appendChild(this.canvas)
    const ctx = this.canvas.getContext('2d')
    if (!ctx) {
      console.error('error getting canvas context')
      return
    }
    this.ctx = ctx
    this.ctx.textAlign = 'center'
    this.ctx.textBaseline = 'middle'
    // Mouse handling
    this.mousePos = null
    bind(this.canvas, 'mousemove', (e: MouseEvent) => {
      // this.rect will be set in resize().
      if (!this.rect) return
      this.mousePos = {
        x: e.clientX - this.rect.left,
        y: e.clientY - this.rect.y
      }
      this.draw()
    })
    bind(this.canvas, 'mouseleave', () => {
      this.mousePos = null
      this.draw()
    })

    // Bind resize.
    const resizeObserver = new ResizeObserver(() => this.resize())
    resizeObserver.observe(this.parent)

    // Scrolling by wheel is smoother when the rate is slightly limited.
    this.wheelLimiter = null
    bind(this.canvas, 'wheel', (e: WheelEvent) => { this.wheel(e) }, { passive: true })
    bind(this.canvas, 'click', (e: MouseEvent) => { this.click(e) })
    const setVis = () => {
      this.visible = document.visibilityState !== 'hidden'
      if (this.visible && this.renderScheduled) {
        this.renderScheduled = false
        this.draw()
      }
    }
    bind(document, 'visibilitychange', setVis)
    this.unattachers = [() => { Doc.unbind(document, 'visibilitychange', setVis) }]
  }

  wheeled () {
    this.wheelLimiter = window.setTimeout(() => { this.wheelLimiter = null }, 100)
  }

  /* clear the canvas. */
  clear () {
    this.ctx.clearRect(0, 0, this.canvas.width, this.canvas.height)
  }

  /* draw calls the child class's render method. */
  draw () {
    this.render()
  }

  /* click is the handler for a click event on the canvas. */
  click (e: MouseEvent) {
    this.report.click(e)
  }

  /* wheel is a mousewheel event handler. */
  wheel (e: WheelEvent) {
    this.zoom(e.deltaY < 0)
  }

  /*
   * resize updates the chart size. The parentHeight is an argument to support
   * updating the height programmatically after the caller sets a style.height
   * but before the clientHeight has been updated.
   */
  resize () {
    this.canvas.width = this.parent.clientWidth
    this.canvas.height = this.parent.clientHeight
    const xLblHeight = 30
    const yGuess = 40 // y label width guess. Will be adjusted when drawn.
    const plotExtents = new Extents(0, this.canvas.width, 0, this.canvas.height - xLblHeight)
    const xLblExtents = new Extents(0, this.canvas.width, this.canvas.height - xLblHeight, this.canvas.height)
    const yLblExtents = new Extents(0, yGuess, 0, this.canvas.height - xLblHeight)
    this.plotRegion = new Region(this.ctx, plotExtents)
    this.xRegion = new Region(this.ctx, xLblExtents)
    this.yRegion = new Region(this.ctx, yLblExtents)
    // After changing the visibility, this.canvas.getBoundingClientRect will
    // return nonsense until a render.
    window.requestAnimationFrame(() => {
      this.rect = this.canvas.getBoundingClientRect()
      this.report.resize()
    })
  }

  /* zoom is called when the user scrolls the mouse wheel on the canvas. */
  zoom (bigger: boolean) {
    if (this.wheelLimiter) return
    this.report.zoom(bigger)
  }

  /* The market handler will call unattach when the markets page is unloaded. */
  unattach () {
    for (const u of this.unattachers) u()
    this.unattachers = []
  }

  /* render must be implemented by the child class. */
  render () {
    console.error('child class must override render method')
  }

  /* applyLabelStyle applies the style used for axis tick labels. */
  applyLabelStyle (fontSize?: number) {
    this.ctx.textAlign = 'center'
    this.ctx.textBaseline = 'middle'
    this.ctx.font = `${fontSize ?? '14'}px 'sans', sans-serif`
    this.ctx.fillStyle = this.theme.axisLabel
  }

  /* plotXLabels applies the provided labels to the x axis and draws the grid. */
  plotXLabels (labels: LabelSet, minX: number, maxX: number, unitLines: string[]) {
    const extents = new Extents(minX, maxX, 0, 1)
    this.xRegion.plot(extents, (ctx: CanvasRenderingContext2D, tools: Translator) => {
      this.applyLabelStyle()
      const centerX = (maxX + minX) / 2
      let lastX = minX
      let unitCenter = centerX
      const [leftEdge, rightEdge] = [tools.x(minX), tools.x(maxX)]
      const centerY = tools.y(0.5)
      labels.lbls.forEach(lbl => {
        const m = ctx.measureText(lbl.txt)
        const x = tools.x(lbl.val)
        if (x - m.width / 2 < leftEdge || x + m.width / 2 > rightEdge) return
        ctx.fillText(lbl.txt, x, centerY)
        if (centerX >= lastX && centerX < lbl.val) {
          unitCenter = (lastX + lbl.val) / 2
        }
        lastX = lbl.val
      })
      ctx.font = '11px \'sans\', sans-serif'
      if (unitLines.length === 2) {
        ctx.fillText(unitLines[0], tools.x(unitCenter), tools.y(0.63))
        ctx.fillText(unitLines[1], tools.x(unitCenter), tools.y(0.23))
      } else if (unitLines.length === 1) {
        ctx.fillText(unitLines[0], tools.x(unitCenter), centerY)
      }
    }, true)
  }

  plotXGrid (labels: LabelSet, minX: number, maxX: number) {
    const extents = new Extents(minX, maxX, 0, 1)
    this.plotRegion.plot(extents, (ctx: CanvasRenderingContext2D, tools: Translator) => {
      ctx.lineWidth = 1
      ctx.strokeStyle = this.theme.gridLines
      labels.lbls.forEach(lbl => {
        line(ctx, tools.x(lbl.val), tools.y(0), tools.x(lbl.val), tools.y(1))
      })
    }, true)
  }

  /*
   * plotYLabels applies the y labels based on the provided plot region, and
   * draws the grid.
   */
  plotYLabels (labels: LabelSet, minY: number, maxY: number, unit: string) {
    const extents = new Extents(0, 1, minY, maxY)

    const fillRect = (ctx: CanvasRenderingContext2D, x: number, y: number, w: number, h: number, r: number) => {
      ctx.save()
      ctx.fillStyle = this.theme.body
      ctx.beginPath()
      if (ctx.roundRect) ctx.roundRect(x, y, w, h, r) // Safari < 16 doesn't support
      else ctx.rect(x, y, w, h)
      ctx.fill()
      ctx.restore()
    }

    this.yRegion.plot(extents, (ctx: CanvasRenderingContext2D, tools: Translator) => {
      this.applyLabelStyle()
      this.ctx.textAlign = 'left'
      const centerY = maxY / 2
      let lastY = 0
      let unitCenter = centerY
      const x = tools.x(0)
      const [xPad, yPad] = [3, 3]
      labels.lbls.forEach(lbl => {
        const y = tools.y(lbl.val)
        if (y < tools.y(maxY) + yPad + 7 || y > tools.y(minY) - yPad - 7) return
        const m = ctx.measureText(lbl.txt)
        fillRect(ctx, x, y - 7 - yPad, m.width + xPad * 2, 14 + yPad * 3, 3)
        ctx.fillText(lbl.txt, x + xPad, y + 2)
        if (centerY >= lastY && centerY < lbl.val) {
          unitCenter = (lastY + lbl.val) / 2
        }
        lastY = lbl.val
      })
      const m = ctx.measureText(unit)
      const y = tools.y(unitCenter)
      fillRect(ctx, x, y - yPad - 7, m.width + xPad * 2, 14 + yPad * 2, 3)
      ctx.fillText(unit, x + xPad, tools.y(unitCenter))
    }, true)
  }

  plotYGrid (region: Region, labels: LabelSet, minY: number, maxY: number) {
    const extents = new Extents(0, 1, minY, maxY)
    region.plot(extents, (ctx: CanvasRenderingContext2D, tools: Translator) => {
      ctx.lineWidth = 1
      ctx.strokeStyle = this.theme.gridLines
      labels.lbls.forEach(lbl => {
        line(ctx, tools.x(0), tools.y(lbl.val), tools.x(1), tools.y(lbl.val))
      })
    }, true)
  }

  /*
   * doYLabels generates and applies the y-axis labels, based upon the
   * provided plot region.
   */
  makeYLabels (region: Region, step: number, unit: string, valFmt?: (v: number) => string): LabelSet {
    this.applyLabelStyle()
    const yLabels = makeLabels(this.ctx, region.height(), this.dataExtents.y.min,
      this.dataExtents.y.max, 50, step, unit, valFmt)

    // Reassign the width of the y-label column to accommodate the widest text.
    const yAxisWidth = (yLabels.widest || 0) + 20 /* x padding */
    this.yRegion.extents.x.max = yAxisWidth
    this.yRegion.extents.y.max = region.extents.y.max

    return yLabels
  }

  line (x0: number, y0: number, x1: number, y1: number, skipStroke?: boolean) {
    line(this.ctx, x0, y0, x1, y1, skipStroke)
  }

  /* dot draws a circle with the provided context. */
  dot (x: number, y: number, color: string, radius: number) {
    dot(this.ctx, x, y, color, radius)
  }
}

/* DepthChart is a javascript Canvas-based depth chart renderer. */
export class DepthChart extends Chart {
  reporters: DepthReporters
  book: OrderBook
  zoomLevel: number
  lotSize: number
  conventionalRateStep: number
  lines: DepthLine[]
  markers: Record<string, DepthMarker[]>
  zoomInBttn: Region
  zoomOutBttn: Region
  baseUnit: string
  quoteUnit: string

  constructor (parent: HTMLElement, reporters: DepthReporters, zoom: number) {
    super(parent, {
      resize: () => this.resized(),
      click: (e: MouseEvent) => this.clicked(e),
      zoom: (bigger: boolean) => this.zoomed(bigger)
    })
    this.reporters = reporters
    this.zoomLevel = zoom
    this.lines = []
    this.markers = {
      buys: [],
      sells: []
    }
    this.setZoomBttns() // can't wait for requestAnimationFrame -> resized
    this.resize()
  }

  // setZoomBttns creates new regions for zoom in and zoom out buttons. It is
  // used in initiation of the buttons and resizing.
  setZoomBttns () {
    this.zoomInBttn = new Region(this.ctx, new Extents(0, 0, 0, 0))
    this.zoomOutBttn = new Region(this.ctx, new Extents(0, 0, 0, 0))
  }

  /* resized is called when the window or parent element are resized. */
  resized () {
    // The button region extents are set during drawing.
    this.setZoomBttns()
    if (this.book) this.draw()
  }

  /* zoomed zooms the current view in or out. bigger=true is zoom in. */
  zoomed (bigger: boolean) {
    if (!this.zoomLevel) return
    if (!this.book.buys || !this.book.sells) return
    this.wheeled()
    // Zoom in to 66%, but out to 150% = 1 / (2/3) so that the same zoom levels
    // are hit when reversing direction.
    this.zoomLevel *= bigger ? 2 / 3 : 3 / 2
    this.zoomLevel = clamp(this.zoomLevel, 0.005, 2)
    this.draw()
    this.reporters.zoom(this.zoomLevel)
  }

  /* clicked is the canvas 'click' event handler. */
  clicked (e: MouseEvent) {
    if (!this.dataExtents) return
    const x = e.clientX - this.rect.left
    const y = e.clientY - this.rect.y
    if (this.zoomInBttn.contains(x, y)) { this.zoom(true); return }
    if (this.zoomOutBttn.contains(x, y)) { this.zoom(false); return }
    const translator = this.plotRegion.translator(this.dataExtents)
    this.reporters.click(translator.unx(x))
  }

  // set sets the current data set and draws.
  set (book: OrderBook, lotSize: number, rateStepEnc: number, baseUnitInfo: UnitInfo, quoteUnitInfo: UnitInfo) {
    this.book = book
    this.lotSize = lotSize / baseUnitInfo.conventional.conversionFactor
    this.conventionalRateStep = Doc.conventionalRateStep(rateStepEnc, baseUnitInfo, quoteUnitInfo)
    this.baseUnit = baseUnitInfo.conventional.unit
    this.quoteUnit = quoteUnitInfo.conventional.unit
    const [midGap, gapWidth] = this.gap()
    const spread = gapWidth / midGap
    if (!this.zoomLevel) {
      // Default to 5% zoom, but with a minimum of 5 * spread, but still
      // observing the hard cap of 200%.
      const minZoom = Math.max(spread * 5, 0.05)
      this.zoomLevel = Math.min(minZoom, 2)
    } else if (this.zoomLevel < spread * 2) {
      // The saved zoom level is shared by all markets, so it may be too narrow
      // to show anything but the gap on a market with a wider spread. Zoom out
      // so that the best buy and sell are inside the middle half of the chart.
      // This is not saved, so the user's zoom is kept for other markets.
      this.zoomLevel = Math.min(spread * 2, 2)
    }
    this.draw()
  }

  /*
   * render draws the chart.
   * 1. Calculate the data extents and translate the order book data to a
   *    cumulative form.
   * 2. Draw axis ticks and grid, mid-gap line and value, zoom buttons, mouse
   *    position indicator...
   * 4. Tick labels.
   * 5. Data.
   * 6. Epoch line legend.
   * 7. Hover legend.
   */
  render () {
    // if connection fails it is not possible to get book.
    if (!this.book || !this.visible || this.canvas.width === 0) {
      this.renderScheduled = true
      return
    }

    this.clear()
    // if (!this.book || this.book.empty()) return
    const ctx = this.ctx
    const mousePos = this.mousePos
    const buys = this.book.buys
    const sells = this.book.sells

    const [midGap, gapWidth] = this.gap()

    const halfWindow = this.zoomLevel * midGap / 2
    const high = midGap + halfWindow
    const low = midGap - halfWindow

    // Get a sorted copy of the markers list.
    const buyMarkers = [...this.markers.buys]
    const sellMarkers = [...this.markers.sells]
    buyMarkers.sort((a, b) => b.rate - a.rate)
    sellMarkers.sort((a, b) => a.rate - b.rate)
    const markers: DepthMark[] = []

    const buyDepth: [number, number][] = []
    const buyEpoch: [number, number][] = []
    const sellDepth: [number, number][] = []
    const sellEpoch: [number, number][] = []
    const volumeReport = {
      buyBase: 0,
      buyQuote: 0,
      sellBase: 0,
      sellQuote: 0
    }
    let sum = 0
    // The epoch line is above the non-epoch region, so the epochSum y value
    // must account for non-epoch orders too.
    let epochSum = 0

    for (let i = 0; i < buys.length; i++) {
      const ord = buys[i]
      epochSum += ord.qty
      if (ord.rate >= low) buyEpoch.push([ord.rate, epochSum])
      if (ord.epoch) continue
      sum += ord.qty
      buyDepth.push([ord.rate, sum])
      volumeReport.buyBase += ord.qty
      volumeReport.buyQuote += ord.qty * ord.rate
      while (buyMarkers.length && floatCompare(buyMarkers[0].rate, ord.rate)) {
        const mark = buyMarkers.shift()
        if (!mark) continue
        markers.push({
          rate: mark.rate,
          qty: ord.epoch ? epochSum : sum,
          sell: ord.sell,
          active: mark.active
        })
      }
    }
    const buySum = buyDepth.length ? last(buyDepth)[1] : 0
    buyDepth.push([low, buySum])
    const epochBuySum = buyEpoch.length ? last(buyEpoch)[1] : 0
    buyEpoch.push([low, epochBuySum])

    epochSum = sum = 0
    for (let i = 0; i < sells.length; i++) {
      const ord = sells[i]
      epochSum += ord.qty
      if (ord.rate <= high) sellEpoch.push([ord.rate, epochSum])
      if (ord.epoch) continue
      sum += ord.qty
      sellDepth.push([ord.rate, sum])
      volumeReport.sellBase += ord.qty
      volumeReport.sellQuote += ord.qty * ord.rate
      while (sellMarkers.length && floatCompare(sellMarkers[0].rate, ord.rate)) {
        const mark = sellMarkers.shift()
        if (!mark) continue
        markers.push({
          rate: mark.rate,
          qty: ord.epoch ? epochSum : sum,
          sell: ord.sell,
          active: mark.active
        })
      }
    }
    // Add a data point going to the left so that the data doesn't end with a
    // vertical line.
    const sellSum = sellDepth.length ? last(sellDepth)[1] : 0
    sellDepth.push([high, sellSum])
    const epochSellSum = sellEpoch.length ? last(sellEpoch)[1] : 0
    sellEpoch.push([high, epochSellSum])

    // Add ~30px padding to the top of the chart.
    const h = this.xRegion.extents.y.min
    const growthFactor = (h + 40) / h
    const maxY = (epochSellSum && epochBuySum ? Math.max(epochBuySum, epochSellSum) : epochSellSum || epochBuySum || 1) * growthFactor

    const dataExtents = new Extents(low, high, 0, maxY)
    this.dataExtents = dataExtents

    // A function to be run at the end if there is legend data to display.
    let mouseData: MouseReport | null = null

    // Draw the grid.
    const xLabels = makeLabels(ctx, this.plotRegion.width(), dataExtents.x.min, dataExtents.x.max, 100, this.conventionalRateStep, '')
    this.plotXGrid(xLabels, low, high)
    const yLabels = this.makeYLabels(this.plotRegion, this.lotSize, this.baseUnit)
    this.plotYGrid(this.plotRegion, yLabels, this.dataExtents.y.min, this.dataExtents.y.max)

    this.plotRegion.plot(dataExtents, (ctx, tools) => {
      ctx.lineWidth = 1
      // first, a square around the plot area.
      ctx.strokeStyle = this.theme.gridBorder
      // draw a line to indicate mid-gap
      ctx.lineWidth = 2.5
      ctx.strokeStyle = this.theme.gapLine
      line(ctx, tools.x(midGap), tools.y(0), tools.x(midGap), tools.y(0.3 * dataExtents.y.max))

      ctx.font = '30px \'demi-sans\', sans-serif'
      ctx.textAlign = 'center'
      ctx.textBaseline = 'middle'
      ctx.fillStyle = this.theme.value
      const y = 0.5 * dataExtents.y.max
      ctx.fillText(Doc.formatFourSigFigs(midGap), tools.x(midGap), tools.y(y))
      ctx.font = '12px \'sans\', sans-serif'
      // ctx.fillText('mid-market price', tools.x(midGap), tools.y(y) + 24)
      ctx.fillText(`${(gapWidth / midGap * 100).toFixed(2)}% spread`,
        tools.x(midGap), tools.y(y) + 24)

      // Draw zoom buttons.
      ctx.textAlign = 'center'
      ctx.textBaseline = 'middle'
      const topCenterX = this.plotRegion.extents.midX
      const topCenterY = tools.y(maxY * 0.9)
      const zoomPct = dataExtents.xRange / midGap * 100
      const zoomText = `${zoomPct.toFixed(1)}%`
      const w = ctx.measureText(zoomText).width
      ctx.font = '13px \'sans\', sans-serif'
      ctx.fillText(zoomText, topCenterX, topCenterY + 1)
      // define the region for the zoom button
      const bttnSize = 20
      const xPad = 10
      let bttnLeft = topCenterX - w / 2 - xPad - bttnSize
      const bttnTop = topCenterY - bttnSize / 2
      this.zoomOutBttn.setExtents(
        bttnLeft,
        bttnLeft + bttnSize,
        bttnTop,
        bttnTop + bttnSize
      )
      let hover = mousePos && this.zoomOutBttn.contains(mousePos.x, mousePos.y)
      this.zoomOutBttn.plot(new Extents(0, 1, 0, 1), ctx => {
        ctx.font = '12px \'icomoon\''
        ctx.fillStyle = this.theme.zoom
        if (hover) {
          ctx.fillStyle = this.theme.zoomHover
          ctx.font = '13px \'icomoon\''
        }
        ctx.fillText(minusChar, this.zoomOutBttn.extents.midX, this.zoomOutBttn.extents.midY)
      })
      bttnLeft = topCenterX + w / 2 + xPad
      this.zoomInBttn.setExtents(
        bttnLeft,
        bttnLeft + bttnSize,
        bttnTop,
        bttnTop + bttnSize
      )
      hover = mousePos && this.zoomInBttn.contains(mousePos.x, mousePos.y)
      this.zoomInBttn.plot(new Extents(0, 1, 0, 1), ctx => {
        ctx.font = '12px \'icomoon\''
        ctx.fillStyle = this.theme.zoom
        if (hover) {
          ctx.fillStyle = this.theme.zoomHover
          ctx.font = '14px \'icomoon\''
        }
        ctx.fillText(plusChar, this.zoomInBttn.extents.midX, this.zoomInBttn.extents.midY)
      })

      // Draw a dotted vertical line where the mouse is, and a dot at the level
      // of the depth line.
      const drawLine = (x: number, color: string) => {
        if (x > high || x < low) return
        ctx.save()
        ctx.setLineDash([3, 5])
        ctx.lineWidth = 1.5
        ctx.strokeStyle = color
        line(ctx, tools.x(x), tools.y(0), tools.x(x), tools.y(maxY))
        ctx.restore()
      }

      for (const line of this.lines || []) {
        drawLine(line.rate, line.color)
      }

      const tolerance = (high - low) * 0.005
      const hoverMarkers = []
      for (const marker of markers || []) {
        const hovered = (mousePos && withinTolerance(marker.rate, tools.unx(mousePos.x), tolerance))
        if (hovered) hoverMarkers.push(marker.rate)
        ctx.save()
        ctx.lineWidth = (hovered || marker.active) ? 5 : 3
        ctx.strokeStyle = marker.sell ? this.theme.sellLine : this.theme.buyLine
        ctx.fillStyle = marker.sell ? this.theme.sellFill : this.theme.buyFill
        const size = (hovered || marker.active) ? 10 : 8
        ctx.beginPath()
        const tip = {
          x: tools.x(marker.rate),
          y: tools.y(marker.qty) - 8
        }
        const top = tip.y - (Math.sqrt(3) * size / 2) // cos(30)
        ctx.moveTo(tip.x, tip.y)
        ctx.lineTo(tip.x - size / 2, top)
        ctx.lineTo(tip.x + size / 2, top)
        ctx.closePath()
        ctx.stroke()
        ctx.fill()
        ctx.restore()
      }

      // If the mouse is in the chart area, draw the crosshairs.
      if (!mousePos) return
      if (!this.plotRegion.contains(mousePos.x, mousePos.y)) return
      // The mouse is in the plot region. Get the data coordinates and find the
      // side and depth for the x value.
      const dataX = tools.unx(mousePos.x)
      let evalSide = sellDepth
      let trigger = (ptX: number) => ptX >= dataX
      let dotColor = this.theme.sellLine
      if (dataX < midGap) {
        evalSide = buyDepth
        trigger = (ptX) => ptX <= dataX
        dotColor = this.theme.buyLine
      }
      let bestDepth = evalSide[0]
      for (let i = 0; i < evalSide.length; i++) {
        const pt = evalSide[i]
        if (trigger(pt[0])) break
        bestDepth = pt
      }
      drawLine(dataX, this.theme.crosshairs)
      mouseData = {
        rate: dataX,
        depth: bestDepth[1],
        dotColor: dotColor,
        hoverMarkers: hoverMarkers
      }
    })

    // Draw the epoch lines
    ctx.lineWidth = 1.5
    ctx.setLineDash([3, 3])
    // epoch sells
    ctx.fillStyle = this.theme.sellFill
    ctx.strokeStyle = this.theme.sellLine
    this.drawDepth(sellEpoch)
    // epoch buys
    ctx.fillStyle = this.theme.buyFill
    ctx.strokeStyle = this.theme.buyLine
    this.drawDepth(buyEpoch)

    // Draw the book depth.
    ctx.lineWidth = 2.5
    ctx.setLineDash([])
    // book sells
    ctx.fillStyle = this.theme.sellFill
    ctx.strokeStyle = this.theme.sellLine
    this.drawDepth(sellDepth)
    // book buys
    ctx.fillStyle = this.theme.buyFill
    ctx.strokeStyle = this.theme.buyLine
    this.drawDepth(buyDepth)

    this.plotYLabels(yLabels, this.dataExtents.y.min, this.dataExtents.y.max, this.baseUnit)
    this.plotXLabels(xLabels, low, high, [`${this.quoteUnit}/`, this.baseUnit])

    // Display the dot at the intersection of the mouse hover line and the depth
    // line. This should be drawn after the depths.
    if (mouseData) {
      this.plotRegion.plot(dataExtents, (ctx, tools) => {
        if (!mouseData) return // For TypeScript. Duh.
        dot(ctx, tools.x(mouseData.rate), tools.y(mouseData.depth), mouseData.dotColor, 5)
      })
    }

    // Report the book volumes.
    this.reporters.volume(volumeReport)
    this.reporters.mouse(mouseData)
  }

  /* drawDepth draws a single side's depth chart data. */
  drawDepth (depth: [number, number][]) {
    const firstPt = depth[0]
    let x: number
    this.plotRegion.plot(this.dataExtents, (ctx, tools) => {
      const yZero = tools.y(0)
      let y = tools.y(firstPt[1])
      ctx.beginPath()
      ctx.moveTo(tools.x(firstPt[0]), tools.y(firstPt[1]))
      for (let i = 0; i < depth.length; i++) {
        // Set x, but don't set y until we draw the horizontal line.
        x = tools.x(depth[i][0])
        ctx.lineTo(x, y)
        // If this is past the render edge, quit drawing.
        y = tools.y(depth[i][1])
        ctx.lineTo(x, y)
      }
      ctx.stroke()
      ctx.lineTo(x, yZero)
      ctx.lineTo(tools.x(firstPt[0]), yZero)
      ctx.closePath()
      ctx.globalAlpha = 0.25
      ctx.fill()
    })
  }

  /* returns the mid-gap rate and gap width as a tuple. */
  gap () {
    const [b, s] = [this.book.bestGapBuy(), this.book.bestGapSell()]
    if (!b) {
      if (!s) return [1, 0]
      return [s.rate, 0]
    } else if (!s) return [b.rate, 0]
    return [(s.rate + b.rate) / 2, s.rate - b.rate]
  }

  /* setLines stores the indicator lines to draw. */
  setLines (lines: DepthLine[]) {
    this.lines = lines
  }

  /* setMarkers sets the indicator markers to draw. */
  setMarkers (markers: Record<string, DepthMarker[]>) {
    this.markers = markers
  }
}

/*
 * PriceChart is a lightweight, dependency-free exchange-style price chart:
 * candles/line/area, volume, MA/EMA overlays, auto/manual/log/% price scale,
 * crosshair with an OHLC readout, adaptive local-time axis, last-price line
 * with a candle-close countdown, mouse/touch zoom and pan, and optional
 * collapsing of periods with no trades ("no gaps").
 */

const DAY = 864e5
const HOUR = 36e5
const WEEK = 7 * DAY
const MONTH = 30 * DAY // nominal; 1M bins follow calendar months (UTC)

export interface ChartTF {
  ms: number
  src: string // the server bin size the timeframe is built from
}

/* chartTFs are the chart timeframes. Native server bins are 5m, 1h and 24h. */
export const chartTFs: Record<string, ChartTF> = {
  '5m': { ms: 3e5, src: '5m' },
  '15m': { ms: 9e5, src: '5m' },
  '30m': { ms: 18e5, src: '5m' },
  '1h': { ms: HOUR, src: '1h' },
  '4h': { ms: 4 * HOUR, src: '1h' },
  '1D': { ms: DAY, src: '24h' },
  '3D': { ms: 3 * DAY, src: '24h' },
  '1W': { ms: WEEK, src: '24h' },
  '1M': { ms: MONTH, src: '24h' }
}

export interface PCandle {
  t: number // bin start, unix ms
  o: number
  h: number
  l: number
  c: number
  v: number // base volume, atoms
  tr: boolean // had trades
}

export interface PriceChartSettings {
  type: string // candle, line, area
  scale: string // lin, log, pct
  auto: boolean
  vol: boolean
  ma: Record<string, boolean>
  ema: boolean
  skip: boolean // collapse periods without trades
}

interface Layout {
  pw: number
  ph: number
  cw: number
  top: number
  bot: number
  tlo: number
  thi: number
}

interface PTheme {
  grid: string
  txt: string
  hi: string
  axis: string
  up: string
  dn: string
  cross: string
  tag: string
  tagTxt: string
  line: string
}

const pcDark: PTheme = { grid: '#14283c', txt: '#8a9bb0', hi: '#c5d0dc', axis: '#23344a', up: '#2ebd85', dn: '#e04f5f', cross: '#6b7d93', tag: '#2a3b52', tagTxt: '#e6edf5', line: '#2997ff' }
const pcLight: PTheme = { grid: '#e7eaee', txt: '#5d6b7a', hi: '#1b1b1b', axis: '#ccd3db', up: '#1f8a4c', dn: '#c0392b', cross: '#8a96a3', tag: '#dde3ea', tagTxt: '#1b1b1b', line: '#1e6fd9' }
const MAS: [number, string][] = [[7, '#f0b90b'], [25, '#e377c2'], [99, '#9d7cf4']]
const EMA_N = 20
const EMA_COLOR = '#4fc3f7'
const FONT = '11px sans-serif'

/*
 * normalizeCandles converts server candles to chart candles keyed by bin
 * start. Bins without trades get the previous close. Duplicate bins (e.g. the
 * overlap of older history and the live feed) keep the later one.
 *
 * The server assigns each epoch to the bin its END stamp falls in, and a
 * candle's startStamp is the start of its first epoch, which is a few seconds
 * BEFORE the bin boundary (e.g. 10:59:55 for the 11:00 1h bin). So the bin is
 * taken from endStamp; using startStamp put every candle one bin too early.
 */
export function normalizeCandles (cs: Candle[], ms: number): PCandle[] {
  const out: PCandle[] = []
  let pc = 0
  for (const c of cs) {
    const k: PCandle = { t: Math.floor((c.endStamp || c.startStamp) / ms) * ms, o: c.startRate, h: c.highRate, l: c.lowRate, c: c.endRate, v: c.matchVolume, tr: c.matchVolume > 0 }
    if (!k.h) k.o = k.h = k.l = k.c = pc
    else if (!k.o) k.o = pc || k.l
    if (!k.c) continue
    const prev = out[out.length - 1]
    if (prev && prev.t >= k.t) {
      if (prev.t === k.t) out[out.length - 1] = k
      continue
    }
    pc = k.c
    out.push(k)
  }
  return out
}

/*
 * fillGaps adds flat, trade-less candles for bins missing from the data (e.g.
 * while the server was down), so the time axis of fixed-size timeframes is
 * continuous. Calendar months are left as they are. Very long gaps are not
 * filled, and the total number of added candles is capped.
 */
export function fillGaps (cs: PCandle[], ms: number): PCandle[] {
  if (ms === MONTH || cs.length < 2) return cs
  const out: PCandle[] = []
  let added = 0
  for (const c of cs) {
    const prev = out[out.length - 1]
    if (prev) {
      const missing = Math.round((c.t - prev.t) / ms) - 1
      if (missing > 0 && missing <= 5000 && added + missing <= 20000) {
        for (let i = 1; i <= missing; i++) out.push({ t: prev.t + i * ms, o: prev.c, h: prev.c, l: prev.c, c: prev.c, v: 0, tr: false })
        added += missing
      }
    }
    out.push(c)
  }
  return out
}

function binKey (t: number, ms: number): number {
  if (ms === MONTH) {
    const d = new Date(t)
    return d.getUTCFullYear() * 12 + d.getUTCMonth()
  }
  if (ms === WEEK) return Math.floor((t / DAY + 3) / 7) // weeks start Monday
  return Math.floor(t / ms)
}

function binStart (k: number, ms: number): number {
  if (ms === MONTH) return Date.UTC(Math.floor(k / 12), k % 12, 1)
  if (ms === WEEK) return (k * 7 - 3) * DAY
  return k * ms
}

/* aggregateCandles builds coarser bins. OHLC come from bins with trades. */
export function aggregateCandles (cs: PCandle[], ms: number): PCandle[] {
  const out: PCandle[] = []
  let cur: PCandle | null = null
  let ck = 0
  for (const c of cs) {
    const k = binKey(c.t, ms)
    if (!cur || k !== ck) {
      ck = k
      cur = { t: binStart(k, ms), o: c.o, h: c.h, l: c.l, c: c.c, v: 0, tr: false }
      out.push(cur)
    }
    if (c.tr) {
      if (!cur.tr) [cur.o, cur.h, cur.l] = [c.o, c.h, c.l]
      else [cur.h, cur.l] = [Math.max(cur.h, c.h), Math.min(cur.l, c.l)]
      cur.tr = true
      cur.v += c.v
    }
    cur.c = c.c
  }
  return out
}

export class PriceChart {
  parent: HTMLElement
  canvas: HTMLCanvasElement
  ctx: CanvasRenderingContext2D
  s: PriceChartSettings
  rf: number // message rate -> conventional
  vf: number // base atoms -> conventional
  all: PCandle[]
  view: PCandle[]
  ind: [string, string, number[]][]
  ms: number
  n: number // candles across the plot
  off: number // candles from the right edge
  ylo: number
  yhi: number
  w: number
  h: number
  L: Layout | null
  m: { x: number, y: number } | null
  drag: { x: number, y: number, off: number, n: number, lo: number, hi: number, zone: string } | null
  pinch: { d: number, n: number } | null
  ptr: Map<number, { x: number, y: number }>
  raf: number
  ticker: number
  ro: ResizeObserver
  onChange: (s: PriceChartSettings) => void
  onOlder: () => void // the left edge is near the oldest candle

  constructor (parent: HTMLElement, s: PriceChartSettings) {
    this.parent = parent
    this.s = s
    this.rf = this.vf = 1e8
    this.all = []
    this.view = []
    this.ind = []
    this.ms = HOUR
    this.n = 80
    this.off = -3
    this.L = this.m = this.drag = this.pinch = null
    this.ptr = new Map()
    this.onChange = this.onOlder = () => { /* set by owner */ }
    const c = this.canvas = document.createElement('canvas')
    c.tabIndex = 0
    c.className = 'price-chart'
    parent.appendChild(c)
    this.ctx = c.getContext('2d') as CanvasRenderingContext2D
    this.ro = new ResizeObserver(() => this.resize())
    this.ro.observe(parent)
    // Redraw once a second for the candle-close countdown.
    this.ticker = window.setInterval(() => { if (this.view.length && !document.hidden) this.req() }, 1000)
    const pos = (e: MouseEvent) => {
      const r = c.getBoundingClientRect()
      return { x: e.clientX - r.left, y: e.clientY - r.top }
    }
    const pts = () => Array.from(this.ptr.values())
    c.addEventListener('pointerdown', (e: PointerEvent) => {
      c.setPointerCapture(e.pointerId)
      const p = pos(e)
      this.ptr.set(e.pointerId, p)
      if (this.ptr.size === 2) {
        const [a, b] = pts()
        this.pinch = { d: Math.hypot(a.x - b.x, a.y - b.y), n: this.n }
        this.drag = null
        return
      }
      const L = this.L
      if (!L) return
      this.drag = { ...p, off: this.off, n: this.n, lo: L.tlo, hi: L.thi, zone: p.x > L.pw ? 'y' : p.y > L.ph ? 'x' : 'pan' }
    })
    c.addEventListener('pointermove', (e: PointerEvent) => {
      const p = pos(e)
      this.m = p
      if (this.ptr.has(e.pointerId)) this.ptr.set(e.pointerId, p)
      const { drag: d, L } = this
      if (this.pinch && this.ptr.size === 2) {
        const [a, b] = pts()
        this.zoomTo(this.pinch.n * this.pinch.d / Math.max(10, Math.hypot(a.x - b.x, a.y - b.y)), (a.x + b.x) / 2)
      } else if (d && L) {
        const [dx, dy] = [p.x - d.x, p.y - d.y]
        if (d.zone === 'y') { // stretch the price axis, turns Auto off
          const mid = (d.lo + d.hi) / 2
          const k = Math.exp(dy * 0.006)
          this.ylo = mid - (mid - d.lo) * k
          this.yhi = mid + (d.hi - mid) * k
          if (this.s.auto) this.set({ auto: false })
        } else if (d.zone === 'x') {
          this.n = this.clampN(d.n * Math.exp(-dx * 0.006))
          this.off = this.clampOff(this.off)
        } else {
          this.off = this.clampOff(d.off + dx / L.cw)
          if (!this.s.auto) {
            const r = (d.hi - d.lo) * dy / (L.bot - L.top)
            this.ylo = d.lo + r
            this.yhi = d.hi + r
          }
        }
      }
      c.style.cursor = d ? 'grabbing' : L && p.x > L.pw ? 'ns-resize' : L && p.y > L.ph ? 'ew-resize' : 'crosshair'
      this.req()
    })
    const up = (e: PointerEvent) => {
      this.ptr.delete(e.pointerId)
      if (this.ptr.size < 2) this.pinch = null
      this.drag = null
    }
    c.addEventListener('pointerup', up)
    c.addEventListener('pointercancel', up)
    c.addEventListener('pointerleave', () => {
      if (this.drag) return
      this.m = null
      this.req()
    })
    c.addEventListener('wheel', (e: WheelEvent) => {
      e.preventDefault()
      this.zoomTo(this.n * Math.exp(e.deltaY * 0.0015), pos(e).x)
    }, { passive: false })
    c.addEventListener('dblclick', (e: MouseEvent) => {
      if (this.L && pos(e).x > this.L.pw) this.set({ auto: true })
      else this.reset()
    })
    c.addEventListener('keydown', (e: KeyboardEvent) => {
      const step = Math.max(1, this.n / 10)
      switch (e.key) {
        case 'ArrowLeft': this.off = this.clampOff(this.off + step); break
        case 'ArrowRight': this.off = this.clampOff(this.off - step); break
        case '+': case '=': this.zoomTo(this.n / 1.25); break
        case '-': this.zoomTo(this.n * 1.25); break
        default: return
      }
      e.preventDefault()
      this.req()
    })
  }

  /* set updates the settings and redraws. */
  set (patch: Partial<PriceChartSettings>) {
    Object.assign(this.s, patch)
    if ('skip' in patch || 'ma' in patch || 'ema' in patch) this.filter()
    if ('skip' in patch) {
      this.n = this.clampN(Math.min(this.n, this.view.length + 3))
      this.off = -3
      this.s.auto = true
    }
    if ('scale' in patch) this.s.auto = true
    this.req()
    this.onChange(this.s)
  }

  /*
   * setData sets the candles. With keepView, the view stays where it is
   * relative to the newest candle, for live updates and prepended history.
   */
  setData (cs: PCandle[], ms: number, rf: number, vf: number, keepView?: boolean) {
    [this.all, this.ms, this.rf, this.vf] = [cs, ms, rf, vf]
    this.filter()
    if (keepView) this.req()
    else this.reset()
  }

  filter () {
    const all = this.all
    const v = this.view = this.skipping() ? all.filter((c, i) => c.tr || i === all.length - 1) : all
    this.ind = []
    for (const [p, col] of MAS) {
      if (!this.s.ma[p]) continue
      const a: number[] = []
      let sum = 0
      v.forEach((c, i) => {
        sum += c.c
        if (i >= p) sum -= v[i - p].c
        a.push(i >= p - 1 ? sum / p : NaN)
      })
      this.ind.push(['MA' + p, col, a])
    }
    if (this.s.ema) {
      const a: number[] = []
      const k = 2 / (EMA_N + 1)
      let e = 0
      v.forEach((c, i) => {
        e = i ? c.c * k + e * (1 - k) : c.c
        a.push(i >= EMA_N - 1 ? e : NaN)
      })
      this.ind.push(['EMA' + EMA_N, EMA_COLOR, a])
    }
  }

  /*
   * skipping is whether periods without trades are collapsed. That is only
   * done for 1D and longer timeframes. On a thin market most intraday bins
   * have no trades, so collapsing them would put candles that are hours or
   * days apart next to each other, and a 5m chart would span weeks.
   */
  skipping (): boolean { return this.s.skip && this.ms >= DAY }

  clampN (n: number) { return Math.min(Math.max(n, 8), Math.max(this.view.length * 1.2, 8), 4000) }
  clampOff (v: number) { return Math.min(Math.max(v, -this.n * 0.5), Math.max(this.view.length - this.n * 0.2, 0)) }

  /* zoomTo zooms to n candles, keeping the candle at x fixed. */
  zoomTo (n: number, x?: number) {
    const L = this.L
    if (!L) return
    const mx = x ?? L.pw / 2
    const len = this.view.length
    const idx = len - this.off - (L.pw - mx) / L.cw
    this.n = this.clampN(n)
    this.off = this.clampOff(len - idx - (L.pw - mx) / (L.pw / this.n))
    this.req()
  }

  reset () {
    this.n = this.clampN(Math.min(this.parent.clientWidth < 500 ? 40 : 80, this.view.length + 3))
    this.off = -3
    this.set({ auto: true })
  }

  fit () {
    this.n = this.clampN(this.view.length + 4)
    this.off = -3
    this.set({ auto: true })
  }

  /* setRange shows the last dur ms. It is false if there's not enough history. */
  setRange (dur: number): boolean {
    const v = this.view
    if (!v.length) return false
    const from = this.all[this.all.length - 1].t - dur + this.ms
    let i = v.length - 1
    while (i > 0 && v[i - 1].t >= from) i--
    this.n = this.clampN(v.length - i + 3)
    this.off = -3
    this.set({ auto: true })
    return this.all[0].t <= from
  }

  req () {
    if (this.raf) return
    this.raf = requestAnimationFrame(() => {
      this.raf = 0
      this.draw()
    })
  }

  resize () {
    const d = window.devicePixelRatio || 1
    const [w, h] = [this.parent.clientWidth, this.parent.clientHeight]
    this.canvas.width = Math.round(w * d)
    this.canvas.height = Math.round(h * d)
    this.ctx.setTransform(d, 0, 0, d, 0, 0)
    this.w = w
    this.h = h
    this.req()
  }

  unattach () {
    this.ro.disconnect()
    clearInterval(this.ticker)
  }

  draw () {
    const { ctx, s, w, h, view } = this
    if (!w || !h) return
    ctx.clearRect(0, 0, w, h)
    if (!view.length) return
    const o = State.isDark() ? pcDark : pcLight
    ctx.font = FONT
    ctx.textBaseline = 'middle'
    ctx.lineWidth = 1
    const axW = w < 500 ? 66 : 78
    const axH = 22
    const [pw, ph] = [w - axW, h - axH]
    const cw = pw / this.n
    const end = view.length - this.off
    const i0 = Math.max(0, Math.floor(end - this.n) - 1)
    const i1 = Math.min(view.length, Math.ceil(end) + 1)
    if (i0 < Math.max(5, this.n * 0.3)) this.onOlder()
    const xOf = (i: number) => pw - (end - i - 0.5) * cw
    const top = 40
    const volH = s.vol ? ph * 0.18 : 0
    const bot = ph - volH - 8
    // Price range of the visible candles (Auto), or the manual range.
    let [lo, hi, vmax] = [Infinity, -Infinity, 0]
    for (let i = i0; i < i1; i++) {
      const c = view[i]
      lo = Math.min(lo, s.type === 'candle' ? c.l : c.c)
      hi = Math.max(hi, s.type === 'candle' ? c.h : c.c)
      vmax = Math.max(vmax, c.v)
    }
    if (hi <= lo) [hi, lo] = [hi * 1.01, lo * 0.99]
    const log = s.scale === 'log'
    const T = (r: number) => log ? Math.log(r) : r
    const iT = (r: number) => log ? Math.exp(r) : r
    if (s.auto || this.ylo === undefined) {
      const pd = (T(hi) - T(lo)) * 0.08
      this.ylo = T(lo) - pd
      this.yhi = T(hi) + pd
    }
    const [tlo, thi] = [this.ylo, this.yhi]
    const yOf = (r: number) => top + (thi - T(r)) / (thi - tlo) * (bot - top)
    const rOfY = (y: number) => iT(thi - (y - top) / (bot - top) * (thi - tlo))
    this.L = { pw, ph, cw, top, bot, tlo, thi }
    const base = view[Math.min(Math.max(i0, Math.ceil(end - this.n)), view.length - 1)].c
    const pct = s.scale === 'pct'
    const disp = (r: number) => pct ? (r / base - 1) * 100 : r / this.rf
    // Price grid. Axis decimals follow the grid step; tags get one more.
    const [rlo, rhi] = [rOfY(bot), rOfY(top)]
    const step = niceStep((disp(rhi) - disp(rlo)) / Math.max(2, (bot - top) / 45))
    const dec = Math.max(0, -Math.floor(Math.log10(step) + 1e-9))
    const lbl = (r: number, x = 0) => {
      const v = disp(r)
      return pct ? (v >= 0 ? '+' : '') + v.toFixed(Math.min(dec + x, 2)) + '%' : v.toFixed(Math.min(dec + x, 12))
    }
    ctx.strokeStyle = o.grid
    ctx.fillStyle = o.txt
    ctx.textAlign = 'left'
    let lastY = 1e9
    for (let v = Math.ceil(disp(rlo) / step) * step; v <= disp(rhi); v += step) {
      const r = pct ? base * (1 + v / 100) : v * this.rf
      const y = Math.round(yOf(r)) + 0.5
      if (lastY - y < 24) continue
      lastY = y
      hline(ctx, 0, pw, y)
      ctx.fillText(lbl(r), pw + 6, y)
    }
    // Adaptive time axis: the smallest interval giving >= 80px per label.
    const ib = Math.min(Math.max(i1 - 1, i0 + 1), view.length - 1)
    const msPx = Math.max(ib > i0 ? (view[ib].t - view[i0].t) / ((ib - i0) * cw) : 0, this.ms / cw)
    const ivMs = (v: number) => v < 0 ? -v * MONTH : v
    const iv = IVS.find(v => ivMs(v) >= 80 * msPx && ivMs(v) >= this.ms) || IVS[IVS.length - 1]
    ctx.textAlign = 'center'
    let [lastX, plt] = [-1e9, 0]
    let pk = i0 > 0 ? tkey(view[i0 - 1].t, iv) : null
    for (let i = i0; i < i1; i++) {
      const k = tkey(view[i].t, iv)
      if (pk !== null && k !== pk) {
        const X = Math.round(xOf(i)) + 0.5
        if (X - lastX > 56 && X > 22 && X < pw - 20) {
          lastX = X
          ctx.strokeStyle = o.grid
          vline(ctx, X, 0, ph)
          const [t, big] = tlabel(view[i].t, plt || view[i - 1].t)
          plt = view[i].t
          ctx.fillStyle = big ? o.hi : o.txt
          ctx.font = (big ? 'bold ' : '') + FONT
          ctx.fillText(t, X, ph + axH / 2)
          ctx.font = FONT
        }
      }
      pk = k
    }
    ctx.strokeStyle = o.axis
    vline(ctx, pw + 0.5, 0, h)
    hline(ctx, 0, w, ph + 0.5)
    // Series.
    ctx.save()
    ctx.beginPath()
    ctx.rect(0, 0, pw, ph)
    ctx.clip()
    const bw = Math.max(1, cw * 0.72)
    if (s.vol && vmax) {
      ctx.globalAlpha = 0.35
      for (let i = i0; i < i1; i++) {
        const c = view[i]
        if (!c.v) continue
        const vh = Math.max(1, c.v / vmax * (volH - 4))
        ctx.fillStyle = c.c >= c.o ? o.up : o.dn
        ctx.fillRect(xOf(i) - bw / 2, ph - vh, bw, vh)
      }
      ctx.globalAlpha = 1
    }
    const path = () => {
      ctx.beginPath()
      for (let i = i0; i < i1; i++) ctx.lineTo(xOf(i), yOf(view[i].c))
    }
    if (s.type === 'candle') {
      for (let i = i0; i < i1; i++) {
        const c = view[i]
        const X = xOf(i)
        if (!c.tr) { // no trades: a faint dash at the carried price
          ctx.fillStyle = o.txt
          ctx.globalAlpha = 0.35
          ctx.fillRect(X - bw / 2, Math.round(yOf(c.c)), bw, 1)
          ctx.globalAlpha = 1
          continue
        }
        ctx.fillStyle = ctx.strokeStyle = c.c >= c.o ? o.up : o.dn
        vline(ctx, Math.round(X) + 0.5, yOf(c.h), yOf(c.l))
        const y1 = yOf(Math.max(c.o, c.c))
        ctx.fillRect(X - bw / 2, y1, bw, Math.max(1, yOf(Math.min(c.o, c.c)) - y1))
      }
    } else {
      if (s.type === 'area') {
        path()
        ctx.lineTo(xOf(i1 - 1), bot + 8)
        ctx.lineTo(xOf(i0), bot + 8)
        const g = ctx.createLinearGradient(0, top, 0, bot)
        g.addColorStop(0, o.line + '59')
        g.addColorStop(1, o.line + '00')
        ctx.fillStyle = g
        ctx.fill()
      }
      path()
      ctx.strokeStyle = o.line
      ctx.lineWidth = 1.6
      ctx.stroke()
    }
    ctx.lineWidth = 1.2
    for (const [, col, a] of this.ind) {
      ctx.strokeStyle = col
      ctx.beginPath()
      let pen = false
      for (let i = i0; i < i1; i++) {
        if (isNaN(a[i])) pen = false
        else {
          ctx[pen ? 'lineTo' : 'moveTo'](xOf(i), yOf(a[i]))
          pen = true
        }
      }
      ctx.stroke()
    }
    ctx.lineWidth = 1
    ctx.restore()
    // Last price line, tag, and countdown to the candle close.
    const last = view[view.length - 1]
    const ly = yOf(last.c)
    const lc = last.c >= last.o ? o.up : o.dn
    if (ly > 0 && ly < ph) {
      ctx.setLineDash([2, 3])
      ctx.strokeStyle = lc
      hline(ctx, 0, pw, Math.round(ly) + 0.5)
      ctx.setLineDash([])
      // Bins up to 1D are aligned to the unix epoch, so the current one closes
      // at the next multiple of the bin size.
      const left = this.ms - Date.now() % this.ms
      tag(ctx, pw, ly, axW, [pct ? lbl(last.c, 1) : fmtPrice(last.c / this.rf), this.ms <= 3 * DAY ? dur(left) : ''], lc, '#fff')
    }
    // Crosshair.
    let hov = view.length - 1
    const m = this.m
    if (m && m.x < pw && m.y < ph) {
      const i = Math.round(end - 0.5 - (pw - m.x) / cw)
      if (i >= 0 && i < view.length) hov = i
      const cx = Math.round(xOf(hov)) + 0.5
      ctx.strokeStyle = o.cross
      ctx.setLineDash([4, 4])
      vline(ctx, cx, 0, ph)
      hline(ctx, 0, pw, Math.round(m.y) + 0.5)
      ctx.setLineDash([])
      if (m.y > top - 20 && m.y < bot + 8) tag(ctx, pw, m.y, axW, [lbl(rOfY(m.y), 1)], o.tag, o.tagTxt)
      const tl = tfull(view[hov].t, this.ms)
      const tw = ctx.measureText(tl).width + 14
      const tx = Math.min(Math.max(cx, tw / 2), pw - tw / 2)
      ctx.fillStyle = o.tag
      ctx.fillRect(tx - tw / 2, ph + 1, tw, axH - 1)
      ctx.fillStyle = o.tagTxt
      ctx.textAlign = 'center'
      ctx.fillText(tl, tx, ph + axH / 2)
    }
    // Legend: OHLC, change and volume of the hovered (else last) candle, then
    // the indicator values.
    const c = view[hov]
    const prev = view[hov - 1] || c
    const chg = (c.c - prev.c) / prev.c * 100
    const col = c.c >= c.o ? o.up : o.dn
    const p = (r: number) => fmtPrice(r / this.rf)
    const row: [string, string][] = [['O', p(c.o)], ['H', p(c.h)], ['L', p(c.l)], ['C', p(c.c)], ['', (chg >= 0 ? '+' : '') + chg.toFixed(2) + '%'], ['Vol', fmtVol(c.v / this.vf)]]
    if (w < 500) row.splice(4, 1)
    let lx = 8
    ctx.textAlign = 'left'
    for (const [k, v] of row) {
      if (k) {
        ctx.fillStyle = o.txt
        ctx.fillText(k, lx, 12)
        lx += ctx.measureText(k).width + 3
      }
      ctx.fillStyle = k === 'Vol' ? o.hi : col
      ctx.fillText(v, lx, 12)
      lx += ctx.measureText(v).width + 9
    }
    lx = 8
    for (const [name, cl, a] of this.ind) {
      const t = name + ' ' + (isNaN(a[hov]) ? '—' : p(a[hov]))
      ctx.fillStyle = cl
      ctx.fillText(t, lx, 28)
      lx += ctx.measureText(t).width + 12
    }
  }
}

// Time axis label intervals. Negative values are months.
const IVS = [3e5, 9e5, 18e5, HOUR, 2 * HOUR, 3 * HOUR, 6 * HOUR, 12 * HOUR, DAY, 2 * DAY, WEEK, 2 * WEEK, -1, -3, -12]
const MON = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
const WDAY = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']
const p2 = (n: number) => String(n).padStart(2, '0')

/* tkey is the local-time interval index of t. */
function tkey (t: number, iv: number): number {
  const d = new Date(t)
  if (iv < 0) return Math.floor((d.getFullYear() * 12 + d.getMonth()) / -iv)
  const lt = t - d.getTimezoneOffset() * 6e4
  return iv >= WEEK ? Math.floor((lt / DAY + 3) / (iv / DAY)) : Math.floor(lt / iv)
}

/* tlabel labels an interval boundary with its biggest changed unit. */
function tlabel (t: number, pt: number): [string, boolean] {
  const [d, p] = [new Date(t), new Date(pt)]
  if (d.getFullYear() !== p.getFullYear()) return [String(d.getFullYear()), true]
  if (d.getMonth() !== p.getMonth()) return [MON[d.getMonth()], true]
  if (d.getDate() !== p.getDate()) return [String(d.getDate()), false]
  return [p2(d.getHours()) + ':' + p2(d.getMinutes()), false]
}

function tfull (t: number, ms: number): string {
  const d = new Date(t)
  const s = `${WDAY[d.getDay()]} ${d.getDate()} ${MON[d.getMonth()]} '${String(d.getFullYear()).slice(2)}`
  return ms >= DAY ? s : `${s}  ${p2(d.getHours())}:${p2(d.getMinutes())}`
}

function dur (ms: number): string {
  const s = Math.floor(ms / 1e3)
  const hh = Math.floor(s / 3600)
  return (hh ? hh + ':' : '') + p2(Math.floor(s / 60) % 60) + ':' + p2(s % 60)
}

function hline (ctx: CanvasRenderingContext2D, x0: number, x1: number, y: number) {
  ctx.beginPath()
  ctx.moveTo(x0, y)
  ctx.lineTo(x1, y)
  ctx.stroke()
}

function vline (ctx: CanvasRenderingContext2D, x: number, y0: number, y1: number) {
  ctx.beginPath()
  ctx.moveTo(x, y0)
  ctx.lineTo(x, y1)
  ctx.stroke()
}

function tag (ctx: CanvasRenderingContext2D, pw: number, y: number, axW: number, lines: string[], bg: string, fg: string) {
  const y0 = Math.max(y - 9, 0)
  ctx.fillStyle = bg
  ctx.fillRect(pw + 1, y0, axW, lines[1] ? 30 : 18)
  ctx.fillStyle = fg
  ctx.textAlign = 'left'
  ctx.fillText(lines[0], pw + 6, y0 + 9)
  if (!lines[1]) return
  ctx.globalAlpha = 0.8
  ctx.fillText(lines[1], pw + 6, y0 + 22)
  ctx.globalAlpha = 1
}

function niceStep (raw: number): number {
  const p = Math.pow(10, Math.floor(Math.log10(raw)))
  const f = raw / p
  return (f < 1.5 ? 1 : f < 3.5 ? 2 : f < 7.5 ? 5 : 10) * p
}

/* fmtPrice shows about 4-5 significant figures without exponents. */
function fmtPrice (v: number): string {
  const a = Math.abs(v)
  if (!a) return '0'
  if (a >= 1e5) return v.toFixed(0)
  if (a >= 1) return v.toPrecision(5)
  return v.toFixed(Math.min(12, 3 - Math.floor(Math.log10(a))))
}

function fmtVol (v: number): string {
  return v >= 1e6 ? (v / 1e6).toFixed(2) + 'M' : v >= 1e3 ? (v / 1e3).toFixed(1) + 'K' : v.toFixed(v < 10 ? 2 : 0)
}

interface WaveOpts {
  message?: string
  backgroundColor?: string | boolean // true for <body> background color
}

/* Wave is a loading animation that displays a colorful line that oscillates */
export class Wave extends Chart {
  ani: Animation
  size: [number, number]
  region: Region
  colorShift: number
  opts: WaveOpts
  msgRegion: Region
  fontSize: number

  constructor (parent: HTMLElement, opts?: WaveOpts) {
    super(parent, {
      resize: () => this.resized(),
      click: (/* e: MouseEvent */) => { /* pass */ },
      zoom: (/* bigger: boolean */) => { /* pass */ }
    })
    this.canvas.classList.add('fill-abs')
    this.canvas.style.zIndex = '5'

    this.opts = opts ?? {}

    const period = 1500 // ms
    const start = Math.random() * period
    this.colorShift = Math.random() * 360

    // y = A*cos(k*x + theta*t + c)
    // combine three waves with different periods and speeds and phases.
    const amplitudes = [1, 0.65, 0.75]
    const ks = [3, 3, 2]
    const speeds = [Math.PI, Math.PI * 10 / 9, Math.PI / 2.5]
    const phases = [0, 0, Math.PI * 1.5]
    const n = 75
    const single = (n: number, angularX: number, angularTime: number): number => {
      return amplitudes[n] * Math.cos(ks[n] * angularX + speeds[n] * angularTime + phases[n])
    }
    const value = (x: number, angularTime: number): number => {
      const angularX = x * Math.PI * 2
      return (single(0, angularX, angularTime) + single(1, angularX, angularTime) + single(2, angularX, angularTime)) / 3
    }
    this.resize()
    this.ani = new Animation(Animation.Forever, () => {
      const angularTime = (new Date().getTime() - start) / period * Math.PI * 2
      const values = []
      for (let i = 0; i < n; i++) {
        values.push(value(i / (n - 1), angularTime))
      }
      this.drawValues(values)
    })
  }

  resized () {
    const opts = this.opts
    const [maxW, maxH] = [150, 100]
    const [cw, ch] = [this.canvas.width, this.canvas.height]
    let [w, h] = [cw * 0.8, ch * 0.8]
    if (w > maxW) w = maxW
    if (h > maxH) h = maxH
    let [l, t] = [(cw - w) / 2, (ch - h) / 2]
    if (opts.message) {
      this.fontSize = clamp(h * 0.15, 10, 14)
      this.applyLabelStyle(this.fontSize)
      const ypad = this.fontSize * 0.5
      const halfH = (this.fontSize / 2) + ypad
      t -= halfH
      this.msgRegion = new Region(this.ctx, new Extents(0, cw, t + h, t + h + 2 * halfH))
    }
    this.region = new Region(this.ctx, new Extents(l, l + w, t, t + h))
  }

  drawValues (values: number[]) {
    if (!this.region) return
    this.clear()
    const hsl = (h: number) => `hsl(${h}, 35%, 50%)`

    const { region, msgRegion, canvas: { width: w, height: h }, opts: { backgroundColor: bg, message: msg }, colorShift, ctx } = this

    if (bg) {
      if (bg === true) ctx.fillStyle = State.isDark() ? '#0a1e34' : '#f0f0f0'
      else ctx.fillStyle = bg
      ctx.fillRect(0, 0, w, h)
    }

    region.plot(new Extents(0, 1, -1, 1), (ctx: CanvasRenderingContext2D, t: Translator) => {
      ctx.lineWidth = 4
      ctx.lineCap = 'round'

      const shift = colorShift + (new Date().getTime() % 2000) / 2000 * 360 // colors move with frequency 1 / 2s
      const grad = ctx.createLinearGradient(t.x(0), 0, t.x(1), 0)
      grad.addColorStop(0, hsl(shift))
      ctx.strokeStyle = grad

      ctx.beginPath()
      ctx.moveTo(t.x(0), t.y(values[0]))
      for (let i = 1; i < values.length; i++) {
        const prog = i / (values.length - 1)
        grad.addColorStop(prog, hsl(prog * 300 + shift))
        ctx.lineTo(t.x(prog), t.y(values[i]))
      }
      ctx.stroke()
    })
    if (!msg) return
    msgRegion.plot(new Extents(0, 1, 0, 1), (ctx: CanvasRenderingContext2D, t: Translator) => {
      this.applyLabelStyle(this.fontSize)
      ctx.fillText(msg, t.x(0.5), t.y(0.5), this.msgRegion.width())
    })
  }

  render () { /* pass */ }

  stop () {
    this.ani.stop()
    this.canvas.remove()
  }
}

/*
 * Extents holds a min and max in both the x and y directions, and provides
 * getters for related data.
 */
export class Extents {
  x: MinMax
  y: MinMax

  constructor (xMin: number, xMax: number, yMin: number, yMax: number) {
    this.setExtents(xMin, xMax, yMin, yMax)
  }

  setExtents (xMin: number, xMax: number, yMin: number, yMax: number) {
    this.x = {
      min: xMin,
      max: xMax
    }
    this.y = {
      min: yMin,
      max: yMax
    }
  }

  get xRange (): number {
    return this.x.max - this.x.min
  }

  get midX (): number {
    return (this.x.max + this.x.min) / 2
  }

  get yRange (): number {
    return this.y.max - this.y.min
  }

  get midY (): number {
    return (this.y.max + this.y.min) / 2
  }
}

/*
 * Region applies an Extents to the canvas, providing utilities for coordinate
 * transformations and restricting drawing to a specified region of the canvas.
 */
export class Region {
  context: CanvasRenderingContext2D
  extents: Extents

  constructor (context: CanvasRenderingContext2D, extents: Extents) {
    this.context = context
    this.extents = extents
  }

  setExtents (xMin: number, xMax: number, yMin: number, yMax: number) {
    this.extents.setExtents(xMin, xMax, yMin, yMax)
  }

  width (): number {
    return this.extents.xRange
  }

  height (): number {
    return this.extents.yRange
  }

  contains (x: number, y: number): boolean {
    const ext = this.extents
    return (x < ext.x.max && x > ext.x.min &&
      y < ext.y.max && y > ext.y.min)
  }

  /*
   * A translator provides 4 function for coordinate transformations. x and y
   * translate data coordinates to canvas coordinates for the specified data
   * Extents. unx and uny translate canvas coordinates to data coordinates.
   */
  translator (dataExtents: Extents): Translator {
    const region = this.extents
    const xMin = dataExtents.x.min
    // const xMax = dataExtents.x.max
    const yMin = dataExtents.y.min
    // const yMax = dataExtents.y.max
    const yRange = dataExtents.yRange
    const xRange = dataExtents.xRange
    const screenMinX = region.x.min
    const screenW = region.x.max - screenMinX
    const screenMaxY = region.y.max
    const screenH = screenMaxY - region.y.min
    const xFactor = screenW / xRange
    const yFactor = screenH / yRange
    return {
      x: (x: number) => (x - xMin) * xFactor + screenMinX,
      y: (y: number) => screenMaxY - (y - yMin) * yFactor,
      unx: (x: number) => (x - screenMinX) / xFactor + xMin,
      uny: (y: number) => yMin - (y - screenMaxY) / yFactor,
      w: (w: number) => w / xRange * screenW,
      h: (h: number) => -h / yRange * screenH
    }
  }

  /* clear clears the region. */
  clear () {
    const ext = this.extents
    this.context.clearRect(ext.x.min, ext.y.min, ext.xRange, ext.yRange)
  }

  /* plot prepares tools for drawing using data coordinates. */
  plot (dataExtents: Extents, drawFunc: (ctx: CanvasRenderingContext2D, tools: Translator) => void, skipMask?: boolean) {
    const ctx = this.context
    const region = this.extents
    ctx.save() // Save the original state
    if (!skipMask) {
      ctx.beginPath()
      ctx.rect(region.x.min, region.y.min, region.xRange, region.yRange)
      ctx.clip()
    }

    // The drawFunc will be passed a set of tool that can be used to assist
    // drawing. The tools start with the transformation functions.
    const tools = this.translator(dataExtents)

    // Create a transformation that allows drawing in data coordinates. It's
    // not advisable to stroke or add text with this transform in place, as the
    // result will be distorted. You can however use ctx.moveTo and ctx.lineTo
    // with this transform in place using data coordinates, and remove the
    // transform before stroking. The dataCoords method of the supplied tool
    // provides this functionality.

    // TODO: Figure out why this doesn't work on WebView.
    // const yRange = dataExtents.yRange
    // const xFactor = region.xRange / dataExtents.xRange
    // const yFactor = region.yRange / yRange
    // const xMin = dataExtents.x.min
    // const yMin = dataExtents.y.min
    // // These translation factors are complicated because the (0, 0) of the
    // // region is not necessarily the (0, 0) of the canvas.
    // const tx = (region.x.min + xMin) - xMin * xFactor
    // const ty = -region.y.min - (yRange - yMin) * yFactor
    // const setTransform = () => {
    //   // Data coordinates are flipped about y. Flip the coordinates and
    //   // translate top left corner to canvas (0, 0).
    //   ctx.transform(1, 0, 0, -1, -xMin, yMin)
    //   // Scale to data coordinates and shift into place for the region's offset
    //   // on the canvas.
    //   ctx.transform(xFactor, 0, 0, yFactor, tx, ty)
    // }
    // // dataCoords allows some drawing to be performed directly in data
    // // coordinates. Most actual drawing functions like ctx.stroke and
    // // ctx.fillRect should not be called from inside dataCoords, but
    // // ctx.moveTo and ctx.LineTo are fine.
    // tools.dataCoords = f => {
    //   ctx.save()
    //   setTransform()
    //   f()
    //   ctx.restore()
    // }

    drawFunc(this.context, tools)
    ctx.restore()
  }
}

/*
 * makeLabels attempts to create the appropriate labels for the specified
 * screen size, context, and label spacing.
 */
function makeLabels (
  ctx: CanvasRenderingContext2D,
  screenW: number,
  min: number,
  max: number,
  spacingGuess: number,
  step: number,
  unit: string,
  valFmt?: (v: number) => string
): LabelSet {
  valFmt = valFmt || Doc.formatFourSigFigs
  const n = screenW / spacingGuess
  const diff = max - min
  if (n < 1 || diff <= 0) return { lbls: [] }
  const tickGuess = diff / n
  // make the tick spacing a multiple of the step
  const tick = tickGuess + step - (tickGuess % step)
  let x = min + tick - (min % tick)
  const absMax = Math.max(Math.abs(max), Math.abs(min))
  // The Math.round part is the minimum precision required to see the change in the numbers.
  // The 2 accounts for the precision of the tick.
  const sigFigs = Math.round(Math.log10(absMax / tick)) + 2
  const pts: Label[] = []
  let widest = 0
  while (x < max) {
    x = Number(x.toPrecision(sigFigs))
    const lbl = valFmt(x)
    widest = Math.max(widest, ctx.measureText(lbl).width)
    pts.push({
      val: x,
      txt: lbl
    })
    x += tick
  }
  const unitW = ctx.measureText(unit).width
  if (unitW > widest) widest = unitW
  return {
    widest: widest,
    lbls: pts
  }
}

/* The last element of an array. */
function last (arr: any[]): any {
  return arr[arr.length - 1]
}

/* line draws a line with the provided context. */
function line (ctx: CanvasRenderingContext2D, x0: number, y0: number, x1: number, y1: number, skipStroke?: boolean) {
  ctx.beginPath()
  ctx.moveTo(x0, y0)
  ctx.lineTo(x1, y1)
  if (!skipStroke) ctx.stroke()
}

/* dot draws a circle with the provided context. */
function dot (ctx: CanvasRenderingContext2D, x: number, y: number, color: string, radius: number) {
  ctx.fillStyle = color
  ctx.beginPath()
  ctx.arc(x, y, radius, 0, PIPI)
  ctx.fill()
}

/* floatCompare compares two floats to within a tolerance of  1e-8. */
function floatCompare (a: number, b: number) {
  return withinTolerance(a, b, 1e-8)
}

/*
 * withinTolerance returns true if the difference between a and b are with
 * the specified tolerance.
 */
function withinTolerance (a: number, b: number, tolerance: number) {
  return Math.abs(a - b) < Math.abs(tolerance)
}
