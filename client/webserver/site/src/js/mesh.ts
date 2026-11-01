import BasePage from './basepage'
import Doc from './doc'
import { getJSON } from './http'
import { app, PageElement, CoreNote, UnitInfo } from './registry'
import MeshViz, { VizPeer, Health } from './meshviz'
import MeshNodePanel from './meshnode'

export interface MeshMarketInfo {
  name: string
  baseID: number
  quoteID: number
  baseSymbol: string
  quoteSymbol: string
  lotSize: number
  testRate: number
  makerLockSec: number
  takerLockSec: number
  subscribed: boolean
}

export interface PeerInfo {
  id: string
  name?: string
  proto: number
  minProto: number
  app?: string
  node?: string
  firstSeen: number
  lastSeen: number
  online: boolean
  seen: string[]
  compatible: boolean
  reason?: string
}

export interface NodeInfo {
  id: string
  addr?: string
  source: string
  connected: boolean
}

export interface MeshStatus {
  enabled: boolean
  connected: boolean
  peerID: string
  name: string
  node: string
  nodes: NodeInfo[] | null
  bootstrap: string[] | null
  peers: PeerInfo[] | null
  protocolVersion: number
  minProtocolVersion: number
  appVersion: string
  markets: MeshMarketInfo[] | null
}

export interface ViewOrder {
  id: string
  base: number
  quote: number
  owner: string
  sell: boolean
  qty: number
  lotSize: number
  rate: number
  stamp: number
  expiry: number
  remaining: number
  ownerName?: string
  own: boolean
  hidden?: string
  warning?: string
}

export interface BookView {
  market: string
  orders: ViewOrder[] | null
  hidden: ViewOrder[] | null
  subscribed: boolean
}

export interface MeshMatch {
  id: string
  market: { base: number, quote: number }
  makerOrderID: string
  maker: string
  taker: string
  makerSell: boolean
  qty: number
  rate: number
  quoteQty: number
  stamp: number
  makerLockTime: number
  takerLockTime: number
}

export interface MatchRecord {
  side: string
  status: string
  reason?: string
  updated: number
  match: MeshMatch
}

export interface SwapView {
  match: MeshMatch
  side: string
  status: string
  reason?: string
  updated: number
  events: { time: number, msg: string }[] | null
  ownLockTime: number
  peerName?: string
  peer: string
}

export interface Activity {
  orders: ViewOrder[] | null
  matches: MatchRecord[] | null
  swaps: SwapView[] | null
}

export interface MeshEvent {
  kind: string
  market?: string
  peer?: string
  orderID?: string
  matchID?: string
  status?: string
  side?: string
  sell?: boolean
  qty?: number
  rate?: number
  own?: boolean
  maker?: string
  taker?: string
  node?: string
  info?: PeerInfo
}

export interface MeshNote extends CoreNote {
  event: MeshEvent
}

const finalSwap = ['complete', 'refunded', 'abandoned']
const failedMatch = ['rejected', 'timed_out']

/* swapStep maps a swap status to the number of finished steps (0-4) for each side. */
export function swapStep (side: string, status: string): number {
  if (status === 'complete') return 4
  if (side === 'maker') {
    switch (status) {
      case 'init_pending': return 0
      case 'init_sent': return 1
      case 'part_audited': return 2
      case 'redeemed': return 3
    }
    return 0
  }
  switch (status) {
    case 'init_audited': return 1
    case 'part_pending': return 1
    case 'part_sent': return 2
  }
  return 0
}

export function stepLabels (side: string): string[] {
  return side === 'maker' ? ['Lock', 'Audit', 'Redeem', 'Done'] : ['Audit', 'Lock', 'Redeem', 'Done']
}

/* shortID shortens a libp2p peer ID for display. */
export function shortID (id: string): string {
  if (!id) return ''
  return '…' + id.slice(-6)
}

export default class MeshPage extends BasePage {
  body: HTMLElement
  page: Record<string, PageElement>
  status: MeshStatus | null
  markets: MeshMarketInfo[]
  viz: MeshViz | null
  activity: Activity | null
  timers: number[]
  pending: Record<string, number>
  seenMatches: Set<string>
  nodePanel: MeshNodePanel

  constructor (body: HTMLElement) {
    super()
    this.body = body
    const page = this.page = Doc.idDescendants(body)
    this.status = null
    this.markets = []
    this.viz = null
    this.activity = null
    this.timers = []
    this.pending = {}
    this.seenMatches = new Set()
    Doc.cleanTemplates(page.swapTmpl)
    this.nodePanel = new MeshNodePanel(page.meshNodePanel)
    this.nodePanel.onChange = info => Doc.setVis(info.available, page.meshNodeCard)
    this.nodePanel.start()

    app().registerNoteFeeder({
      mesh: (n: CoreNote) => { this.handleMeshNote(n as MeshNote) }
    })
    this.init()
  }

