import Doc from './doc'
import { getJSON, postJSON } from './http'
import { showNetDetails, netText } from './netdetails'

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

export interface MeshNodeInfo {
  available: boolean
  enabled: boolean
  status?: NodeStatus
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

/*
 * MeshNodePanel shows the node toggle, its state, the reachability result and
 * the port forwarding guide. It is used by the setup wizard and the Mesh page.
 */
export default class MeshNodePanel {
  root: HTMLElement
  info: MeshNodeInfo | null
  timer: number
  busy: boolean
  toggle: HTMLInputElement
  body: HTMLElement
  errMsg: HTMLElement
  guideOpen: boolean
  onChange?: (info: MeshNodeInfo) => void

  constructor (root: HTMLElement) {
    this.root = root
    this.info = null
    this.timer = 0
    this.busy = false
    this.guideOpen = false
    root.classList.add('mesh-node-panel')
    Doc.empty(root)
    const head = el('label', 'mnp-toggle d-flex align-items-center pointer')
    this.toggle = document.createElement('input')
    this.toggle.type = 'checkbox'
    this.toggle.className = 'form-check-input me-2 mt-0'
    this.toggle.id = 'meshNodeToggle'
    head.appendChild(this.toggle)
    head.appendChild(el('span', 'demi', 'Run a mesh node on this computer'))
    head.appendChild(el('span', 'grey fs13 ms-2', '(optional, off by default)'))
    root.appendChild(head)
    this.errMsg = el('div', 'text-danger fs14 mt-1 d-hide')
    root.appendChild(this.errMsg)
    this.body = el('div', 'mnp-body mt-2')
    root.appendChild(this.body)
    Doc.bind(this.toggle, 'change', () => { this.setEnabled(this.toggle.checked) })
  }

  async start () {
    await this.refresh()
    this.schedule()
  }

  stop () {
    if (this.timer) window.clearTimeout(this.timer)
    this.timer = 0
  }

