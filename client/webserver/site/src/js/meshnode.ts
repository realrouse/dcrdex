import Doc from './doc'
import { getJSON, postJSON } from './http'
import { showNetDetails, netText } from './netdetails'
import { randomName } from './meshnames'

/* The optional embedded mesh node ("Help the network: run a mesh node"). */

export interface ReachResult {
  peer: string
  name?: string
  ok: boolean
  err?: string
}

export interface NodeStatus {
  running: boolean
  starting: boolean
  error?: string
  peerID?: string
  port?: number
  listen?: string[]
  publicAddr?: string
  ipSource?: string
  portMap?: string
  portMapErr?: string
  meshPeers: number
  meshNodes: number
  clients: number
  mismatch?: string[]
  reach: string
  reachNote?: string
  reachChecked?: number
  reachResults?: ReachResult[]
  startedAt?: number
}

export interface MeshNodeSettings {
  enabled: boolean
  port?: number
  name?: string
  bootstrap?: string[]
  publicIP?: string
}

export interface MeshNodeDefaults {
  port: number
  name: string
  bootstrap: string[] | null
}

export interface MeshNodeInfo {
  available: boolean
  enabled: boolean
  status?: NodeStatus
  settings?: MeshNodeSettings
  defaults?: MeshNodeDefaults
}

export interface BootstrapCheck {
  addr: string
  id?: string
  ok: boolean
  err?: string
  ms?: number
}

export interface Precheck {
  port: number
  portOK: boolean
  portErr?: string
  bootstrap: BootstrapCheck[]
}

export interface NodeForm {
  port: number
  name: string
  bootstrap: string[]
  publicIP: string
}

interface NodeResponse {
  ok: boolean
  node?: MeshNodeInfo
  msg?: string
  requestSuccessful?: boolean
}

export async function fetchMeshNode (): Promise<MeshNodeInfo | null> {
  const res = await getJSON('/api/mesh/node') as NodeResponse
  if (!res || !res.ok || !res.node) return null
  return res.node
}

function el (tag: string, cls?: string, text?: string): HTMLElement {
  const e = document.createElement(tag)
  if (cls) e.className = cls
  if (text !== undefined) e.textContent = text
  return e
}

const methodName: Record<string, string> = { 'upnp': 'UPnP', 'nat-pmp': 'NAT-PMP', 'manual': 'manual', 'observed': 'seen by mesh nodes' }

function btn (cls: string, text: string, id?: string): HTMLButtonElement {
  const b = document.createElement('button')
  b.type = 'button'
  b.className = cls
  b.textContent = text
  if (id) b.id = id
  return b
}

const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))

function short (id: string): string { return id ? '…' + id.slice(-6) : '' }

/* reachGuide is the "make your node reachable" help (port forward, VPS). */
function reachGuide (st: NodeStatus, open: boolean): HTMLElement {
  const d = document.createElement('details')
  d.className = 'mnp-guide fs13 mt-2'
  d.open = open
  d.appendChild(el('summary', 'demi pointer', 'How to make your node reachable'))
  const lan = (st.listen || []).map(a => a.split('/')[2]).filter(ip => ip && ip !== '0.0.0.0' && ip !== '127.0.0.1')
  const ol = el('ol', 'mb-1')
  ol.appendChild(el('li', '', netText(`Port forward: open your router's admin page and forward TCP port ${st.port} to this computer` +
    (lan.length ? ` (${lan.join(', ')})` : '') + '. Or switch on UPnP / NAT-PMP there.')))
  ol.appendChild(el('li', '', netText(`Firewall: allow incoming TCP ${st.port} to bisonw.`)))
  ol.appendChild(el('li', '', netText('No access to the router, or your provider uses carrier-grade NAT? Run the node on a VPS with a public IP, ' +
    `open TCP ${st.port} there, and enter the VPS IP as "Public IP" in the advanced settings.`)))
  ol.appendChild(el('li', '', 'Then press "Retry".'))
  d.appendChild(ol)
  const fails = (st.reachResults || []).filter(r => !r.ok)
  if (fails.length) d.appendChild(el('div', 'grey', netText('Peer answers: ' + fails.map(r => `${r.name || short(r.peer)}: ${r.err || 'failed'}`).join('; '))))
  if (!showNetDetails()) d.appendChild(el('div', 'grey fs12 mt-1', 'Addresses and port numbers are hidden. Turn on "Show network details" on the Mesh page to see them.'))
  return d
}

