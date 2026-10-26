#!/usr/bin/env bash
# SIMNET ONLY. P1 end-to-end check: two bisonw instances on a running
# bisoncraft/mesh test mesh share signed standing orders on orderbook.dcr_btc.
#   - late subscriber receives an order posted before it subscribed
#   - a live order posted afterwards propagates
#   - a cancel propagates
#   - a short-lived order expires on the remote book
# Requires a running mesh (e.g. bisoncraft/mesh testing/harness.sh) whose node
# data dirs are in $MESH_RUN/tatanka-N (to read node IDs from their logs).
set -euo pipefail

BISONW=${BISONW:-/workspace/tatanka-dev/bin/bisonw-tatanka}
MESH_RUN=${MESH_RUN:-/workspace/tatanka-test/run}
RUN=${RUN:-/workspace/tatanka-dev/run/p1}
NODE_A=${NODE_A:-2}; PORT_A=${PORT_A:-12347}
NODE_B=${NODE_B:-3}; PORT_B=${PORT_B:-12348}
WEB_A=127.0.0.1:15771; DBG_A=127.0.0.1:15871
WEB_B=127.0.0.1:15772; DBG_B=127.0.0.1:15872
MARKET=dcr_btc
EXPIRY_SEC=${EXPIRY_SEC:-25}
TIMEOUT=${TIMEOUT:-40}

log() { echo "$(date '+%H:%M:%S') $*"; }
fail() { log "FAIL: $*"; exit 1; }