  schedule () {
    this.stop()
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

  async setEnabled (on: boolean) {
    this.busy = true
    this.toggle.disabled = true
    Doc.hide(this.errMsg)
    if (on) this.renderStarting()
    const res = await postJSON('/api/mesh/node', { enabled: on }) as NodeResponse
    this.busy = false
    this.toggle.disabled = false
    if (!res || !res.ok || !res.node) {
      this.errMsg.textContent = res && res.msg ? res.msg : 'error changing the mesh node setting'
      Doc.show(this.errMsg)
      this.toggle.checked = !on
      return
    }
    this.info = res.node
    this.render()
    this.schedule()
  }

  async recheck () {
    Doc.hide(this.errMsg)
    const res = await postJSON('/api/mesh/node/check') as NodeResponse
    if (!res || !res.ok || !res.node) {
      this.errMsg.textContent = res && res.msg ? res.msg : 'error starting the check'
      Doc.show(this.errMsg)
      return
    }
    this.info = res.node
    this.render()
    this.schedule()
  }

  renderStarting () {
    Doc.empty(this.body)
    this.body.appendChild(el('div', 'fs14 grey', 'Starting the node and asking the router to open the port…'))
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
    this.toggle.checked = info.enabled
    this.toggle.disabled = !info.available
    if (!info.available) {
      body.appendChild(el('div', 'fs14 grey', 'Running a mesh node is only available on simnet (test network) with the mesh enabled for now.'))
      return
    }
    const st = info.status
    if (!info.enabled || !st) {
      body.appendChild(el('div', 'fs14 grey', 'Mesh nodes relay orders and messages between bisonw users. ' +
        'Trading does not need one: bisonw only makes outgoing connections, so no ports need to be opened. ' +
        'If you switch this on, bisonw runs a node in the background, tries to open its port on your router (UPnP / NAT-PMP) ' +
        'and asks other peers whether they can reach it. Note: the mesh is permissioned for now, so a new node ' +
        'only relays for the network after the node operators add its ID to their whitelist.'))
      return
    }
    if (st.error) {
      body.appendChild(el('div', 'text-danger fs14', netText(`The node is not running: ${st.error}`)))
      return
    }
    if (st.starting && !st.running) {
      this.renderStarting()
      return
    }

    // Reachability verdict first.
    const verdict = el('div', 'mnp-verdict d-flex align-items-center flex-wrap mb-2')
    const pill = el('span', `mnp-pill ${st.reach}`)
    pill.id = 'meshNodeReach'
    pill.textContent = st.reach === 'reachable'
      ? 'Reachable'
      : st.reach === 'unreachable' ? 'Not reachable' : st.reach === 'checking' ? 'Checking…' : 'Reachability unknown'
    verdict.appendChild(pill)
    let summary = ''
    switch (st.reach) {
      case 'reachable': {
        const by = (st.reachResults || []).filter(r => r.ok).map(r => r.name || r.peer.slice(-6))
        summary = `Peers connected to your node from outside (${by.join(', ')}). The node is running.`
        break
      }
      case 'unreachable': summary = st.reachNote || 'Other peers can not connect to your node.'; break
      case 'checking': summary = 'Asking other bisonw peers to connect to your node…'; break
      default: summary = st.reachNote || 'Not checked yet.'
    }
    verdict.appendChild(el('span', 'fs14 ms-2', netText(summary)))
    body.appendChild(verdict)

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
    body.appendChild(this.row('Public address', st.publicAddr
      ? (net ? st.publicAddr : hiddenNote) + ` (${methodName[st.ipSource || ''] || st.ipSource || '?'})`
      : 'unknown'))

    // The permissioned mesh.
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

    const btns = el('div', 'd-flex align-items-center mt-2')
    const recheck = el('button', 'go small', 'Check again')
    recheck.id = 'meshNodeRecheck'
    ;(recheck as HTMLButtonElement).type = 'button'
    ;(recheck as HTMLButtonElement).disabled = st.reach === 'checking'
    Doc.bind(recheck, 'click', (e: Event) => { e.preventDefault(); this.recheck() })
    btns.appendChild(recheck)
    if (st.reachChecked) btns.appendChild(el('span', 'fs12 grey ms-2', `last checked ${new Date(st.reachChecked).toLocaleTimeString()}`))
    body.appendChild(btns)

    if (st.reach !== 'reachable') body.appendChild(this.guide(st))
  }

  guide (st: NodeStatus): HTMLElement {
    const d = document.createElement('details')
    d.className = 'mnp-guide fs13 mt-2'
    d.open = this.guideOpen || st.reach === 'unreachable'
    Doc.bind(d, 'toggle', () => { this.guideOpen = d.open })
    d.appendChild(el('summary', 'demi pointer', 'How to make your node reachable'))
    const lan = (st.listen || []).map(a => a.split('/')[2]).filter(ip => ip && ip !== '0.0.0.0' && ip !== '127.0.0.1')
    const ol = el('ol', 'mb-1')
    const li1 = el('li', '', netText(`Port forward: open your router's admin page and forward TCP port ${st.port} to this computer` +
      (lan.length ? ` (${lan.join(', ')})` : '') + '. Or switch on UPnP / NAT-PMP there and turn the node off and on again.'))
    const li2 = el('li', '', netText(`Firewall: allow incoming TCP ${st.port} to bisonw.`))
    const li3 = el('li', '', netText('No access to the router, or your provider uses carrier-grade NAT? Run the node on a VPS with a public IP instead, ' +
      `open TCP ${st.port} there, and set TATANKA_MESH_NODE_IP to the VPS IP (simnet test setting).`))
    const li4 = el('li', '', 'Then press "Check again".')
    ol.appendChild(li1); ol.appendChild(li2); ol.appendChild(li3); ol.appendChild(li4)
    d.appendChild(ol)
    const fails = (st.reachResults || []).filter(r => !r.ok)
    if (fails.length) {
      d.appendChild(el('div', 'grey', netText('Peer answers: ' + fails.map(r => `${r.name || r.peer.slice(-6)}: ${r.err || 'failed'}`).join('; '))))
    }
    if (!showNetDetails()) {
      d.appendChild(el('div', 'grey fs12 mt-1', 'Addresses and port numbers are hidden. Turn on "Show network details" at the top of the Mesh page to see them.'))
    }
    return d
  }
}