  async init () {
    await this.refreshStatus()
    const st = this.status
    if (!st || !st.enabled) {
      Doc.show(this.page.meshDisabled)
      return
    }
    this.viz = new MeshViz(this.page.meshCanvas as HTMLCanvasElement)
    this.viz.onHover = (p: VizPeer | null, x: number, y: number) => this.showTip(p, x, y)
    this.updateViz()
    this.viz.start()
    this.markets = st.markets || []
    await this.refreshActivity()
    this.timers.push(window.setInterval(() => { this.refreshStatus() }, 5000))
    this.timers.push(window.setInterval(() => { this.refreshActivity() }, 7000))
  }

  unload () {
    this.nodePanel.stop()
    for (const t of this.timers) window.clearInterval(t)
    for (const t of Object.values(this.pending)) window.clearTimeout(t)
    if (this.viz) this.viz.stop()
  }

  /* debounce runs f at most every ms milliseconds per key. */
  debounce (key: string, ms: number, f: () => void) {
    if (this.pending[key]) return
    this.pending[key] = window.setTimeout(() => {
      delete this.pending[key]
      f()
    }, ms)
  }

  unitInfo (assetID: number): UnitInfo {
    return app().unitInfo(assetID)
  }

  /* convRate converts a message rate to a conventional rate. */
  convRate (msgRate: number, m: MeshMarketInfo): number {
    const b = this.unitInfo(m.baseID).conventional.conversionFactor
    const q = this.unitInfo(m.quoteID).conventional.conversionFactor
    return msgRate / 1e8 * b / q
  }

  msgRate (conv: number, m: MeshMarketInfo): number {
    const b = this.unitInfo(m.baseID).conventional.conversionFactor
    const q = this.unitInfo(m.quoteID).conventional.conversionFactor
    return Math.round(conv * 1e8 * q / b)
  }

  fmtRate (msgRate: number, m: MeshMarketInfo): string {
    return Doc.formatFourSigFigs(this.convRate(msgRate, m))
  }

  fmtAmt (atoms: number, assetID: number): string {
    return `${Doc.formatCoinValue(atoms, this.unitInfo(assetID))} ${this.unitInfo(assetID).conventional.unit}`
  }

  peerName (id: string): string {
    if (this.status && id === this.status.peerID) return 'you'
    const p = (this.status?.peers || []).find(p => p.id === id)
    return (p && p.name) || shortID(id)
  }

  marketByName (name: string): MeshMarketInfo | undefined {
    return this.markets.find(m => m.name === name)
  }

  async refreshStatus () {
    const res = await getJSON('/api/mesh/status')
    if (!res || !res.ok) return
    this.status = res.status as MeshStatus
    this.renderStatus()
    this.updateViz()
  }

  health (): Health {
    const st = this.status
    if (!st || !st.connected) return 'offline'
    return (st.nodes || []).length >= 2 ? 'good' : 'degraded'
  }

  renderStatus () {
    const { page, status: st } = this
    if (!st) return
    const h = this.health()
    page.healthPill.className = `mesh-pill ${h}`
    page.healthPill.textContent = h === 'good' ? 'connected' : h === 'degraded' ? 'connected, one mesh node' : 'offline'
    page.nodeInfo.textContent = st.connected ? `via mesh node ${shortID(st.node)}` : (st.enabled ? 'connecting…' : '')
    page.selfName.textContent = st.name || 'unnamed'
    page.selfID.textContent = shortID(st.peerID)
    page.selfID.title = st.peerID
    page.protoBadge.textContent = `protocol v${st.protocolVersion}`
    page.protoBadge.title = `mesh trading protocol v${st.protocolVersion} (trades with v${st.minProtocolVersion}+), app ${st.appVersion}`
    const peers = st.peers || []
    const online = peers.filter(p => p.online).length
    page.peerCount.textContent = `${online} peer${online === 1 ? '' : 's'} online, ${(st.nodes || []).length} mesh node${(st.nodes || []).length === 1 ? '' : 's'}`
    Doc.empty(page.peerRows)
    for (const p of peers) {
      const tr = document.createElement('tr')
      if (!p.online) tr.classList.add('offline')
      const ver = p.proto === 0 ? 'unknown' : `v${p.proto}`
      const verCls = p.proto === 0 ? 'unk' : p.compatible ? 'ok' : 'bad'
      tr.appendChild(this.td(p.name || '—'))
      const idTd = this.td(shortID(p.id), 'mono')
      idTd.title = p.id
      tr.appendChild(idTd)
      const vtd = document.createElement('td')
      const badge = document.createElement('span')
      badge.className = `mesh-badge ${verCls}`
      badge.textContent = ver
      badge.title = p.reason || (p.app ? `app ${p.app}` : '')
      vtd.appendChild(badge)
      tr.appendChild(vtd)
      tr.appendChild(this.td(p.node ? shortID(p.node) : '—', 'mono'))
      tr.appendChild(this.td(p.online ? Doc.timeSince(p.lastSeen) + ' ago' : 'offline'))
      page.peerRows.appendChild(tr)
    }
    Doc.setVis(peers.length === 0, page.noPeers)
    page.nodeList.textContent = 'Mesh nodes: ' + ((st.nodes || []).map(n => `${shortID(n.id)}${n.connected ? ' (connected)' : ''}`).join(', ') || 'none')
  }