/*
 * MeshNodePanel is the "Your mesh node" card: a "Start mesh node" button
 * while the node is off, else the node's state with Check again, Change
 * settings and Stop. The button opens the MeshNodeWizard.
 */
export default class MeshNodePanel {
  root: HTMLElement
  info: MeshNodeInfo | null
  timer: number
  busy: boolean
  body: HTMLElement
  errMsg: HTMLElement
  guideOpen: boolean
  wizard: MeshNodeWizard
  onChange?: (info: MeshNodeInfo) => void

  constructor (root: HTMLElement) {
    this.root = root
    this.info = null
    this.timer = 0
    this.busy = false
    this.guideOpen = false
    root.classList.add('mesh-node-panel')
    Doc.empty(root)
    this.errMsg = el('div', 'text-danger fs14 mt-1 d-hide')
    root.appendChild(this.errMsg)
    this.body = el('div', 'mnp-body')
    root.appendChild(this.body)
    this.wizard = new MeshNodeWizard()
    this.wizard.onClose = () => { this.refresh() }
  }

  async start () {
    await this.refresh()
    this.schedule()
  }

  stop () {
    if (this.timer) window.clearTimeout(this.timer)
    this.timer = 0
    this.wizard.close(true)
  }

  schedule () {
    if (this.timer) window.clearTimeout(this.timer)
    const st = this.info?.status
    const fast = st && (st.starting || st.reach === 'checking')
    this.timer = window.setTimeout(async () => {
      await this.refresh()
      this.schedule()
    }, fast ? 1500 : 6000)
  }

  async refresh () {
    if (this.busy) return
    const info = await fetchMeshNode()
    if (!info) return
    this.info = info
    this.render()
  }

  async stopNode () {
    this.busy = true
    Doc.hide(this.errMsg)
    const res = await postJSON('/api/mesh/node', { enabled: false }) as NodeResponse
    this.busy = false
    if (!res || !res.ok || !res.node) {
      this.errMsg.textContent = res && res.msg ? res.msg : 'error stopping the mesh node'
      Doc.show(this.errMsg)
      return
    }
    this.info = res.node
    this.render()
  }

  row (label: string, value: string | HTMLElement, title?: string): HTMLElement {
    const r = el('div', 'mnp-row d-flex fs14')
    r.appendChild(el('span', 'mnp-label grey', label))
    const v = typeof value === 'string' ? el('span', 'mnp-value', value) : value
    if (title) v.title = title
    r.appendChild(v)
    return r
  }

