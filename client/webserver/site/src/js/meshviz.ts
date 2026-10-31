/*
 * MeshViz draws the Tatanka mesh around this client on a canvas: mesh nodes
 * as glowing hexagons, other trading clients as glowing dots, our own client
 * in the middle with the bison logo and a health ring. Links pulse when
 * messages flow, new orders travel as coin particles, and a match lights up
 * the path between the two parties while its swap steps through lock, audit,
 * redeem and complete. It is a tiny force layout with no dependencies, and it
 * honors prefers-reduced-motion by drawing static frames only.
 */

export type VizKind = 'self' | 'node' | 'peer'

export interface VizPeer {
  id: string
  kind: VizKind
  label: string
  sub?: string // second label line, e.g. the protocol version
  online: boolean
  incompatible?: boolean
  unknownVersion?: boolean
  node?: string // the mesh node a client is connected to
  connected?: boolean // for mesh nodes: our current node
  title?: string // tooltip
}

interface SimNode extends VizPeer {
  x: number
  y: number
  vx: number
  vy: number
  alpha: number // fade in/out
  target: number // target alpha
  pulse: number // glow boost, decays
  born: number
}

interface Particle {
  path: [number, number][] // filled lazily from node ids
  ids: string[]
  t: number // 0..1 along the whole path
  speed: number
  img?: HTMLImageElement
  color: string
}

interface MatchGlow {
  id: string
  a: string
  b: string
  step: number // 0..4
  label: string
  until: number // ms timestamp when to stop showing (after complete)
  burst: number // checkmark burst timer
  failed: boolean
}

export type Health = 'good' | 'degraded' | 'offline'

const TAU = Math.PI * 2
const MAX_PARTICLES = 80

export default class MeshViz {
  canvas: HTMLCanvasElement
  ctx: CanvasRenderingContext2D
  nodes: Map<string, SimNode>
  selfID: string
  particles: Particle[]
  matches: Map<string, MatchGlow>
  health: Health
  reduced: boolean
  raf: number
  running: boolean
  w: number
  h: number
  dpr: number
  bison: HTMLImageElement
  coinImgs: Record<string, HTMLImageElement>
  hover: SimNode | null
  resizeObs: ResizeObserver | null
  lastFrame: number
  onHover?: (p: VizPeer | null, x: number, y: number) => void
  dark: boolean

  constructor (canvas: HTMLCanvasElement) {
    this.canvas = canvas
    const ctx = canvas.getContext('2d')
    if (!ctx) throw Error('no 2d context')
    this.ctx = ctx
    this.nodes = new Map()
    this.selfID = ''
    this.particles = []
    this.matches = new Map()
    this.health = 'offline'
    this.reduced = Boolean(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches)
    this.raf = 0
    this.running = false
    this.w = 0
    this.h = 0
    this.dpr = 1
    this.hover = null
    this.lastFrame = 0
    this.dark = document.body.classList.contains('dark')
    this.bison = new Image()
    this.bison.src = '/img/bison-square_50.png'
    this.coinImgs = {}
    this.resizeObs = null
    if (window.ResizeObserver) {
      this.resizeObs = new ResizeObserver(() => { this.resize() })
      this.resizeObs.observe(canvas)
    }
    canvas.addEventListener('mousemove', (e: MouseEvent) => { this.mouseMove(e) })
    canvas.addEventListener('mouseleave', () => {
      this.hover = null
      if (this.onHover) this.onHover(null, 0, 0)
    })
    this.resize()
  }

  coinImg (symbol: string): HTMLImageElement {
    let img = this.coinImgs[symbol]
    if (!img) {
      img = new Image()
      img.src = `/img/coins/${symbol}.png`
      this.coinImgs[symbol] = img
    }
    return img
  }

  resize () {
    const r = this.canvas.getBoundingClientRect()
    this.dpr = window.devicePixelRatio || 1
    this.w = Math.max(r.width, 100)
    this.h = Math.max(r.height, 100)
    this.canvas.width = Math.round(this.w * this.dpr)
    this.canvas.height = Math.round(this.h * this.dpr)
    if (!this.running) this.draw(0)
  }

  start () {
    if (this.running) return
    this.running = true
    const frame = (ts: number) => {
      if (!this.running) return
      const dt = this.lastFrame ? Math.min((ts - this.lastFrame) / 1000, 0.1) : 0.016
      this.lastFrame = ts
      if (!this.reduced) this.step(dt)
      else this.settle()
      this.draw(dt)
      // Reduced motion: redraw rarely, only to reflect state changes.
      if (this.reduced) {
        window.setTimeout(() => { this.raf = requestAnimationFrame(frame) }, 1000)
      } else {
        this.raf = requestAnimationFrame(frame)
      }
    }
    this.raf = requestAnimationFrame(frame)
  }

