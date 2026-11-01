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