  render () {
    const info = this.info
    if (!info) return
    if (this.onChange) this.onChange(info)
    const body = this.body
    Doc.empty(body)
    if (!info.available) {
      body.appendChild(el('div', 'fs14 grey', 'Running a mesh node is only available on simnet (test network) with the mesh enabled for now.'))
      return
    }
    const st = info.status
    if (!info.enabled) {
      body.appendChild(el('div', 'fs14 grey mb-2', 'Mesh nodes relay orders and messages between bisonw users. Trading does not need one. ' +
        'If you want to help the network, start a node: a short guide explains it, sets it up and checks that other peers can reach it.'))
      const b = btn('go', 'Start mesh node', 'meshNodeStartBtn')
      Doc.bind(b, 'click', () => { this.wizard.open(info, 1) })
      body.appendChild(b)
      return
    }
    const name = info.settings?.name || ''
    if (st && st.error) {
      body.appendChild(el('div', 'text-danger fs14', netText(`The node is not running: ${st.error}`)))
    } else if (!st || (st.starting && !st.running)) {
      body.appendChild(el('div', 'fs14 grey', 'Starting the node and asking the router to open the port…'))
    } else {
      const verdict = el('div', 'mnp-verdict d-flex align-items-center flex-wrap mb-2')
      const pill = el('span', `mnp-pill ${st.reach}`)
      pill.id = 'meshNodeReach'
      pill.textContent = st.reach === 'reachable'
        ? 'Reachable'
        : st.reach === 'unreachable' ? 'Not reachable' : st.reach === 'checking' ? 'Checking…' : 'Reachability unknown'
      verdict.appendChild(pill)
      let summary = ''
      switch (st.reach) {
        case 'reachable': summary = `Peers connected to your node from outside (${(st.reachResults || []).filter(r => r.ok).map(r => r.name || short(r.peer)).join(', ')}).`; break
        case 'unreachable': summary = st.reachNote || 'Other peers can not connect to your node.'; break
        case 'checking': summary = 'Asking other bisonw peers to connect to your node…'; break
        default: summary = st.reachNote || 'Not checked yet.'
      }
      verdict.appendChild(el('span', 'fs14 ms-2', netText(summary)))
      body.appendChild(verdict)
      if (name) body.appendChild(this.row('Name', name))
      const peers = el('span', 'mnp-value')
      peers.id = 'meshNodePeers'
      peers.textContent = `${st.meshPeers} of ${st.meshNodes} mesh nodes, ${st.clients} other connection${st.clients === 1 ? '' : 's'}`
      body.appendChild(this.row('Peers', peers))
      body.appendChild(this.row('Node ID', el('span', 'mnp-value mono user-select-all text-break', st.peerID || '')))
      const net = showNetDetails()
      const hiddenNote = 'hidden (turn on Show network details)'
      body.appendChild(this.row('Port', net ? `TCP ${st.port}` : hiddenNote))
      let mapping = 'not opened'
      if (st.portMapErr) mapping = /no UPnP router found/.test(st.portMapErr) && /NAT-PMP/.test(st.portMapErr) ? 'not opened: no UPnP or NAT-PMP router answered' : 'not opened: ' + st.portMapErr
      if (st.portMap) mapping = `opened with ${methodName[st.portMap] || st.portMap}`
      else if (st.ipSource === 'manual') mapping = 'not used (manual public IP)'
      body.appendChild(this.row('Router', netText(mapping), netText(st.portMapErr || '')))
      const src = methodName[st.ipSource || ''] || st.ipSource || '?'
      body.appendChild(this.row('Public address', st.publicAddr
        ? (net ? `${st.publicAddr} (${src})` : `hidden, ${src} (turn on Show network details)`)
        : 'unknown'))
      const nMis = (st.mismatch || []).length
      if (st.meshPeers === 0 || nMis > 0) {
        const wl = el('div', 'mnp-note fs13 mt-2')
        wl.id = 'meshNodeWhitelist'
        const perm = 'The mesh is permissioned: node operators add new nodes by consensus (2/3 of the nodes). Send them your node ID above.'
        if (nMis > 0 && st.meshPeers > 0) wl.textContent = `Peered with ${st.meshPeers} mesh node${st.meshPeers === 1 ? '' : 's'}; ${nMis} other${nMis === 1 ? '' : 's'} did not accept your node yet (not on their whitelist). ${perm}`
        else if (nMis > 0) wl.textContent = `The mesh nodes did not accept your node yet (${nMis} said: not on our whitelist). ${perm} Until then your node runs on its own.`
        else wl.textContent = 'Not peered with a mesh node yet. The mesh is permissioned: node operators must add your node ID to their whitelist.'
        body.appendChild(wl)
      }
      if (st.reach !== 'reachable') {
        const g = reachGuide(st, this.guideOpen)
        Doc.bind(g, 'toggle', () => { this.guideOpen = (g as HTMLDetailsElement).open })
        body.appendChild(g)
      }
    }
    const btns = el('div', 'd-flex align-items-center flex-wrap gap-2 mt-2')
    const recheck = btn('go small', 'Run checks again', 'meshNodeRecheck')
    Doc.bind(recheck, 'click', () => { this.wizard.open(info, 3) })
    const change = btn('go small', 'Change settings', 'meshNodeSettingsBtn')
    Doc.bind(change, 'click', () => { this.wizard.open(info, 2) })
    const stop = btn('go small', 'Stop node', 'meshNodeStopBtn')
    Doc.bind(stop, 'click', () => { this.stopNode() })
    btns.appendChild(recheck); btns.appendChild(change); btns.appendChild(stop)
    if (st && st.reachChecked) btns.appendChild(el('span', 'fs12 grey ms-2', `last checked ${new Date(st.reachChecked).toLocaleTimeString()}`))
    body.appendChild(btns)
  }
}

