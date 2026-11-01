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