  stop () {
    this.running = false
    cancelAnimationFrame(this.raf)
    if (this.resizeObs) this.resizeObs.disconnect()
  }

  /* setSelf sets our own client in the middle. */
  setSelf (p: VizPeer) {
    this.selfID = p.id
    this.upsert({ ...p, kind: 'self' })
  }

  setHealth (h: Health) { this.health = h }

  upsert (p: VizPeer) {
    const n = this.nodes.get(p.id)
    if (n) {
      Object.assign(n, p)
      n.target = p.online ? 1 : 0.35
      return
    }
    // New nodes enter near something they're linked to, else on a circle.
    let x = this.w / 2 + (Math.random() - 0.5) * 40
    let y = this.h / 2 + (Math.random() - 0.5) * 40
    const anchor = p.node ? this.nodes.get(p.node) : this.nodes.get(this.selfID)
    if (anchor && p.kind !== 'self') {
      const a = Math.random() * TAU
      x = anchor.x + Math.cos(a) * 60
      y = anchor.y + Math.sin(a) * 60
    }
    this.nodes.set(p.id, {
      ...p, x, y, vx: 0, vy: 0, alpha: this.reduced ? 1 : 0, target: p.online ? 1 : 0.35, pulse: 1, born: performance.now()
    })
  }

  /* remove fades a node out and deletes it. */
  remove (id: string) {
    const n = this.nodes.get(id)
    if (!n || id === this.selfID) return
    n.target = 0
  }

  /* links returns the edges of the graph. */
  links (): [SimNode, SimNode, boolean][] {
    const out: [SimNode, SimNode, boolean][] = []
    const meshNodes: SimNode[] = []
    for (const n of this.nodes.values()) if (n.kind === 'node') meshNodes.push(n)
    // Mesh nodes form a full mesh among themselves (dashed: assumed).
    for (let i = 0; i < meshNodes.length; i++) {
      for (let j = i + 1; j < meshNodes.length; j++) out.push([meshNodes[i], meshNodes[j], false])
    }
    for (const n of this.nodes.values()) {
      if (n.kind === 'node') continue
      const node = n.node ? this.nodes.get(n.node) : undefined
      if (node) out.push([n, node, true])
    }
    return out
  }

  neighbors (id: string): string[] {
    const out: string[] = []
    for (const [a, b] of this.links()) {
      if (a.id === id) out.push(b.id)
      else if (b.id === id) out.push(a.id)
    }
    return out
  }

  /* path finds the shortest path between two ids (BFS). */
  path (from: string, to: string): string[] {
    if (from === to) return [from]
    const prev = new Map<string, string>()
    const q = [from]
    const seen = new Set([from])
    while (q.length) {
      const cur = q.shift() as string
      for (const nb of this.neighbors(cur)) {
        if (seen.has(nb)) continue
        seen.add(nb)
        prev.set(nb, cur)
        if (nb === to) {
          const p = [to]
          let c = to
          while (prev.has(c)) { c = prev.get(c) as string; p.unshift(c) }
          return p
        }
        q.push(nb)
      }
    }
    return []
  }

  /* pulse makes a node glow briefly. */
  pulse (id: string) {
    const n = this.nodes.get(id)
    if (n) n.pulse = 1
  }

  /* sendParticle animates something traveling from one client to another. */
  sendParticle (from: string, to: string, symbol?: string, color?: string) {
    if (this.reduced) { this.pulse(from); this.pulse(to); return }
    const ids = this.path(from, to)
    if (ids.length < 2) return
    if (this.particles.length >= MAX_PARTICLES) this.particles.shift()
    this.particles.push({
      ids, path: [], t: 0, speed: 0.45 / Math.max(ids.length - 1, 1) * 2,
      img: symbol ? this.coinImg(symbol) : undefined, color: color || '#2dd4bf'
    })
    this.pulse(from)
  }

  /* broadcast animates an order spreading from its owner to every client. */
  broadcast (from: string, symbol: string) {
    let n = 0
    for (const p of this.nodes.values()) {
      if (p.id === from || p.kind === 'node' || p.target === 0) continue
      this.sendParticle(from, p.id, symbol)
      if (++n > 12) break
    }
    if (n === 0) {
      // Nobody else yet: show it reaching our mesh node.
      const self = this.nodes.get(from)
      if (self && self.node) this.sendParticle(from, self.node, symbol)
    }
  }