type CheckState = 'wait' | 'run' | 'pass' | 'fail' | 'warn' | 'skip'

interface CheckRow {
  row: HTMLElement
  icon: HTMLElement
  detail: HTMLElement
  state: CheckState
}

const checkIcons: Record<CheckState, string> = { wait: '○', run: '…', pass: '✓', fail: '✗', warn: '!', skip: '–' }

/*
 * MeshNodeWizard is the "Start mesh node" guide in its own overlay:
 * 1 what running a node means, 2 settings (name, and under Advanced port,
 * bootstrap mesh nodes and a manual public IP), 3 live checks with pass/fail
 * for each, 4 the result with Retry.
 */
export class MeshNodeWizard {
  overlay: HTMLElement
  box: HTMLElement
  info: MeshNodeInfo | null
  form: NodeForm
  step: number
  runID: number
  checks: Record<string, CheckRow>
  lastStatus: NodeStatus | null
  lastPre: Precheck | null
  failed: string
  onClose?: () => void

  constructor () {
    this.overlay = el('div', 'mnw-overlay d-hide')
    this.overlay.id = 'meshNodeWizard'
    this.box = el('div', 'mnw-box')
    this.overlay.appendChild(this.box)
    document.body.appendChild(this.overlay)
    this.info = null
    this.form = { port: 0, name: '', bootstrap: [], publicIP: '' }
    this.step = 0
    this.runID = 0
    this.checks = {}
    this.lastStatus = null
    this.lastPre = null
    this.failed = ''
  }

  open (info: MeshNodeInfo, step: number) {
    this.info = info
    const set = info.settings || { enabled: false }
    const def = info.defaults || { port: 17300, name: randomName(), bootstrap: [] }
    this.form = {
      port: set.port || def.port || 17300,
      name: set.name || def.name || randomName(),
      bootstrap: set.bootstrap && set.bootstrap.length ? set.bootstrap : [],
      publicIP: set.publicIP || ''
    }
    Doc.show(this.overlay)
    this.show(step)
  }

  /* close hides the wizard. silent (page unload): remove it, no callback. */
  close (silent?: boolean) {
    this.runID++
    if (silent) {
      this.overlay.remove()
      return
    }
    if (this.overlay.classList.contains('d-hide')) return
    Doc.hide(this.overlay)
    if (this.onClose) this.onClose()
  }

  show (step: number) {
    this.step = step
    this.runID++
    Doc.empty(this.box)
    const head = el('div', 'mnw-head d-flex align-items-center justify-content-between mb-2')
    head.appendChild(el('span', 'fs20 demi', 'Start mesh node'))
    const x = btn('mnw-x', '×', 'mnwClose')
    Doc.bind(x, 'click', () => this.close())
    head.appendChild(x)
    this.box.appendChild(head)
    const dots = el('div', 'mnw-steps d-flex mb-3 fs13')
    const names = ['About', 'Settings', 'Checks', 'Result']
    names.forEach((n, i) => {
      const d = el('span', 'mnw-dot' + (i + 1 === step ? ' now' : i + 1 < step ? ' done' : ''), `${i + 1}. ${n}`)
      dots.appendChild(d)
    })
    this.box.appendChild(dots)
    const content = el('div', 'mnw-content')
    content.id = `mnwStep${step}`
    this.box.appendChild(content)
    switch (step) {
      case 1: this.renderAbout(content); break
      case 2: this.renderSettings(content); break
      case 3: this.renderChecks(content); this.runChecks(); break
      case 4: this.renderResult(content); break
    }
  }

  footer (buttons: HTMLElement[]): HTMLElement {
    const f = el('div', 'mnw-foot d-flex justify-content-end gap-2 mt-3')
    for (const b of buttons) f.appendChild(b)
    this.box.appendChild(f)
    return f
  }

