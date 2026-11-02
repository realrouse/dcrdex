import State from './state'

/*
 * Screenshot-safe mesh views: IP addresses and ports are hidden unless the
 * user turns on "Show network details" (off by default, kept in
 * localStorage).
 */

const netDetailsLK = 'meshShowNetDetails'

export function showNetDetails (): boolean {
  return State.fetchLocal(netDetailsLK) === true
}

export function setShowNetDetails (on: boolean) {
  State.storeLocal(netDetailsLK, on)
}

const hidden = '[hidden]'

const patterns: [RegExp, string][] = [
  // multiaddr parts: /ip4/1.2.3.4, /ip6/..., /dns4/host, /tcp/7565, /udp/...
  [/\/(ip4|ip6|ip6zone|dns|dns4|dns6|dnsaddr)\/[^/\s,;)]+/g, `/$1/${hidden}`],
  [/\/(tcp|udp)\/\d+/g, `/$1/${hidden}`],
  // [v6]:port
  [/\[[0-9a-fA-F:.%]+\](:\d+)?/g, hidden],
  // IPv4 with optional :port
  [/\b\d{1,3}(\.\d{1,3}){3}(:\d{1,5})?\b/g, hidden],
  // IPv6 (at least 4 groups, or containing ::), not clock times like 12:30:15
  [/\b([0-9a-fA-F]{1,4}:){3,7}[0-9a-fA-F]{1,4}\b/g, hidden],
  [/(\b[0-9a-fA-F]{1,4})?::([0-9a-fA-F]{1,4}(:[0-9a-fA-F]{1,4})*)?\b/g, hidden],
  // localhost:port, host.name:port
  [/\b(localhost|[a-zA-Z0-9-]+(\.[a-zA-Z0-9-]+)+):\d{1,5}\b/g, hidden],
  // "port 17300", "TCP 17300"
  [/\b(port|ports|TCP|UDP|tcp|udp)\s+\d{2,5}\b/g, `$1 ${hidden}`]
]

/* redactNet removes IP addresses, host:port pairs and port numbers from s. */
export function redactNet (s: string): string {
  if (!s) return s
  for (const [re, rep] of patterns) s = s.replace(re, rep)
  return s
}

/* netText returns s as is with network details on, else redacted. */
export function netText (s: string): string {
  return showNetDetails() ? s : redactNet(s)
}