  td (text: string, cls?: string): HTMLElement {
    const td = document.createElement('td')
    td.textContent = text
    if (cls) td.className = cls
    return td
  }

  updateViz () {
    const { viz, status: st } = this
    if (!viz || !st || !st.enabled) return
    viz.setHealth(this.health())
    viz.setSelf({
      id: st.peerID, kind: 'self', label: st.name || shortID(st.peerID), sub: `you · v${st.protocolVersion}`,
      online: true, node: st.node, title: st.peerID
    })
    const keep = new Set([st.peerID])
    for (const n of st.nodes || []) {
      keep.add(n.id)
      viz.upsert({
        id: n.id, kind: 'node', label: 'node ' + shortID(n.id), sub: n.connected ? 'connected' : n.source,
        online: true, connected: n.connected, title: n.addr || n.id
      })
    }
    for (const p of st.peers || []) {
      keep.add(p.id)
      const incompatible = p.proto > 0 && !p.compatible
      viz.upsert({
        id: p.id, kind: 'peer', label: p.name || shortID(p.id),
        sub: p.proto === 0 ? 'no presence' : incompatible ? `v${p.proto} incompatible` : `v${p.proto}`,
        online: p.online, incompatible, unknownVersion: p.proto === 0, node: p.node,
        title: `${p.id}${p.app ? ' · app ' + p.app : ''}${p.reason ? ' · ' + p.reason : ''}`
      })
    }
    for (const id of Array.from(viz.nodes.keys())) if (!keep.has(id)) viz.remove(id)
  }

  showTip (p: VizPeer | null, x: number, y: number) {
    const tip = this.page.vizTip
    if (!p) { Doc.hide(tip); return }
    tip.textContent = `${p.label}${p.sub ? ' (' + p.sub + ')' : ''}: ${p.title || p.id}`
    tip.style.left = `${Math.min(x + 12, 300)}px`
    tip.style.top = `${y + 12}px`
    Doc.show(tip)
  }

  async refreshActivity () {
    const res = await getJSON('/api/mesh/activity')
    if (!res || !res.ok) return
    this.activity = res.activity as Activity
    this.renderActivity()
  }

  renderActivity () {
    const { page, activity: act, status: st } = this
    if (!act || !st) return
    // Matches without a swap (proposed, accepted, rejected, timed out) and swaps.
    const swaps = new Map<string, SwapView>()
    for (const s of act.swaps || []) swaps.set(s.match.id, s)
    const items: { id: string, stamp: number, rec?: MatchRecord, swap?: SwapView }[] = []
    for (const r of act.matches || []) items.push({ id: r.match.id, stamp: r.match.stamp, rec: r, swap: swaps.get(r.match.id) })
    for (const s of swaps.values()) if (!items.find(i => i.id === s.match.id)) items.push({ id: s.match.id, stamp: s.match.stamp, swap: s })
    items.sort((a, b) => b.stamp - a.stamp)
    Doc.empty(page.swapList)
    for (const it of items.slice(0, 30)) page.swapList.appendChild(this.swapItem(it.id, it.rec, it.swap))
    Doc.setVis(items.length === 0, page.noSwaps)
    // Light up active swaps in the map (also those running before the page loaded).
    if (this.viz) {
      for (const it of items) {
        const mt = (it.swap || it.rec)?.match
        if (!mt || this.seenMatches.has(it.id)) continue
        const status = it.swap ? it.swap.status : it.rec?.status || ''
        if (finalSwap.includes(status) || failedMatch.includes(status)) continue
        this.seenMatches.add(it.id)
        const side = it.swap ? it.swap.side : it.rec?.side || 'taker'
        this.viz.setMatch(it.id, mt.maker, mt.taker, swapStep(side, status), status)
      }
    }
  }