  renderAbout (c: HTMLElement) {
    c.appendChild(el('div', 'fs15 mb-2', 'A mesh node relays orders and encrypted swap messages between bisonw users, so the network keeps working without a central server.'))
    const ul = el('ul', 'fs14 mnw-list')
    for (const t of [
      'Trading does not need a node. You can stop it at any time on the Mesh page.',
      'It runs inside bisonw while the app is open, and uses some bandwidth.',
      'Other peers must be able to connect to it. bisonw tries to open the port on your router (UPnP / NAT-PMP); if that fails, you can forward the port or use a VPS.',
      'Peers that connect to your node see your IP address, like for any server.',
      'The test mesh is permissioned for now: your node relays for the network once the node operators add its ID to their whitelist. The checks show where it stands.',
      'It never holds or moves your coins.'
    ]) ul.appendChild(el('li', '', t))
    c.appendChild(ul)
    const cancel = btn('go', 'Cancel', 'mnwCancel')
    Doc.bind(cancel, 'click', () => this.close())
    const next = btn('feature', 'Next', 'mnwNext1')
    Doc.bind(next, 'click', () => this.show(2))
    this.footer([cancel, next])
  }

  renderSettings (c: HTMLElement) {
    c.appendChild(el('div', 'fs14 grey mb-2', 'The defaults work for most people. Network settings are under Advanced.'))
    const nameRow = el('div', 'd-flex align-items-end gap-2')
    c.appendChild(nameRow)
    const nameWrap = el('div', 'flex-grow-1')
    nameRow.appendChild(nameWrap)
    const nl = el('label', 'form-label d-block fs14 mb-1', 'Node name')
    nl.setAttribute('for', 'mnwName')
    nameWrap.appendChild(nl)
    const name = document.createElement('input')
    name.id = 'mnwName'
    name.className = 'mnw-input fs14'
    name.value = this.form.name
    name.maxLength = 32
    nameWrap.appendChild(name)
    const dice = btn('go small mb-1', 'New random name', 'mnwNewName')
    Doc.bind(dice, 'click', () => { name.value = randomName() })
    nameRow.appendChild(dice)
    c.appendChild(el('div', 'fs12 grey mt-1 mb-2', 'Random by default, never your computer name. Shown in this app and the node log; the mesh knows the node by its ID.'))
    const adv = document.createElement('details')
    adv.className = 'mnw-adv mt-2'
    adv.id = 'mnwAdvanced'
    adv.appendChild(el('summary', 'fs14 demi pointer', 'Advanced network settings'))
    const advBody = el('div', 'mt-2')
    adv.appendChild(advBody)
    c.appendChild(adv)
    const add = (label: string, id: string, value: string, hint: string, textarea?: boolean) => {
      const w = el('div', 'mb-2')
      const l = el('label', 'form-label d-block fs14 mb-1', label)
      l.setAttribute('for', id)
      w.appendChild(l)
      const i = document.createElement(textarea ? 'textarea' : 'input') as HTMLInputElement | HTMLTextAreaElement
      i.id = id
      i.className = 'mnw-input fs14 mono'
      i.value = value
      if (textarea) (i as HTMLTextAreaElement).rows = 3
      w.appendChild(i)
      w.appendChild(el('div', 'fs12 grey mt-1', hint))
      advBody.appendChild(w)
      return i
    }
    const port = add('Port (TCP)', 'mnwPort', String(this.form.port), 'Default 17300. If it is taken, pick another one from 1024 to 65535.')
    const defBoot = (this.info?.defaults?.bootstrap || []).join('\n')
    const boot = add('Bootstrap mesh nodes', 'mnwBootstrap', this.form.bootstrap.join('\n'),
      'One address per line (/ip4/…/tcp/…/p2p/<node ID>). Empty: the same mesh nodes the app connects to.', true) as HTMLTextAreaElement
    const nDef = (this.info?.defaults?.bootstrap || []).length
    boot.placeholder = showNetDetails() ? defBoot : `Same as the app (${nDef} mesh node${nDef === 1 ? '' : 's'}; addresses hidden)`
    const pub = add('Public IP (optional)', 'mnwPublicIP', this.form.publicIP, 'Only if you forwarded the port by hand or run bisonw on a VPS. Empty: found automatically.')
    if (this.form.bootstrap.length || this.form.publicIP || (this.form.port && this.form.port !== 17300)) adv.open = true
    const err = el('div', 'text-danger fs14 mt-2 d-hide')
    err.id = 'mnwSettingsErr'
    c.appendChild(err)
    const back = btn('go', 'Back', 'mnwBack2')
    Doc.bind(back, 'click', () => this.show(1))
    const next = btn('feature', 'Start node and run checks', 'mnwStart')
    Doc.bind(next, 'click', () => {
      const p = parseInt(port.value.trim() || '17300')
      if (!(p >= 1024 && p <= 65535)) {
        err.textContent = 'The port must be a number from 1024 to 65535.'
        Doc.show(err)
        return
      }
      this.form = {
        port: p,
        name: name.value.trim() || randomName(),
        bootstrap: boot.value.split(/[\n,]/).map(s => s.trim()).filter(s => s),
        publicIP: pub.value.trim()
      }
      this.show(3)
    })
    this.footer([back, next])
  }

