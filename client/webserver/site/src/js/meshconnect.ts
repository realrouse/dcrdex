/*
 * MeshConnectOverlay shows the Tatanka mesh coming up after login: the mesh
 * client is started during login (core.Login -> connectMesh), but connecting
 * to a bootstrap mesh node, finding other peers (signed presence heartbeats)
 * and receiving their standing orders happen in the background over the next
 * seconds. Until they're done, an animated mesh map and a short status list
 * cover the page. It closes itself once the mesh is ready, can be dismissed,
 * and points to the Mesh tab if it takes long. It isn't shown at all when the
 * mesh is already connected with peers (e.g. on a page reload).
 */
import Doc from './doc'
import { getJSON, postJSON } from './http'
import { app } from './registry'
import MeshViz from './meshviz'
import { MeshStatus, BookView, shortID } from './mesh'

const pollMs = 700
const slowMs = 30000 // show the "taking longer" hint
const bookGraceMs = 6000 // after peers are found, wait this long for orders

type StepState = 'wait' | 'run' | 'done'

export default class MeshConnectOverlay {
  root: HTMLElement
  canvas: HTMLCanvasElement
  steps: Record<string, HTMLElement>
  slow: HTMLElement
  title: HTMLElement
  viz: MeshViz | null
  timer: number
  started: number
  peersAt: number
  orders: number
  closed: boolean
  attempt: number

  constructor () {
    this.viz = null
    this.timer = 0
    this.started = Date.now()
    this.peersAt = 0
    this.orders = -1
    this.closed = false
    this.attempt = 0
  }

  async run () {
    const st = await this.status()
    if (!st || !st.enabled) return
    if (st.connected && this.onlinePeers(st) > 0) return // already connected
    this.build()
    this.update(st)
    this.timer = window.setInterval(() => { this.tick() }, pollMs)
  }

  async status (): Promise<MeshStatus | null> {
    const res = await getJSON('/api/mesh/status')
    if (!res || !res.ok) return null
    return res.status as MeshStatus
  }

  onlinePeers (st: MeshStatus): number {
    return (st.peers || []).filter(p => p.online).length
  }

  build () {
    const root = this.root = document.createElement('div')
    root.id = 'meshConnect'
    root.className = 'mesh-connect'
    root.innerHTML = `
      <div class="mesh-connect-card">
        <div class="d-flex align-items-center justify-content-between mb-1">
          <span class="fs20 demi" data-tmpl="title">Connecting to the Tatanka mesh</span>
        </div>
        <div class="mesh-connect-canvas"><canvas aria-label="Mesh connections"></canvas></div>
        <ul class="mesh-connect-steps fs15">
          <li data-step="node" class="run"><span class="mc-dot"></span><span data-txt>Connecting to a mesh node…</span></li>
          <li data-step="peers" class="wait"><span class="mc-dot"></span><span data-txt>Finding peers…</span></li>
          <li data-step="book" class="wait"><span class="mc-dot"></span><span data-txt>Syncing order books…</span></li>
        </ul>
        <div data-tmpl="slow" class="fs14 grey mt-2 d-hide">This is taking longer than usual.
          <a href="/mesh" data-tmpl="meshLink" class="subtlelink">Open the Mesh tab</a> to see the nodes and peers.</div>
        <div class="text-end mt-2"><button data-tmpl="skip" class="mc-skip fs14">Continue without waiting</button></div>
      </div>`
    document.body.appendChild(root)
    this.canvas = root.querySelector('canvas') as HTMLCanvasElement
    this.title = root.querySelector('[data-tmpl=title]') as HTMLElement
    this.slow = root.querySelector('[data-tmpl=slow]') as HTMLElement
    this.steps = {}
    for (const li of Array.from(root.querySelectorAll('[data-step]')) as HTMLElement[]) this.steps[li.dataset.step as string] = li
    Doc.bind(root.querySelector('[data-tmpl=skip]') as HTMLElement, 'click', () => this.close())
    Doc.bind(root.querySelector('[data-tmpl=meshLink]') as HTMLElement, 'click', (e: Event) => {
      e.preventDefault()
      this.close()
      app().loadPage('mesh')
    })
    try {
      this.viz = new MeshViz(this.canvas)
      this.viz.start()
    } catch (e) {
      this.viz = null // no canvas support; the status list still works
    }
  }