node_id() { grep -ohE 'Node ID: 12D3KooW[A-Za-z0-9]+' "$MESH_RUN/tatanka-$1"/logs/* | tail -1 | awk '{print $3}'; }
ID_A=$(node_id "$NODE_A"); ID_B=$(node_id "$NODE_B")
[ -n "$ID_A" ] && [ -n "$ID_B" ] || fail "could not read mesh node IDs from $MESH_RUN"
ADDR_A=/ip4/127.0.0.1/tcp/$PORT_A/p2p/$ID_A
ADDR_B=/ip4/127.0.0.1/tcp/$PORT_B/p2p/$ID_B

PIDS=()
cleanup() { for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT

rm -rf "$RUN"; mkdir -p "$RUN/a" "$RUN/b"

start() { # name addr web dbg
  local name=$1 addr=$2 web=$3 dbg=$4
  TATANKA_SIMNET_ADDR=$addr TATANKA_MESH_DEBUG_ADDR=$dbg "$BISONW" --simnet --mesh \
    --appdata "$RUN/$name" --webaddr "$web" --log=debug > "$RUN/$name/console.log" 2>&1 &
  PIDS+=($!)
}

wait_http() { # url
  for _ in $(seq 1 "$TIMEOUT"); do curl -sf "$1" >/dev/null 2>&1 && return 0; sleep 1; done
  return 1
}

init() { # name web
  local pw; pw=$(head -c 12 /dev/urandom | base64 | tr -d '/+=')
  printf '%s' "$pw" > "$RUN/$1/.testpw"; chmod 600 "$RUN/$1/.testpw"
  curl -sf -XPOST "http://$2/api/init" -H 'Content-Type: application/json' -d "{\"pass\":\"$pw\"}" \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["ok"], d' || fail "init $1"
}

book_ids() { curl -sf "http://$1/mesh/book?market=$MARKET" | python3 -c 'import sys,json; print(" ".join(o["id"] for o in json.load(sys.stdin)["orders"]))'; }

wait_has() { # dbg id
  for _ in $(seq 1 "$TIMEOUT"); do [[ " $(book_ids "$1") " == *" $2 "* ]] && return 0; sleep 1; done; return 1
}
wait_not() { # dbg id
  for _ in $(seq 1 "$TIMEOUT"); do [[ " $(book_ids "$1") " != *" $2 "* ]] && return 0; sleep 1; done; return 1
}

post_order() { # dbg sell qty rate lifetime
  curl -sf -XPOST "http://$1/mesh/order" -d "{\"market\":\"$MARKET\",\"sell\":$2,\"qty\":$3,\"rate\":$4,\"lifetimeSec\":$5}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])'
}

log "mesh nodes: A -> node-$NODE_A $ADDR_A ; B -> node-$NODE_B $ADDR_B"
start a "$ADDR_A" "$WEB_A" "$DBG_A"
start b "$ADDR_B" "$WEB_B" "$DBG_B"
wait_http "http://$WEB_A/" || fail "bisonw A web not up"
wait_http "http://$WEB_B/" || fail "bisonw B web not up"
init a "$WEB_A"; init b "$WEB_B"
wait_http "http://$DBG_A/mesh/id" || fail "A debug API not up"
wait_http "http://$DBG_B/mesh/id" || fail "B debug API not up"
PEER_A=$(curl -sf "http://$DBG_A/mesh/id" | python3 -c 'import sys,json; print(json.load(sys.stdin)["peerID"])')
PEER_B=$(curl -sf "http://$DBG_B/mesh/id" | python3 -c 'import sys,json; print(json.load(sys.stdin)["peerID"])')
log "bisonw A peer $PEER_A ; bisonw B peer $PEER_B"
for n in a b; do
  for _ in $(seq 1 "$TIMEOUT"); do grep -q 'push stream established' "$RUN/$n/console.log" && break; sleep 1; done
  grep -q 'push stream established' "$RUN/$n/console.log" || fail "bisonw $n not connected to mesh"
done
log "both bisonw connected to the mesh"

curl -sf -XPOST "http://$DBG_A/mesh/subscribe" -d "{\"market\":\"$MARKET\"}" >/dev/null || fail "A subscribe"
O1=$(post_order "$DBG_A" true 100000000 200000 300) || fail "A post order 1"
log "A posted sell order $O1 (B not subscribed yet)"
sleep 3
curl -sf -XPOST "http://$DBG_B/mesh/subscribe" -d "{\"market\":\"$MARKET\"}" >/dev/null || fail "B subscribe"
wait_has "$DBG_B" "$O1" || fail "late subscriber B did not receive order 1"
OWNER=$(curl -sf "http://$DBG_B/mesh/book?market=$MARKET" | python3 -c "import sys,json; print([o['owner'] for o in json.load(sys.stdin)['orders'] if o['id']=='$O1'][0])")
[ "$OWNER" = "$PEER_A" ] || fail "order 1 owner on B is $OWNER, want $PEER_A"
log "PASS late subscriber: B has order 1 (owner $OWNER, signature verified by B)"

O2=$(post_order "$DBG_A" false 50000000 190000 300) || fail "A post order 2"
wait_has "$DBG_B" "$O2" || fail "B did not receive live order 2"
log "PASS live gossip: B has order 2 $O2"

curl -sf -XPOST "http://$DBG_A/mesh/cancel" -d "{\"id\":\"$O1\"}" >/dev/null || fail "A cancel order 1"
wait_not "$DBG_B" "$O1" || fail "cancel of order 1 did not reach B"
[[ " $(book_ids "$DBG_B") " == *" $O2 "* ]] || fail "order 2 vanished from B"
log "PASS cancel: order 1 removed on B, order 2 still there"

O3=$(post_order "$DBG_A" true 10000000 210000 "$EXPIRY_SEC") || fail "A post order 3"
wait_has "$DBG_B" "$O3" || fail "B did not receive order 3"
log "B has short-lived order 3 $O3 (lifetime ${EXPIRY_SEC}s)"
sleep "$EXPIRY_SEC"
wait_not "$DBG_B" "$O3" || fail "order 3 did not expire on B"
log "PASS expiry: order 3 gone from B after ${EXPIRY_SEC}s"

log "final B book: $(book_ids "$DBG_B")"
log "P1 E2E PASS"