  renderChecks (c: HTMLElement) {
    c.appendChild(el('div', 'fs14 grey mb-2', 'Checking your node. This takes up to a minute.'))
    const list = el('div', 'mnw-checks')
    c.appendChild(list)
    this.checks = {}
    const add = (key: string, label: string) => {
      const row = el('div', 'mnw-check d-flex align-items-start py-1')
      row.id = `mnwCheck-${key}`
      const icon = el('span', 'mnw-icon wait', checkIcons.wait)
      const txt = el('div', 'flex-grow-1')
      txt.appendChild(el('div', 'fs14 demi', label))
      const detail = el('div', 'fs13 grey')
      txt.appendChild(detail)
      row.appendChild(icon)
      row.appendChild(txt)
      list.appendChild(row)
      this.checks[key] = { row, icon, detail, state: 'wait' }
    }
    add('port', 'Port free on this computer')
    add('bootstrap', 'Bootstrap mesh nodes answer')
    add('start', 'Node started')
    add('reach', 'Reachable by other peers (port open)')
    add('whitelist', 'Accepted by the mesh (whitelist)')
    const cancel = btn('go', 'Back to settings', 'mnwBack3')
    Doc.bind(cancel, 'click', () => this.show(2))
    this.footer([cancel])
  }

  set (key: string, state: CheckState, detail: string) {
    const c = this.checks[key]
    if (!c) return
    c.state = state
    c.icon.className = `mnw-icon ${state}`
    c.icon.textContent = checkIcons[state]
    c.detail.textContent = netText(detail)
    c.row.dataset.state = state
  }

  skipRest (keys: string[], why: string) {
    for (const k of keys) if (this.checks[k].state === 'wait' || this.checks[k].state === 'run') this.set(k, 'skip', why)
  }