  /* setMatch lights the path between maker and taker at a swap step. */
  setMatch (id: string, a: string, b: string, step: number, label: string, failed = false) {
    let m = this.matches.get(id)
    if (!m) {
      m = { id, a, b, step, label, until: 0, burst: 0, failed }
      this.matches.set(id, m)
    }
    const prevStep = m.step
    m.step = step
    m.label = label
    m.failed = failed
    if (step >= 4 && prevStep < 4) {
      m.burst = 1
      m.until = performance.now() + 12000
    } else if (failed) {
      m.until = performance.now() + 12000
    }
    if (step > prevStep || prevStep === undefined) this.sendParticle(a, b, undefined, '#fbbf24')
  }

  settle () {
    // Reduced motion: run the force layout to rest without animating.
    for (let i = 0; i < 30; i++) this.step(0.05, true)
  }

  step (dt: number, quiet = false) {
    const cx = this.w / 2
    const cy = this.h / 2
    const arr = Array.from(this.nodes.values())
    const k = Math.min(this.w, this.h)
    // Repulsion.
    for (let i = 0; i < arr.length; i++) {
      const a = arr[i]
      for (let j = i + 1; j < arr.length; j++) {
        const b = arr[j]
        let dx = a.x - b.x
        let dy = a.y - b.y
        let d2 = dx * dx + dy * dy
        if (d2 < 1) { dx = Math.random() - 0.5; dy = Math.random() - 0.5; d2 = 1 }
        const f = 7000 / d2
        const d = Math.sqrt(d2)
        a.vx += f * dx / d; a.vy += f * dy / d
        b.vx -= f * dx / d; b.vy -= f * dy / d
      }
    }
    // Springs.
    for (const [a, b, solid] of this.links()) {
      const dx = b.x - a.x
      const dy = b.y - a.y
      const d = Math.max(Math.sqrt(dx * dx + dy * dy), 1)
      const rest = solid ? k * 0.3 : k * 0.42
      const f = (d - rest) * 0.02
      a.vx += f * dx / d; a.vy += f * dy / d
      b.vx -= f * dx / d; b.vy -= f * dy / d
    }
    for (const n of arr) {
      // Gentle pull to the center keeps unlinked nodes on screen.
      n.vx += (cx - n.x) * 0.003
      n.vy += (cy - n.y) * 0.003
      n.vx *= 0.82
      n.vy *= 0.82
      if (n.id === this.selfID) {
        n.x = cx; n.y = cy; n.vx = 0; n.vy = 0
      } else {
        n.x += n.vx * Math.min(dt * 60, 3)
        n.y += n.vy * Math.min(dt * 60, 3)
      }
      n.x = Math.min(Math.max(n.x, 30), this.w - 30)
      n.y = Math.min(Math.max(n.y, 24), this.h - 30)
      if (!quiet) {
        n.alpha += (n.target - n.alpha) * Math.min(dt * 3, 1)
        n.pulse = Math.max(0, n.pulse - dt * 0.8)
      } else {
        n.alpha = n.target
      }
    }
    for (const [id, n] of this.nodes) if (n.target === 0 && n.alpha < 0.02) this.nodes.delete(id)
    for (const p of this.particles) p.t += p.speed * dt
    this.particles = this.particles.filter(p => p.t < 1)
    const now = performance.now()
    for (const [id, m] of this.matches) {
      m.burst = Math.max(0, m.burst - dt * 0.7)
      if (m.until && now > m.until) this.matches.delete(id)
    }
  }

  color (n: SimNode): string {
    if (n.kind === 'self') return '#2dd4bf'
    if (n.kind === 'node') return n.connected ? '#a78bfa' : '#8b5cf6'
    if (n.incompatible) return '#f97316'
    if (n.unknownVersion) return '#94a3b8'
    return '#38bdf8'
  }

