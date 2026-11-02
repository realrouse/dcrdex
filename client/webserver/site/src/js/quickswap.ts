import BasePage from './basepage'
import Doc from './doc'
import { postJSON } from './http'
import { app, PageElement, SupportedAsset } from './registry'

// Quick Swap: swap any coin for another over the Tatanka mesh and DEX server
// markets, directly or through one intermediate coin. The route finder and
// executor are in client/core/quickswap*.go.

interface Fill { orderID: string, owner?: string, rate: number, lots: number, lotSize: number, qty: number, in: number, out: number }

interface StepPlan {
  source: string // mesh | server
  host: string
  market: string
  base: number
  quote: number
  fromAsset: number
  toAsset: number
  fromSymbol: string
  toSymbol: string
  sell: boolean
  lotSize: number
  in: number
  out: number
  avgRate: number
  worstRate: number
  bookOrders: number
  fills: Fill[] | null
}

interface Route {
  from: number
  to: number
  fromSymbol: string
  toSymbol: string
  amount: number
  in: number
  out: number
  leftover: number
  steps: StepPlan[]
  label: string
}

interface Quote {
  route: Route | null
  others: Route[] | null
  routes: number
  driftPct: number
  minOut: number
  markets: string[] | null
}

interface Take {
  orderID: string
  ownerName?: string
  matchID?: string
  rate: number
  lots: number
  qty: number
  in: number
  out: number
  status: string // proposing, swapping, complete, failed
  swapStatus?: string
  reason?: string
  replacement: boolean
  worsePct: number
  started: number
  deadline?: number
}

interface StepRun {
  plan: StepPlan
  status: string // pending, running, complete, stopped
  budget: number
  target: number
  spent: number
  received: number
  committed: number
  takes: Take[] | null
  round: number
  maxRounds: number
  wait?: { until: number, from?: number, msg: string }
  note?: string
  worsePct: number
  serverOrder?: string
  started?: number
  ended?: number
}

interface Run {
  id: string
  status: string // running, complete, stopped, failed
  message?: string
  started: number
  ended?: number
  from: number
  to: number
  fromSymbol: string
  toSymbol: string
  amount: number
  driftPct: number
  expected: number
  minOut: number
  projected: number
  driftUsedPct: number
  received: number
  route: Route
  step: number
  steps: StepRun[]
  kept?: { asset: number, symbol: string, amount: number }[]
  stopRequested: boolean
  log: { time: number, msg: string, kind?: string }[] | null
}

const MeshHost = 'tatanka-mesh'

function esc (s: string | number): string {
  return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c] as string))
}

function sym (assetID: number): string {
  const a = app().assets[assetID]
  return a ? a.unitInfo.conventional.unit : String(assetID)
}

function fmt (assetID: number, atoms: number): string {
  return Doc.formatCoinValue(atoms, app().unitInfo(assetID))
}

function amt (assetID: number, atoms: number): string {
  return `${fmt(assetID, atoms)} ${sym(assetID)}`
}

