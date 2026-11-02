# Tatanka mesh in the Bison Wallet GUI (simnet `--mesh`)

## Pages

- **Trade** (`/markets`) is the normal Bison Wallet markets page. With the mesh
  running, the mesh markets (DCR-BTC, LBC-BTC, LBC-DCR) appear in the market
  list under the host `tatanka-mesh`. The order book, depth chart, order form
  and "Your Orders" for those markets come from the mesh
  (`site/src/js/meshmarkets.ts`):
  - A limit order that crosses other peers' orders takes them at their rates
    (`/api/mesh/take`); any rest is posted as a standing order
    (`/api/mesh/order`, one hour lifetime). A market order only takes.
  - Cancel uses `/api/mesh/cancel`.
  - There is no price history (candles) or server tier limit on mesh markets.
- **Quick Swap** (`/quickswap`, header entry "Quick Swap", shown when the mesh
  is on or a DEX server is added): swap any coin for another. See "Quick Swap"
  below.
- **Mesh** (`/mesh`, header entry "Mesh", shown only when the mesh is on) is
  connectivity only: animated map of you, the mesh nodes and the peers,
  peer/version table, and the list of mesh matches and swaps.

## When the mesh connects

1. App password login: `Core.Login` -> `login()` creates the mesh client
   (`mesh.NewClient`) when `--mesh` is set on simnet.
2. After the wallets are connected and active trades resolved, `Core.Login`
   calls `connectMesh` (`client/core/mesh.go`). It runs `Client.Connect`, which
   subscribes to the inbox, presence and the default markets and starts the
   background loops. Login does not wait for the network.
3. In the background, about 1-2 s later, the client connects to a bootstrap
   mesh node (log: `Mesh connection: node ...`). Peers show up through their
   signed presence heartbeats, and their orders arrive through the market
   topics (peers re-broadcast their orders when someone subscribes).

## Connect overlay

`site/src/js/meshconnect.ts`. On the first page load after login where the user
has the mesh enabled, the app checks `/api/mesh/status`. If the mesh is not yet
connected with online peers, a full-page overlay shows the animated
connections and three steps: connecting to a mesh node, peers found, order
books synced. It closes by itself when done; after 30 s it shows a link to the
Mesh tab. "Continue without waiting" closes it. If the mesh is already
connected (e.g. a page reload) the overlay is not shown.

## Price history (candles)

When a swap completes, each participant signs a `TradeSummary` (market, match
ID, rate, qty, time, maker and taker peer IDs; no addresses) and publishes it
on the market's `orderbook.<mkt>` topic. Every client validates the signature
against the signer's peer ID (the signer must be the maker or the taker),
bounds-checks the values, dedupes by match ID and keeps 30 days of trades in
`<appdata>/<net>/mesh/trades.json`. On subscribing, a client asks online peers
for the last 7 days (replies are capped, paced and rate limited). Candles
(5m, 1h, 24h) and 24h spots are built locally and served by
`POST /api/mesh/candles {market, dur}` and `GET /api/mesh/spots`, which the
markets page chart and market list use. Peers that collude can still publish
fake trades between themselves, so mesh prices are indicative only.

## Running a mesh node (optional)

"Help the network: run a mesh node" is an optional setup wizard step (after
Quick Configuration) and a "Your mesh node" card on the Mesh page. Off by
default; simnet with `--mesh` only for now. The setting is kept in
`<appdata>/simnet/mesh/node.json`, the node key in `mesh/node/p.key`.

- bisonw runs the bisoncraft/mesh node code in-process on its own libp2p host,
  TCP port 17300 (or a free port if that is busy). No metrics/pprof server.
- It asks the router to open the port: UPnP IGD, then NAT-PMP. The public IP is
  the manual one (`TATANKA_MESH_NODE_IP`), else the router's external IP, else
  the IP the mesh nodes observe.
- Reachability: there is no dial-back service on the mesh nodes, so the check
  asks other bisonw peers on the `reach.tatanka-dex` topic to dial the node's
  public address. A dial only counts if the node proves its peer ID. Answers
  are rate limited (one dial per 3 s, one per requester per 30 s); probes for
  private addresses are only answered on simnet. Verdict: reachable (any peer
  connected), not reachable (all failed, or no public address), unknown (no
  answers).
- The mesh is permissioned: nodes only peer with whitelisted nodes and a
  whitelist change needs 2/3 of the nodes. A new node runs, can be reached and
  shows "not on their whitelist" until the operators add its ID. Clients
  (normal bisonw use) need no open ports at all.
- API: `GET /api/mesh/node`, `POST /api/mesh/node {"enabled": bool}`,
  `POST /api/mesh/node/check`.
- Test settings: `TATANKA_MESH_NODE_IP`, `TATANKA_MESH_NODE_PEERS` (extra
  node multiaddrs, dialed and whitelisted), `TATANKA_MESH_NODE_WHITELIST`,
  `TATANKA_MESH_NODE_NOPORTMAP=1`.

## Quick Swap

`client/core/quickswap_route.go` (route finder, pure), `client/core/quickswap.go`
(executor and Core glue), `site/src/js/quickswap.ts`, `site/src/html/quickswap.tmpl`.

- The user picks a "from" coin, a "to" coin, an amount and a drift tolerance
  (0.5%, 1% (default) or custom, at most 10%).