  draw (_dt: number) {
    const { ctx, dpr } = this
    this.dark = document.body.classList.contains('dark')
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    ctx.clearRect(0, 0, this.w, this.h)
    const textColor = this.dark ? '#e5e7eb' : '#1f2937'
    const subColor = this.dark ? '#9ca3af' : '#6b7280'
    const t = performance.now() / 1000

    // Links.
    for (const [a, b, solid] of this.links()) {
      const alpha = Math.min(a.alpha, b.alpha)
      ctx.save()
      ctx.globalAlpha = alpha * (solid ? 0.55 : 0.25)
      ctx.strokeStyle = this.dark ? '#64748b' : '#94a3b8'
      ctx.lineWidth = solid ? 1.6 : 1
      if (!solid) ctx.setLineDash([4, 5])
      ctx.beginPath(); ctx.moveTo(a.x, a.y); ctx.lineTo(b.x, b.y); ctx.stroke()
      ctx.restore()
    }

    // Match paths.
    for (const m of this.matches.values()) {
      const ids = this.path(m.a, m.b)
      if (ids.length < 2) continue
      const pts = ids.map(id => this.nodes.get(id) as SimNode)
      ctx.save()
      const glow = 0.6 + 0.4 * Math.sin(t * 4)
      ctx.strokeStyle = m.failed ? '#ef4444' : m.step >= 4 ? '#22c55e' : '#fbbf24'
      ctx.globalAlpha = this.reduced ? 0.9 : glow
      ctx.lineWidth = 4
      ctx.shadowColor = ctx.strokeStyle
      ctx.shadowBlur = 12
      ctx.beginPath()
      ctx.moveTo(pts[0].x, pts[0].y)
      for (const p of pts.slice(1)) ctx.lineTo(p.x, p.y)
      ctx.stroke()
      // Step label at the middle of the path.
      const mid = pts[Math.floor(pts.length / 2)]
      const prev = pts[Math.floor(pts.length / 2) - 1] || mid
      let lx = (mid.x + prev.x) / 2
      let ly = (mid.y + prev.y) / 2
      if (pts.length === 2 || mid === prev) { lx = (pts[0].x + pts[1].x) / 2; ly = (pts[0].y + pts[1].y) / 2 }
      ly -= 22
      ctx.shadowBlur = 0
      ctx.globalAlpha = 1
      this.stepDots(lx, ly, m)
      if (m.burst > 0) this.checkBurst(pts[0], pts[pts.length - 1], m.burst)
      ctx.restore()
    }

    // Particles.
    for (const p of this.particles) {
      const pts = p.ids.map(id => this.nodes.get(id)).filter(Boolean) as SimNode[]
      if (pts.length < 2) continue
      const segs = pts.length - 1
      const pos = p.t * segs
      const i = Math.min(Math.floor(pos), segs - 1)
      const f = pos - i
      const x = pts[i].x + (pts[i + 1].x - pts[i].x) * f
      const y = pts[i].y + (pts[i + 1].y - pts[i].y) * f
      if (f > 0.9 && pts[i + 1]) pts[i + 1].pulse = Math.max(pts[i + 1].pulse, 0.5)
      ctx.save()
      ctx.globalAlpha = Math.min(1, (1 - p.t) * 4)
      if (p.img && p.img.complete && p.img.naturalWidth) {
        ctx.shadowColor = '#fde68a'
        ctx.shadowBlur = 10
        ctx.drawImage(p.img, x - 8, y - 8, 16, 16)
      } else {
        ctx.fillStyle = p.color
        ctx.shadowColor = p.color
        ctx.shadowBlur = 12
        ctx.beginPath(); ctx.arc(x, y, 4, 0, TAU); ctx.fill()
      }
      ctx.restore()
    }

    // Nodes.
    for (const n of this.nodes.values()) {
      ctx.save()
      ctx.globalAlpha = n.alpha
      const c = this.color(n)
      const breathe = this.reduced ? 0 : Math.sin(t * 2 + n.born / 500) * 2
      if (n.kind === 'self') {
        this.healthRing(n.x, n.y, t)
        ctx.shadowColor = c
        ctx.shadowBlur = 18 + n.pulse * 20
        ctx.fillStyle = this.dark ? '#0f172a' : '#ffffff'
        ctx.beginPath(); ctx.arc(n.x, n.y, 22, 0, TAU); ctx.fill()
        ctx.shadowBlur = 0
        if (this.bison.complete && this.bison.naturalWidth) {
          ctx.drawImage(this.bison, n.x - 15, n.y - 15, 30, 30)
        } else {
          ctx.fillStyle = c
          ctx.beginPath(); ctx.arc(n.x, n.y, 10, 0, TAU); ctx.fill()
        }
      } else if (n.kind === 'node') {
        ctx.shadowColor = c
        ctx.shadowBlur = 14 + n.pulse * 18 + breathe
        ctx.fillStyle = c
        this.hexagon(n.x, n.y, n.connected ? 13 : 11)
        ctx.fill()
        if (n.connected) {
          ctx.shadowBlur = 0
          ctx.strokeStyle = '#ffffff'
          ctx.lineWidth = 1.5
          this.hexagon(n.x, n.y, 13)
          ctx.stroke()
        }
      } else {
        ctx.shadowColor = c
        ctx.shadowBlur = 12 + n.pulse * 18 + breathe
        ctx.fillStyle = c
        ctx.beginPath(); ctx.arc(n.x, n.y, 9, 0, TAU); ctx.fill()
        if (!n.online) {
          ctx.shadowBlur = 0
          ctx.strokeStyle = subColor
          ctx.setLineDash([2, 3])
          ctx.beginPath(); ctx.arc(n.x, n.y, 13, 0, TAU); ctx.stroke()
        }
      }
      // Labels.
      ctx.shadowBlur = 0
      ctx.textAlign = 'center'
      ctx.fillStyle = textColor
      ctx.font = n.kind === 'self' ? 'bold 13px sans-serif' : '12px sans-serif'
      const ly = n.y + (n.kind === 'self' ? 40 : 25)
      ctx.fillText(n.label, n.x, ly)
      if (n.sub) {
        ctx.font = '10px sans-serif'
        ctx.fillStyle = n.incompatible ? '#f97316' : subColor
        ctx.fillText(n.sub, n.x, ly + 12)
      }
      ctx.restore()
    }
  }