function hms (ms: number, secs?: boolean): string {
  const d = new Date(ms)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}${secs ? ':' + p(d.getSeconds()) : ''}`
}

function logo (assetID: number): string {
  const a = app().assets[assetID]
  return a ? Doc.logoPath(a.symbol) : ''
}

function chip (assetID: number): string {
  return `<span class="qs-chip"><img src="${esc(logo(assetID))}" alt="">${esc(sym(assetID))}</span>`
}

function badge (source: string): string {
  return source === 'mesh' ? '<span class="qs-mesh-badge">MESH</span>' : '<span class="qs-srv-badge">SERVER</span>'
}

function chips (route: Route): string {
  let h = chip(route.from)
  for (const st of route.steps) h += `<span class="qs-hop"><span class="qs-ar">⟶</span>${badge(st.source)}</span>${chip(st.toAsset)}`
  return h
}

/* rate of a step as "1 FROM ≈ x TO". */
function stepRate (st: StepPlan, rate: number): string {
  const conv = app().conventionalRate(st.base, st.quote, rate)
  if (!conv) return '-'
  // conv is quote per base.
  return `1 ${sym(st.base)} ≈ ${Doc.formatFourSigFigs(conv)} ${sym(st.quote)}`
}

function stepTitle (st: StepPlan): string {
  return st.sell ? `Sell ${sym(st.fromAsset)} for ${sym(st.toAsset)}` : `Buy ${sym(st.toAsset)} with ${sym(st.fromAsset)}`
}

function venue (st: StepPlan): string {
  const mkt = `${sym(st.base)}-${sym(st.quote)}`
  return st.source === 'mesh' ? `Tatanka mesh · ${mkt}` : `${st.host} · ${mkt}`
}

function parseAmount (s: string, assetID: number): number {
  const v = parseFloat(s.replace(/,/g, ''))
  if (!isFinite(v) || v <= 0) return 0
  const ui = app().unitInfo(assetID)
  return Math.round(v * ui.conventional.conversionFactor)
}

export default class QuickSwapPage extends BasePage {
  page: Record<string, PageElement>
  assets: SupportedAsset[]
  from: number
  to: number
  tol: number
  quote: Quote | null
  quoteSeq: number
  quoteTimer: number
  pollTimer: number
  tickTimer: number
  run: Run | null
  unloaded: boolean

  constructor (body: HTMLElement) {
    super()
    const page = this.page = Doc.idDescendants(body)
    this.quote = null
    this.quoteSeq = 0
    this.quoteTimer = 0
    this.pollTimer = 0
    this.tickTimer = 0
    this.run = null
    this.unloaded = false
    this.tol = 1
    this.assets = Object.values(app().user.assets).filter(a => a.wallet && !a.token)
    if (this.assets.length < 2) {
      Doc.show(page.qsNoMarkets)
      return
    }
    const has = (s: string) => this.assets.find(a => a.symbol === s)
    this.from = (has('lbc') || this.assets[0]).id
    this.to = (has('dcr') && this.from !== 42 ? 42 : this.assets.find(a => a.id !== this.from)?.id) as number
    for (const sel of [page.fromSel, page.toSel]) {
      for (const a of this.assets) {
        const opt = document.createElement('option')
        opt.value = String(a.id)
        opt.textContent = a.unitInfo.conventional.unit
        sel.appendChild(opt)
      }
    }
    this.setCoins()
    Doc.bind(page.fromSel, 'change', () => {
      this.from = Number(page.fromSel.value)
      if (this.from === this.to) this.to = this.assets.find(a => a.id !== this.from)?.id as number
      this.setCoins()
      this.requote()
    })
    Doc.bind(page.toSel, 'change', () => {
      this.to = Number(page.toSel.value)
      if (this.from === this.to) this.from = this.assets.find(a => a.id !== this.to)?.id as number
      this.setCoins()
      this.requote()
    })
    Doc.bind(page.flipBtn, 'click', () => {
      [this.from, this.to] = [this.to, this.from]
      this.setCoins()
      this.requote()
    })
    Doc.bind(page.amountIn, 'input', () => this.requote())
    Doc.bind(page.maxBtn, 'click', () => {
      const w = app().assets[this.from]?.wallet
      if (!w) return
      const ui = app().unitInfo(this.from)
      // Keep a little for network fees.
      const v = Math.max(0, w.balance.available - ui.conventional.conversionFactor / 100);
      (page.amountIn as HTMLInputElement).value = String(v / ui.conventional.conversionFactor)
      this.requote()
    })
    for (const b of Doc.applySelector(page.qsFormView, '.qs-opt')) {
      Doc.bind(b, 'click', () => {
        for (const o of Doc.applySelector(page.qsFormView, '.qs-opt')) o.classList.toggle('on', o === b)
        const t = b.dataset.tol as string
        if (t === 'custom') {
          Doc.show(page.tolCustom);
          (page.tolCustom as HTMLInputElement).focus()
          return
        }
        Doc.hide(page.tolCustom)
        this.tol = Number(t)
        this.requote()
      })
    }
    Doc.bind(page.tolCustom, 'input', () => {
      const v = parseFloat((page.tolCustom as HTMLInputElement).value)
      if (isFinite(v) && v >= 0 && v <= 10) {
        this.tol = v
        this.requote()
      }
    })
    Doc.bind(page.swapBtn, 'click', () => this.start())
    Doc.bind(page.stopBtn, 'click', () => this.stop())
    Doc.bind(page.newSwapBtn, 'click', () => this.showForm())
    Doc.bind(page.tradeBtn, 'click', () => {
      const r = this.run
      if (!r) return
      const st = r.steps[Math.min(r.step, r.steps.length - 1)].plan
      app().loadPage('markets', { host: st.source === 'mesh' ? MeshHost : st.host, baseID: st.base, quoteID: st.quote })
    })
    this.tickTimer = window.setInterval(() => this.tick(), 250)
    this.init()
  }

  async init () {
    // Resume a running swap, if any.
    const res = await postJSON('/api/quickswap/status', {})
    if (this.unloaded) return
    const runs: Run[] = (res && res.ok && res.runs) || []
    const running = runs.find(r => r.status === 'running')
    if (running) {
      this.showRun(running)
      return
    }
    this.showPast(runs)
    this.showForm()
  }

  unload () {
    this.unloaded = true
    window.clearTimeout(this.quoteTimer)
    window.clearTimeout(this.pollTimer)
    window.clearInterval(this.tickTimer)
  }

  setCoins () {
    const page = this.page;
    page.fromSel.value = String(this.from);
    page.toSel.value = String(this.to)
    page.fromLogo.setAttribute('src', logo(this.from))
    page.toLogo.setAttribute('src', logo(this.to))
    const bal = (id: number) => {
      const w = app().assets[id]?.wallet
      return w ? `Balance: ${amt(id, w.balance.available)}` : 'no wallet'
    }
    page.fromBal.textContent = bal(this.from)
    page.toBal.textContent = bal(this.to)
  }

  showForm () {
    Doc.hide(this.page.qsRunView)
    Doc.show(this.page.qsFormView)
    this.run = null
    this.setCoins()
    this.requote()
  }

  showPast (runs: Run[]) {
    const page = this.page
    const done = runs.filter(r => r.status !== 'running').slice(0, 5)
    if (!done.length) {
      page.pastRuns.innerHTML = ''
      return
    }
    let h = '<div class="qs-stephead">Recent Quick Swaps</div>'
    for (const r of done) {
      h += `<div class="qs-kv pointer" data-run="${esc(r.id)}"><span>${hms(r.started)} · ${esc(amt(r.from, r.amount))} → ${esc(sym(r.to))}</span>` +
        `<b>${esc(r.status)}${r.received ? ' · ' + esc(amt(r.to, r.received)) : ''}</b></div>`
    }
    page.pastRuns.innerHTML = h
    for (const el of Doc.applySelector(page.pastRuns, '[data-run]')) {
      Doc.bind(el, 'click', () => {
        const r = done.find(x => x.id === el.dataset.run)
        if (r) this.showRun(r)
      })
    }
  }

  requote () {
    window.clearTimeout(this.quoteTimer)
    this.quoteTimer = window.setTimeout(() => this.fetchQuote(), 350)
  }

  async fetchQuote () {
    const page = this.page
    const amount = parseAmount((page.amountIn as HTMLInputElement).value, this.from)
    const seq = ++this.quoteSeq
    const btn = page.swapBtn as HTMLButtonElement
    Doc.hide(page.startErr)
    if (!amount) {
      this.quote = null
      this.renderQuote(null, '')
      btn.disabled = true
      btn.textContent = 'Enter an amount'
      return
    }
    const res = await postJSON('/api/quickswap/quote', { from: this.from, to: this.to, amount, driftPct: this.tol })
    if (seq !== this.quoteSeq || this.unloaded) return
    if (!res || !res.ok) {
      this.quote = null
      this.renderQuote(null, (res && res.msg) || 'no quote')
      btn.disabled = true
      btn.textContent = 'No route'
      return
    }
    this.quote = res.quote
    this.renderQuote(res.quote, '')
  }

  renderQuote (q: Quote | null, err: string) {
    const page = this.page
    const btn = page.swapBtn as HTMLButtonElement
    Doc.setVis(err, page.quoteErr)
    page.quoteErr.textContent = err.replace(/^quick swap: /, '')
    const r = q && q.route
    if (!r) {
      page.amountOut.textContent = '0'
      page.routeChips.innerHTML = ''
      page.planTl.innerHTML = ''
      page.otherRoutes.innerHTML = ''
      Doc.hide(page.quoteInfo, page.riskNote)
      Doc.show(page.noPlan, page.noOthers)
      return
    }
    page.amountOut.textContent = fmt(r.to, r.out)
    page.routeChips.innerHTML = chips(r) + `<span class="grey ms-auto fs12">best of ${q.routes} route${q.routes === 1 ? '' : 's'}</span>`
    page.estOut.textContent = amt(r.to, r.out)
    // Overall rate: to per from.
    const fromUI = app().unitInfo(r.from)
    const toUI = app().unitInfo(r.to)
    const overall = (r.out / toUI.conventional.conversionFactor) / (r.in / fromUI.conventional.conversionFactor)
    page.estRate.textContent = `1 ${sym(r.from)} ≈ ${Doc.formatFourSigFigs(overall)} ${sym(r.to)}`
    page.estIn.textContent = amt(r.from, r.in) + (r.in < r.amount ? ` (whole lots; ${amt(r.from, r.amount - r.in)} not needed)` : '')
    Doc.setVis(r.leftover > 0, page.leftoverRow)
    if (r.leftover > 0) page.estLeft.textContent = amt(r.steps[0].toAsset, r.leftover)
    page.minOut.textContent = amt(r.to, q.minOut)
    Doc.show(page.quoteInfo)
    Doc.hide(page.noPlan)
    if (r.steps.length > 1) {
      page.riskText.innerHTML = `If step 2 can't complete within your tolerance, you'll keep <b>${esc(sym(r.steps[0].toAsset))}</b> from step 1. It stays in your ${esc(sym(r.steps[0].toAsset))} wallet and nothing is lost.`
    } else {
      page.riskText.innerHTML = `If the orders can't be filled within your tolerance, the swap stops and you keep your <b>${esc(sym(r.from))}</b>. Nothing is lost.`
    }
    Doc.show(page.riskNote)
    // Plan timeline.
    let h = ''
    r.steps.forEach((st, i) => {
      const n = (st.fills || []).length
      h += `<div class="qs-st"><span class="qs-n">${i + 1}</span><div class="qs-t">${esc(stepTitle(st))} ${badge(st.source)}</div>` +
        `<div class="qs-d">${esc(amt(st.fromAsset, st.in))} → ~${esc(amt(st.toAsset, st.out))} at ${esc(stepRate(st, st.avgRate))}<br>` +
        `${esc(venue(st))} · ${n} matching order${n === 1 ? '' : 's'}</div></div>`
    })
    const keep = r.steps.length > 1 ? `or you keep the ${esc(sym(r.steps[0].toAsset))} from step 1` : `or you keep your ${esc(sym(r.from))}`
    h += `<div class="qs-st"><span class="qs-n">✓</span><div class="qs-t">${esc(sym(r.to))} in your wallet</div>` +
      `<div class="qs-d">At least ${esc(amt(r.to, q.minOut))}, ${keep}.</div></div>`
    page.planTl.innerHTML = h
    const others = q.others || []
    Doc.setVis(!others.length, page.noOthers)
    page.otherRoutes.innerHTML = others.map(o => `<div class="qs-kv"><span>${esc(o.label)}</span><b>${esc(amt(o.to, o.out))}</b></div>`).join('')
    const bal = app().assets[r.from]?.wallet?.balance.available || 0
    if (bal < r.in) {
      btn.disabled = true
      btn.textContent = `Not enough ${sym(r.from)}`
    } else {
      btn.disabled = false
      btn.textContent = `Swap ${fmt(r.from, r.in)} ${sym(r.from)} → ${sym(r.to)}`
    }
  }

  async start () {
    const page = this.page
    const q = this.quote
    if (!q || !q.route) return
    const btn = page.swapBtn as HTMLButtonElement
    btn.disabled = true
    const res = await postJSON('/api/quickswap/start', { from: q.route.from, to: q.route.to, amount: q.route.amount, driftPct: q.driftPct, minOut: q.minOut })
    if (!res || !res.ok) {
      Doc.show(page.startErr)
      page.startErr.textContent = ((res && res.msg) || 'error').replace(/^quick swap: /, '')
      btn.disabled = false
      this.requote()
      return
    }
    this.showRun(res.run)
  }

  async stop () {
    if (!this.run) return
    const res = await postJSON('/api/quickswap/stop', { id: this.run.id })
    if (res && res.ok) this.renderRun(res.run)
  }

  showRun (r: Run) {
    Doc.hide(this.page.qsFormView)
    Doc.show(this.page.qsRunView)
    this.renderRun(r)
    this.poll()
  }

  async poll () {
    window.clearTimeout(this.pollTimer)
    if (!this.run || this.unloaded) return
    const res = await postJSON('/api/quickswap/status', { id: this.run.id })
    if (this.unloaded || !this.run) return
    if (res && res.ok && res.run) this.renderRun(res.run)
    if (this.run && this.run.status === 'running') this.pollTimer = window.setTimeout(() => this.poll(), 1000)
  }

  renderRun (r: Run) {
    this.run = r
    const page = this.page
    const running = r.status === 'running'
    const nSteps = r.steps.length
    page.runTitle.textContent = `${running ? 'Swapping' : 'Swap'} ${fmt(r.from, r.route.in)} ${sym(r.from)} → ${sym(r.to)}`
    const badges: Record<string, [string, string]> = {
      running: ['IN PROGRESS', 'run'], complete: ['COMPLETE', 'ok'], stopped: ['STOPPED', 'warn'], failed: ['FAILED', 'bad']
    }
    const [bt, bc] = badges[r.status] || [r.status, '']
    page.runBadge.textContent = bt
    page.runBadge.className = `qs-badge ${bc}`
    page.runSub.textContent = `Step ${Math.min(r.step + 1, nSteps)} of ${nSteps} · started ${hms(r.started)} · tolerance ${r.driftPct}% · at least ${amt(r.to, r.minOut)}`
    page.runChips.innerHTML = chips(r.route)
    let h = ''
    r.steps.forEach((s, i) => {
      const st = s.plan
      const cls = s.status === 'complete' ? 'done' : s.status === 'running' ? 'cur' : s.status === 'stopped' ? 'stop' : ''
      const mark = s.status === 'complete' ? '✓' : String(i + 1)
      const takes = s.takes || []
      const done = takes.filter(t => t.status === 'complete')
      let right = ''
      if (s.status === 'complete') right = '<span class="qs-ok">complete</span>'
      else if (s.status === 'stopped') right = '<span class="qs-warn">stopped</span>'
      else if (s.status === 'running') {
        const pct = s.target ? Math.min(100, Math.round(s.received / s.target * 100)) : 0
        right = `<span class="qs-cur">in progress · ${pct}% filled</span>`
      } else right = '<span class="grey">waiting</span>'
      h += `<div class="qs-st"><span class="qs-n ${cls}">${mark}</span><div class="qs-t">${esc(stepTitle(st))} ${badge(st.source)}<span class="ms-auto fs12">${right}</span></div>`
      if (s.status === 'pending') {
        h += `<div class="qs-d">~${esc(amt(st.fromAsset, st.in))} → ~${esc(amt(st.toAsset, st.out))} · ${esc(venue(st))}</div></div>`
        return
      }
      const spent = s.spent || st.in
      h += `<div class="qs-d">${esc(amt(st.fromAsset, spent))} → ${esc(amt(st.toAsset, s.received))}${s.status === 'complete' ? '' : ' so far'} · ` +
        `${done.length} swap${done.length === 1 ? '' : 's'} complete${s.ended && s.started ? ' · ' + Math.max(1, Math.round((s.ended - s.started) / 60000)) + ' min' : ''}</div>`
      if (s.note) h += `<div class="qs-ev">${esc(s.note).replace(/^Route adjusted:/, '<b>Route adjusted:</b>')}</div>`
      if (takes.length) {
        // Fill bar of the step's target.
        const tgt = Math.max(s.target, 1)
        const w = (v: number) => Math.min(100, v / tgt * 100).toFixed(1)
        const sw = takes.filter(t => t.status === 'swapping').reduce((a, t) => a + t.out, 0)
        const pr = takes.filter(t => t.status === 'proposing').reduce((a, t) => a + t.out, 0)
        h += `<div class="qs-fill"><div class="a" data-w="${w(s.received)}"></div><div class="b" data-w="${w(sw)}"></div><div class="c" data-w="${w(pr)}"></div></div><div class="qs-legend">`
        takes.forEach(t => {
          const what = t.status === 'complete' ? 'swapped' : t.status === 'swapping' ? 'swapping' : t.status === 'proposing' ? 'waiting for the maker' : `failed: ${t.reason || ''}`
          const repl = t.replacement ? ` · replacement, ${t.worsePct > 0.005 ? t.worsePct.toFixed(2) + '% worse' : t.worsePct < -0.005 ? (-t.worsePct).toFixed(2) + '% better' : 'same price'}` : ''
          h += `<span><i class="lg-${esc(t.status)}"></i>Order ${esc(t.orderID.slice(0, 8))} (${esc(t.ownerName || '')}) · ${esc(amt(st.base, t.qty))} @ ${esc(Doc.formatFourSigFigs(app().conventionalRate(st.base, st.quote, t.rate)))} · ${esc(what)}${esc(repl)}</span>`
        })
        const left = Math.max(0, s.target - s.received - sw - pr)
        if (s.status === 'running' && left > 0) h += `<span><i class="lg-left"></i>${esc(amt(st.toAsset, left))} left</span>`
        h += '</div>'
      }
      if (s.wait && s.status === 'running') {
        if (s.wait.until) {
          h += `<div class="qs-count"><svg class="qs-ring" viewBox="0 0 38 38"><circle cx="19" cy="19" r="16" class="bg"/><circle cx="19" cy="19" r="16" class="fg" data-until="${s.wait.until}" data-start="${s.wait.from || (takes.find(t => t.deadline === s.wait?.until)?.started) || (s.wait.until - 20000)}"/>` +
            `<text x="19" y="23" text-anchor="middle" data-cd="${s.wait.until}"></text></svg><span>${esc(s.wait.msg)}</span></div>`
        } else {
          h += `<div class="qs-count"><span class="qs-spin"></span><span>${esc(s.wait.msg)}</span></div>`
        }
      }
      h += '</div>'
    })
    const last = r.steps[nSteps - 1].plan
    const mid = nSteps > 1 ? sym(r.steps[0].plan.toAsset) : sym(r.from)
    const finalCls = r.status === 'complete' ? 'done' : ''
    let finalTxt = `Expected ${esc(amt(r.to, r.expected))} · guaranteed at least ${esc(amt(r.to, r.minOut))}, or you keep the remaining ${esc(mid)}.`
    if (r.status === 'complete') finalTxt = `Received ${esc(amt(r.to, r.received))} (guaranteed at least ${esc(amt(r.to, r.minOut))}).`
    else if (r.received > 0) finalTxt = `Received ${esc(amt(r.to, r.received))} before the swap stopped.`
    h += `<div class="qs-st"><span class="qs-n ${finalCls}">✓</span><div class="qs-t">${esc(sym(last.toAsset))} in your wallet</div><div class="qs-d">${finalTxt}</div></div>`
    page.runTl.innerHTML = h
    // Inline style attributes are blocked by the CSP; set widths via CSSOM.
    for (const el of Doc.applySelector(page.runTl, '[data-w]')) el.style.width = `${el.dataset.w}%`
    // Final message.
    const kept = (r.kept || []).map(k => amt(k.asset, k.amount)).join(' and ')
    page.runMsg.className = `qs-msg ${r.status}`
    Doc.setVis(!running && r.message, page.runMsg)
    page.runMsg.textContent = (r.message || '') + (kept && r.status === 'complete' ? ` Leftover kept: ${kept}.` : '')
    // Buttons.
    const stopBtn = page.stopBtn as HTMLButtonElement
    Doc.setVis(running, stopBtn)
    Doc.setVis(!running, page.newSwapBtn)
    if (running) {
      if (r.stopRequested) {
        stopBtn.disabled = true
        stopBtn.textContent = 'Stopping…'
      } else {
        stopBtn.disabled = false
        stopBtn.textContent = nSteps > 1 ? `Stop and keep ${mid}` : `Stop (keep your ${mid})`
      }
    }
    // Activity.
    page.runLog.innerHTML = (r.log || []).map(l => `<div class="${esc(l.kind || '')}"><span>${hms(l.time, true)}</span><span>${esc(l.msg)}</span></div>`).join('')
    // Drift.
    let d = ''
    r.steps.forEach((s, i) => {
      if (s.status === 'pending') return
      const p = s.worsePct || 0
      d += `<div class="qs-kv"><span>Step ${i + 1}${(s.takes || []).some(t => t.replacement) ? ' (blended)' : ''}</span><b class="${p > 0.005 ? 'qs-warn' : ''}">${p >= 0 ? '+' : ''}${p.toFixed(2)}%</b></div>`
    })
    const used = r.driftUsedPct || 0
    d += `<div class="qs-kv"><span>Total vs. your ${r.driftPct}% tolerance</span><b class="${used > r.driftPct ? 'qs-bad' : 'qs-okc'}">${used.toFixed(2)}% used</b></div>`
    page.driftRows.innerHTML = d
    page.driftFill.style.width = `${r.driftPct > 0 ? Math.min(100, used / r.driftPct * 100) : 0}%`
    this.tick()
  }

  /* tick updates the countdown rings. */
  tick () {
    const now = Date.now()
    for (const t of Doc.applySelector(this.page.runTl, 'text[data-cd]')) {
      const left = Math.max(0, Math.ceil((Number(t.dataset.cd) - now) / 1000))
      t.textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`
    }
    for (const c of Doc.applySelector(this.page.runTl, 'circle[data-until]')) {
      const until = Number(c.dataset.until)
      const start = Number(c.dataset.start)
      const frac = Math.max(0, Math.min(1, (until - now) / Math.max(until - start, 1)))
      const len = 2 * Math.PI * 16
      c.setAttribute('stroke-dasharray', `${(frac * len).toFixed(1)} ${len.toFixed(1)}`)
    }
  }
}