  setStep (key: string, state: StepState, text?: string) {
    const li = this.steps[key]
    li.classList.remove('wait', 'run', 'done')
    li.classList.add(state)
    if (text) (li.querySelector('[data-txt]') as HTMLElement).textContent = text
  }

  async tick () {
    if (this.closed) return
    const st = await this.status()
    if (this.closed) return
    if (!st || !st.enabled) { this.close(); return }
    this.update(st)
    if (st.connected && this.peersAt) await this.checkBooks(st)
    if (Date.now() - this.started > slowMs) Doc.show(this.slow)
  }

  update (st: MeshStatus) {
    const peers = this.onlinePeers(st)
    if (st.connected) this.setStep('node', 'done', `Connected to mesh node ${shortID(st.node)}`)
    else this.setStep('node', 'run', `Connecting to a mesh node (${(st.bootstrap || []).length} bootstrap)…`)
    if (st.connected && peers > 0) {
      if (!this.peersAt) this.peersAt = Date.now()
      this.setStep('peers', 'done', `Found ${peers} peer${peers === 1 ? '' : 's'}`)
    } else if (st.connected) this.setStep('peers', 'run', 'Finding peers…')
    this.draw(st)
  }

  async checkBooks (st: MeshStatus) {
    let n = 0
    for (const m of st.markets || []) {
      const res = await postJSON('/api/mesh/book', { market: m.name })
      if (res && res.ok) n += ((res.book as BookView).orders || []).length
    }
    this.orders = n
    if (n > 0) this.setStep('book', 'done', `Order books synced: ${n} order${n === 1 ? '' : 's'}`)
    else if (Date.now() - this.peersAt > bookGraceMs) this.setStep('book', 'done', 'Order books synced: no orders yet')
    else this.setStep('book', 'run', 'Syncing order books…')
    if (this.steps.book.classList.contains('done')) this.finish()
  }

  draw (st: MeshStatus) {
    const viz = this.viz
    if (!viz) return
    const nodes = st.nodes || []
    // While connecting, we're "trying" the known mesh nodes in turn.
    this.attempt++
    const trying = st.connected ? st.node : nodes.length ? nodes[this.attempt % nodes.length].id : ''
    viz.setHealth(!st.connected ? 'offline' : nodes.length >= 2 ? 'good' : 'degraded')
    viz.setSelf({ id: st.peerID, kind: 'self', label: st.name || 'you', sub: st.connected ? 'connected' : 'connecting…', online: true, node: trying })
    for (const n of nodes) {
      viz.upsert({ id: n.id, kind: 'node', label: 'node ' + shortID(n.id), sub: n.connected ? 'connected' : n.id === trying ? 'trying' : n.source, online: true, connected: n.connected })
    }
    for (const p of st.peers || []) {
      if (!p.online) continue
      viz.upsert({ id: p.id, kind: 'peer', label: p.name || shortID(p.id), sub: p.proto ? `v${p.proto}` : '', online: true, node: p.node, incompatible: p.proto > 0 && !p.compatible })
    }
    if (!trying) return
    if (!st.connected) {
      viz.sendParticle(st.peerID, trying, undefined, '#2970ff')
      viz.pulse(trying)
      return
    }
    // Connected: presence heartbeats travel between us and the peers.
    for (const p of st.peers || []) if (p.online) viz.sendParticle(p.id, st.peerID, undefined, '#2dd4bf')
  }

  finish () {
    if (this.closed) return
    this.title.textContent = 'Connected to the Tatanka mesh'
    window.clearInterval(this.timer)
    window.setTimeout(() => this.close(), 900)
  }

  close () {
    if (this.closed) return
    this.closed = true
    window.clearInterval(this.timer)
    this.root.classList.add('mc-fade')
    window.setTimeout(() => {
      if (this.viz) this.viz.stop()
      this.root.remove()
    }, 350)
  }
}