  swapItem (id: string, rec?: MatchRecord, swap?: SwapView): HTMLElement {
    const div = Doc.clone(this.page.swapTmpl)
    const tmpl = Doc.parseTemplate(div)
    const mt = (swap || rec)?.match as MeshMatch
    const st = this.status as MeshStatus
    const weMaker = mt.maker === st.peerID
    const weSell = weMaker ? mt.makerSell : !mt.makerSell
    const m = this.markets.find(mk => mk.baseID === mt.market.base && mk.quoteID === mt.market.quote)
    const side = swap ? swap.side : rec ? rec.side : (weMaker ? 'maker' : 'taker')
    const status = swap ? swap.status : rec ? rec.status : ''
    tmpl.dir.textContent = weSell ? 'Sold' : 'Bought'
    if (!swap || !finalSwap.includes(status)) tmpl.dir.textContent = weSell ? 'Selling' : 'Buying'
    tmpl.dir.className = weSell ? 'demi sellcolor' : 'demi buycolor'
    tmpl.amt.textContent = m ? `${this.fmtAmt(mt.qty, m.baseID)} @ ${this.fmtRate(mt.rate, m)}` : `${mt.qty}`
    tmpl.peer.textContent = swap && swap.peerName ? swap.peerName : this.peerName(weMaker ? mt.taker : mt.maker)
    tmpl.status.textContent = status.replace('_', ' ')
    tmpl.status.className = `mesh-status ${status === 'complete' ? 'ok' : finalSwap.includes(status) || failedMatch.includes(status) ? 'bad' : 'run'}`
    tmpl.status.title = `match ${id} (${side})`
    if (swap) {
      const done = swapStep(side, status)
      const labels = stepLabels(side)
      for (let i = 0; i < 4; i++) {
        const s = document.createElement('span')
        s.className = 'mesh-step' + (i < done ? ' done' : i === done && !finalSwap.includes(status) ? ' now' : '')
        if (status === 'refunded' || status === 'abandoned') s.classList.add('failed')
        s.textContent = labels[i]
        tmpl.steps.appendChild(s)
      }
      const lock = new Date(swap.ownLockTime * 1000)
      let info = ''
      if (status === 'complete') info = 'Swap complete.'
      else if (status === 'refunded') info = 'Your coins were refunded.'
      else if (status === 'abandoned') info = 'Abandoned before any coins were locked.'
      else info = `If the swap stalls, your coins can be refunded after ${lock.toLocaleString()}.`
      const last = (swap.events || [])[(swap.events || []).length - 1]
      if (swap.reason) info += ` ${swap.reason}`
      else if (last && !finalSwap.includes(status)) info += ` Last: ${last.msg}`
      tmpl.info.textContent = info
    } else {
      Doc.hide(tmpl.steps)
      tmpl.info.textContent = rec && rec.reason ? rec.reason : 'Negotiating the match over the encrypted inbox…'
    }
    return div
  }

  handleMeshNote (n: MeshNote) {
    const e = n.event
    if (!e) return
    const viz = this.viz
    const st = this.status
    switch (e.kind) {
      case 'order': {
        const m = e.market ? this.marketByName(e.market) : undefined
        if (viz && e.peer) viz.broadcast(e.peer, m ? m.baseSymbol : 'dcr')
        if (e.own) this.debounce('act', 300, () => this.refreshActivity())
        break
      }
      case 'cancel':
      case 'update':
        if (viz && e.peer) viz.pulse(e.peer)
        if (e.own) this.debounce('act', 300, () => this.refreshActivity())
        break
      case 'match':
        if (viz && e.matchID && e.maker && e.taker) {
          const failed = failedMatch.includes(e.status || '')
          viz.setMatch(e.matchID, e.maker, e.taker, 0, e.status || '', failed)
          this.seenMatches.add(e.matchID)
        }
        this.debounce('act', 200, () => this.refreshActivity())
        break
      case 'swap':
        if (viz && e.matchID && e.maker && e.taker) {
          const step = swapStep(e.side || '', e.status || '')
          const failed = e.status === 'refunded' || e.status === 'abandoned'
          viz.setMatch(e.matchID, e.maker, e.taker, step, (e.status || '').replace('_', ' '), failed)
          this.seenMatches.add(e.matchID)
        }
        this.debounce('act', 200, () => this.refreshActivity())
        break
      case 'peer':
        if (st && e.info) {
          const peers = st.peers || []
          const i = peers.findIndex(p => p.id === e.info?.id)
          if (i >= 0) peers[i] = e.info
          else peers.unshift(e.info)
          st.peers = peers
          this.updateViz()
        }
        this.debounce('status', 1000, () => this.refreshStatus())
        break
      case 'peer_left':
        if (st && e.peer) {
          const p = (st.peers || []).find(p => p.id === e.peer)
          if (p) p.online = false
          this.updateViz()
        }
        this.debounce('status', 1000, () => this.refreshStatus())
        break
      case 'conn':
        this.debounce('status', 200, () => this.refreshStatus())
        break
    }
  }
}