  async runChecks () {
    const id = this.runID
    const alive = () => id === this.runID
    this.failed = ''
    this.lastStatus = null
    this.lastPre = null
    const f = this.form
    this.set('port', 'run', 'Checking…')
    this.set('bootstrap', 'run', 'Connecting…')
    const pre = await postJSON('/api/mesh/node/precheck', f) as { ok: boolean, check?: Precheck, msg?: string }
    if (!alive()) return
    if (!pre || !pre.ok || !pre.check) {
      const msg = pre && pre.msg ? pre.msg : 'check failed'
      this.set('port', 'fail', msg)
      this.failed = 'settings'
      this.skipRest(['bootstrap', 'start', 'reach', 'whitelist'], 'Not checked: fix the settings first.')
      return this.finish()
    }
    const pc = this.lastPre = pre.check
    if (pc.portOK) this.set('port', 'pass', `TCP ${pc.port} is free.`)
    else this.set('port', 'fail', `${pc.portErr || 'port in use'}. Choose another port under Advanced.`)
    const okB = pc.bootstrap.filter(b => b.ok)
    const descr = (b: BootstrapCheck) => `node ${short(b.id || '')}` + (showNetDetails() ? ` ${b.addr}` : '') + (b.ok ? ` (${b.ms ? b.ms + ' ms' : '<1 ms'})` : `: ${b.err}`)
    if (okB.length === pc.bootstrap.length) this.set('bootstrap', 'pass', `${okB.length} of ${pc.bootstrap.length} answer: ` + pc.bootstrap.map(descr).join('; '))
    else if (okB.length > 0) this.set('bootstrap', 'warn', `${okB.length} of ${pc.bootstrap.length} answer: ` + pc.bootstrap.map(descr).join('; '))
    else this.set('bootstrap', 'fail', 'None answer: ' + pc.bootstrap.map(descr).join('; ') + '. Check the internet connection and the bootstrap addresses.')
    if (!pc.portOK || okB.length === 0) {
      this.failed = !pc.portOK ? 'port' : 'bootstrap'
      this.skipRest(['start', 'reach', 'whitelist'], 'Not started.')
      return this.finish()
    }
    // Save the settings and (re)start the node.
    this.set('start', 'run', 'Starting the node and asking the router to open the port…')
    const t0 = Date.now()
    const res = await postJSON('/api/mesh/node/configure', f) as NodeResponse
    if (!alive()) return
    if (!res || !res.ok || !res.node) {
      this.set('start', 'fail', res && res.msg ? res.msg : 'could not start the node')
      this.failed = 'start'
      this.skipRest(['reach', 'whitelist'], 'Node not running.')
      return this.finish()
    }
    let info: MeshNodeInfo | null = res.node
    let st = info.status
    for (let i = 0; i < 40 && !(st && (st.running || st.error)); i++) {
      await sleep(1500)
      if (!alive()) return
      info = await fetchMeshNode()
      st = info?.status
    }
    if (!st || !st.running) {
      this.set('start', 'fail', st && st.error ? st.error : 'The node did not start within a minute.')
      this.failed = 'start'
      this.skipRest(['reach', 'whitelist'], 'Node not running.')
      this.lastStatus = st || null
      return this.finish()
    }
    this.set('start', 'pass', `Running as ${f.name}, node ID ${short(st.peerID || '')}.`)
    // Reachability: the node checks itself once the mesh client is
    // connected; ask again if no fresh verdict comes.
    this.set('reach', 'run', 'Asking other bisonw peers to connect to your node…')
    // A node that was already running (Retry with the same settings) is
    // not restarted, so ask for a new check. Peers answer one probe per
    // node every 30 s, so a check right after an earlier one gets no
    // answers: then wait and ask again (once).
    const wasRunning = !!(st && st.reachChecked && st.reachChecked < t0)
    let since = t0
    for (let round = 0; round < 2; round++) {
      let asked = false
      for (let i = 0; i < 70; i++) {
        const fresh = st && st.reachChecked && st.reachChecked >= since && st.reach !== 'checking'
        if (fresh) break
        if (!asked && st && st.reach !== 'checking' && (wasRunning || round > 0 || i >= 8)) {
          const wait = st.reachChecked ? st.reachChecked + 31000 - Date.now() : 0
          if (wait > 0) {
            this.set('reach', 'run', `Peers answer one check per 30 s; asking again in ${Math.ceil(wait / 1000)} s…`)
          } else {
            asked = true
            this.set('reach', 'run', 'Asking other bisonw peers to connect to your node…')
            await postJSON('/api/mesh/node/check')
          }
        }
        await sleep(1500)
        if (!alive()) return
        info = await fetchMeshNode()
        st = info?.status
      }
      if (!st || st.reach !== 'unknown' || (st.reachResults || []).length > 0) break
      since = Date.now()
    }
    if (!st) return
    this.lastStatus = st
    const by = (st.reachResults || []).filter(r => r.ok).map(r => r.name || short(r.peer))
    if (st.reach === 'reachable') this.set('reach', 'pass', `Connected from outside by ${by.join(', ')}.`)
    else if (st.reach === 'unreachable') { this.set('reach', 'fail', st.reachNote || 'Other peers can not connect to your node.'); this.failed = 'reach' } else { this.set('reach', 'warn', st.reachNote || 'No verdict yet.'); this.failed = this.failed || 'reach-unknown' }
    // Whitelist: wait a little for the mesh nodes' answers.
    this.set('whitelist', 'run', 'Waiting for the mesh nodes…')
    for (let i = 0; i < 6 && st && st.meshPeers === 0 && (st.mismatch || []).length === 0; i++) {
      await sleep(1500)
      if (!alive()) return
      info = await fetchMeshNode()
      st = info?.status
    }
    if (!st) return
    this.lastStatus = st
    const nMis = (st.mismatch || []).length
    if (st.meshPeers > 0 && nMis === 0) this.set('whitelist', 'pass', `Peered with ${st.meshPeers} of ${st.meshNodes} mesh nodes.`)
    else if (st.meshPeers > 0) this.set('whitelist', 'warn', `Peered with ${st.meshPeers} of ${st.meshNodes} mesh nodes; ${nMis} did not accept the node yet (not on their whitelist).`)
    else if (nMis > 0) this.set('whitelist', 'warn', `Pending: ${nMis} mesh node${nMis === 1 ? '' : 's'} answered "not on our whitelist". Send your node ID ${short(st.peerID || '')} to the node operators; the node runs on its own until then.`)
    else this.set('whitelist', 'warn', 'No mesh node peered yet (permissioned mesh).')
    this.finish()
  }