- **Routes.** Every market with wallets for both coins: the mesh markets
  (`DefaultMarkets`) and the markets of connected DEX servers. Direct routes
  and two-step routes through one intermediate coin. Each step walks the
  current book (best price first, whole lots of each order, our own orders
  skipped). Routes are ranked by expected output; ties go to fewer steps, then
  to the one that spends less. In a two-step route, step 1 only buys what step
  2 can use. If the wallet holds less than 0.001 of the intermediate coin, that
  much is kept back for the second swap's fees. Network fees are not in the
  estimate; each swap pays its own.
- **Guaranteed minimum** = expected output x (1 - tolerance).
- **Execution.** The steps run in turn. A mesh step takes the planned orders
  with the normal mesh take flow (`MeshTakeOrder`, then the atomic swap). If
  a take fails (maker says the order is gone, no answer in 20 s, or the swap
  refunds), the step re-walks the current book without the failed orders and
  takes the next best ones, but only if the projected final output of the whole
  route stays at or above the guaranteed minimum. At most 3 replacement rounds
  per step. Otherwise the run stops, and the user keeps what they have: the
  intermediate coin after step 1, or the unspent "from" coin. Swaps already
  agreed always finish (they are atomic). "Stop and keep <coin>" stops taking
  new orders.
- A server step places one immediate (time-in-force now) limit order at the
  worst rate the tolerance still allows, and waits for its matches to finish.
  Not yet tested end to end (the test chains have no DEX server).
- Runs are kept in memory only; after a restart the mesh swaps resume as usual
  (Mesh tab), but the Quick Swap view of them is gone.
- API: `POST /api/quickswap/quote {from, to, amount, driftPct}`,
  `POST /api/quickswap/start {from, to, amount, driftPct, minOut}` (refuses if
  the books no longer give `minOut`), `POST /api/quickswap/status {id}` (no id:
  all runs), `POST /api/quickswap/stop {id}`.

## Privacy (screenshot-safe Mesh page)
- Display names: `--meshname` if given, else a random `adjective-animal-NNNN` name created once and kept in
  `<data dir>/mesh/name`. bisonw never announces the host or user name.
- The Mesh page shows no IP addresses or ports by default: mesh node tooltips show the node ID, the node
  list shows IDs only, and the "Your mesh node" card hides the port, public address and LAN addresses.
  Error texts and peer answers are filtered (`netdetails.ts` `redactNet`). The "Show network details"
  switch at the top (off by default, kept in the browser's localStorage) shows them.

## "Start mesh node" wizard (replaces the node checkbox)
The "Your mesh node" card (Mesh page, and the optional setup step) shows a **Start mesh node** button while
the node is off. It opens a 4-step overlay (meshnode.ts `MeshNodeWizard`):
1. About: what running a node means (relays orders and messages, never holds coins, peers see your IP,
   permissioned test mesh).
2. Settings: node name (random adjective-animal-NNNN default, "New random name"), and under the collapsed
   "Advanced network settings": TCP port (default 17300), bootstrap mesh nodes (empty: the app's list) and
   an optional manual public IP (port forward / VPS; env TATANKA_MESH_NODE_IP still overrides it).
3. Checks, each pass / fail / warning / skipped: port free on this computer, bootstrap mesh nodes accept
   TCP connections (`/api/mesh/node/precheck`), node started (`/api/mesh/node/configure` saves the
   settings and (re)starts the node), reachable by other peers (dial-back probe; waits out the peers'
   30 s per-node probe limit on Retry), accepted by the mesh (whitelist peers / mismatch answers).
4. Result with a verdict, the node ID, the port-forward / VPS guide when not reachable, and Retry,
   Change settings, Done.
While the node runs, the card shows its state with Run checks again, Change settings and Stop node.

## Unfunded makers (Quick Swap stall, P17)
- `SwapConfig.MakerInitTimeout` (default 3 min): a taker that has matched but has not received the
  maker's `swap_init` within it abandons the swap (nothing of the taker's is locked yet) and sends
  `swap_abort`. A maker that cannot fund its contract gives up after `MakerInitWait() - 30 s` (2m30s)
  and sends `swap_abort` with the reason, so an up-to-date maker frees the taker sooner.
- `MakerContractTimeout(asset)`: a maker contract that was announced but never shows up on chain is
  abandoned after 3 expected blocks, at least 10 min (btc 30 min, dcr 15 min, lbc 10 min).
- Quick Swap shows "Waiting for X to lock their coins" with a countdown ring, and on abandonment logs
  the reason and takes the next best order within the drift tolerance.
- Old makers (no `swap_abort`) are handled by the taker timeout alone.

## USDC on Polygon (P18)
- Mesh markets `dcr_usdc.polygon` (lot 1 DCR) and `lbc_usdc.polygon` (lot 5,000 LBC) appear on the
  Trade page under tatanka-mesh like the other mesh markets.
- EVM tokens swap with the token's v0 swap contract. The first trade needs the one-time token
  approval: the Trade page's "Approve USDC.POLYGON" button works for mesh markets, and a mesh swap
  that finds the token unapproved sends the approval itself and retries once it confirms.
- Audits of EVM contracts read value, recipient, secret hash and lock time from the swap contract's
  on-chain state (`AuditContractOnChain`); the counterparty's tx bytes are never trusted.
- Gas: the native Polygon wallet (POL) pays for approval, swap, redeem and refund.
- Simnet test: dex/testing/meshdex/p18-usdc-polygon.sh (needs the dcr and polygon harnesses).