  hexagon (x: number, y: number, r: number) {
    const { ctx } = this
    ctx.beginPath()
    for (let i = 0; i < 6; i++) {
      const a = Math.PI / 6 + i * Math.PI / 3
      const px = x + r * Math.cos(a)
      const py = y + r * Math.sin(a)
      if (i === 0) ctx.moveTo(px, py)
      else ctx.lineTo(px, py)
    }
    ctx.closePath()
  }

  healthRing (x: number, y: number, t: number) {
    const { ctx } = this
    const col = this.health === 'good' ? '#22c55e' : this.health === 'degraded' ? '#eab308' : '#ef4444'
    ctx.save()
    ctx.strokeStyle = col
    ctx.lineWidth = 3
    ctx.shadowColor = col
    ctx.shadowBlur = 10
    const spin = this.reduced ? 0 : t * (this.health === 'good' ? 0.6 : 1.5)
    if (this.health === 'good') {
      ctx.beginPath(); ctx.arc(x, y, 29, 0, TAU); ctx.stroke()
    } else {
      ctx.setLineDash([8, 6])
      ctx.lineDashOffset = -spin * 20
      ctx.beginPath(); ctx.arc(x, y, 29, 0, TAU); ctx.stroke()
    }
    ctx.restore()
  }

  stepDots (x: number, y: number, m: MatchGlow) {
    const { ctx } = this
    const w = 4 * 14
    ctx.save()
    for (let i = 0; i < 4; i++) {
      const px = x - w / 2 + i * 14 + 7
      ctx.beginPath()
      ctx.arc(px, y, 4.5, 0, TAU)
      ctx.fillStyle = i < m.step ? (m.failed ? '#ef4444' : '#22c55e') : (i === m.step ? '#fbbf24' : '#64748b')
      ctx.fill()
    }
    ctx.font = 'bold 11px sans-serif'
    ctx.textAlign = 'center'
    ctx.fillStyle = m.failed ? '#ef4444' : '#f59e0b'
    ctx.fillText(m.label, x, y - 9)
    ctx.restore()
  }

  checkBurst (a: SimNode, b: SimNode, f: number) {
    const { ctx } = this
    for (const n of [a, b]) {
      ctx.save()
      ctx.globalAlpha = f
      ctx.strokeStyle = '#22c55e'
      ctx.lineWidth = 3
      const r = 16 + (1 - f) * 30
      ctx.beginPath(); ctx.arc(n.x, n.y, r, 0, TAU); ctx.stroke()
      for (let i = 0; i < 8; i++) {
        const ang = i * TAU / 8
        ctx.beginPath()
        ctx.moveTo(n.x + Math.cos(ang) * (r + 3), n.y + Math.sin(ang) * (r + 3))
        ctx.lineTo(n.x + Math.cos(ang) * (r + 9), n.y + Math.sin(ang) * (r + 9))
        ctx.stroke()
      }
      ctx.lineWidth = 4
      ctx.beginPath()
      ctx.moveTo(n.x - 7, n.y - 30)
      ctx.lineTo(n.x - 2, n.y - 25)
      ctx.lineTo(n.x + 8, n.y - 36)
      ctx.stroke()
      ctx.restore()
    }
  }

  mouseMove (e: MouseEvent) {
    const r = this.canvas.getBoundingClientRect()
    const x = e.clientX - r.left
    const y = e.clientY - r.top
    let best: SimNode | null = null
    let bd = 22 * 22
    for (const n of this.nodes.values()) {
      const d = (n.x - x) ** 2 + (n.y - y) ** 2
      if (d < bd) { bd = d; best = n }
    }
    this.hover = best
    if (this.onHover) this.onHover(best, x, y)
  }
}