  async finish () {
    const id = this.runID
    await sleep(1200)
    if (id !== this.runID) return
    this.show(4)
  }

  renderResult (c: HTMLElement) {
    const st = this.lastStatus
    const states = Object.fromEntries(Object.entries(this.checks).map(([k, v]) => [k, v.state]))
    let cls = 'pass'
    let title = 'Your mesh node is running and reachable'
    let text = 'Other peers can connect to it. Thank you for helping the network!'
    if (this.failed === 'settings' || this.failed === 'port' || this.failed === 'bootstrap' || this.failed === 'start') {
      cls = 'fail'
      title = 'The node could not start'
      text = this.failed === 'port'
        ? 'The port is in use. Choose another one under Change settings → Advanced, then Retry.'
        : this.failed === 'bootstrap'
          ? 'No bootstrap mesh node answered. Check your internet connection or the addresses, then Retry.'
          : 'See the failed check below.'
    } else if (this.failed === 'reach') {
      cls = 'fail'
      title = 'Your node is running, but other peers can not reach it'
      text = 'It can not relay for the network like this. Open the port (see below), then Retry.'
    } else if (this.failed === 'reach-unknown') {
      cls = 'warn'
      title = 'Your node is running; reachability is not known yet'
      text = 'No peer answered the check. Retry in a minute.'
    } else if (states.whitelist === 'warn') {
      cls = 'warn'
      title = 'Your node is running and reachable'
      text = 'It waits for the node operators to add it to the whitelist of the permissioned mesh.'
    }
    const v = el('div', `mnw-verdict ${cls} mb-2`)
    v.id = 'mnwVerdict'
    v.appendChild(el('div', 'fs16 demi', title))
    v.appendChild(el('div', 'fs14', text))
    c.appendChild(v)
    const list = el('div', 'mnw-checks')
    for (const k of Object.keys(this.checks)) list.appendChild(this.checks[k].row)
    c.appendChild(list)
    if (st && st.peerID) {
      const idRow = el('div', 'fs13 mt-2')
      idRow.appendChild(el('span', 'grey me-2', 'Node ID'))
      idRow.appendChild(el('span', 'mono user-select-all text-break', st.peerID))
      c.appendChild(idRow)
    }
    if (st && this.failed === 'reach') c.appendChild(reachGuide(st, true))
    const change = btn('go', 'Change settings', 'mnwChange')
    Doc.bind(change, 'click', () => this.show(2))
    const retry = btn('go', 'Retry', 'mnwRetry')
    Doc.bind(retry, 'click', () => this.show(3))
    const done = btn('feature', 'Done', 'mnwDone')
    Doc.bind(done, 'click', () => this.close())
    this.footer(cls === 'pass' ? [change, done] : [change, retry, done])
  }
}
