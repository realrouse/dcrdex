#!/usr/bin/env bash
# SIMNET ONLY. P2 end-to-end check: two bisonw instances on a running
# bisoncraft/mesh test mesh negotiate matches over encrypted inbox topics.
#   - A (maker) posts a 4-lot sell order; B (taker) takes 3 lots
#   - both record an identical agreed match (same fields and agreement hash)
#   - the book shows 1 lot remaining on both sides
#   - B takes 2 more lots: only 1 is available (partial fill, remainder 1 lot)
#     and the filled order leaves both books
#   - a proposal that isn't a whole number of lots is rejected by A
#   - with A offline, a proposal times out on B
set -euo pipefail

BISONW=${BISONW:-/workspace/tatanka-dev/bin/bisonw-tatanka}
MESH_RUN=${MESH_RUN:-/workspace/tatanka-test/run}
RUN=${RUN:-/workspace/tatanka-dev/run/p2}
NODE_A=${NODE_A:-2}; PORT_A=${PORT_A:-12347}
NODE_B=${NODE_B:-3}; PORT_B=${PORT_B:-12348}
WEB_A=127.0.0.1:15781; DBG_A=127.0.0.1:15881
WEB_B=127.0.0.1:15782; DBG_B=127.0.0.1:15882
MARKET=dcr_btc
LOT=100000000
RATE=200000
TIMEOUT=${TIMEOUT:-40}

log() { echo "$(date '+%H:%M:%S') $*"; }
fail() { log "FAIL: $*"; exit 1; }
py() { python3 -c "$1"; }

node_id() { grep -ohE 'Node ID: 12D3KooW[A-Za-z0-9]+' "$MESH_RUN/tatanka-$1"/logs/* | tail -1 | awk '{print $3}'; }
ID_A=$(node_id "$NODE_A"); ID_B=$(node_id "$NODE_B")
[ -n "$ID_A" ] && [ -n "$ID_B" ] || fail "could not read mesh node IDs from $MESH_RUN"
ADDR_A=/ip4/127.0.0.1/tcp/$PORT_A/p2p/$ID_A
ADDR_B=/ip4/127.0.0.1/tcp/$PORT_B/p2p/$ID_B

declare -A PID
cleanup() { for p in "${PID[@]}"; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT
[ -d "$RUN" ] && mv "$RUN" "$RUN.$(date +%Y%m%d-%H%M%S)"   # never delete appdata that may hold unrefunded contracts
mkdir -p "$RUN/a" "$RUN/b"

start() { # name addr web dbg
  TATANKA_SIMNET_ADDR=$2 TATANKA_MESH_DEBUG_ADDR=$4 "$BISONW" --simnet --mesh \
    --appdata "$RUN/$1" --webaddr "$3" --log=debug > "$RUN/$1/console.log" 2>&1 &
  PID[$1]=$!
}
wait_http() { for _ in $(seq 1 "$TIMEOUT"); do curl -sf "$1" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
init() {
  local pw; pw=$(head -c 12 /dev/urandom | base64 | tr -d '/+=')
  printf '%s' "$pw" > "$RUN/$1/.testpw"; chmod 600 "$RUN/$1/.testpw"
  curl -sf -XPOST "http://$2/api/init" -H 'Content-Type: application/json' -d "{\"pass\":\"$pw\"}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert d["ok"], d' || fail "init $1"
}
post() { curl -sf -XPOST "http://$1$2" -d "$3"; }
book() { curl -sf "http://$1/mesh/book?market=$MARKET"; }
remaining() { # dbg orderID -> remaining or "none"
  book "$1" | py "import sys,json; o=[o for o in json.load(sys.stdin)['orders'] if o['id']=='$2']; print(o[0]['remaining'] if o else 'none')"
}
wait_remaining() { # dbg orderID want
  local r; for _ in $(seq 1 "$TIMEOUT"); do r=$(remaining "$1" "$2"); [ "$r" = "$3" ] && return 0; sleep 1; done
  log "remaining on $1 for $2 is $r, want $3"; return 1
}
match() { # dbg matchID -> JSON record or ""
  curl -sf "http://$1/mesh/matches" | py "import sys,json; m=[m for m in json.load(sys.stdin)['matches'] if m['match']['id']=='$2']; print(json.dumps(m[0]) if m else '')"
}
status() { match "$1" "$2" | py 'import sys,json; s=sys.stdin.read().strip(); print(json.loads(s)["status"] if s else "none")'; }
wait_status() { # dbg matchID status
  local s; for _ in $(seq 1 "$TIMEOUT"); do s=$(status "$1" "$2"); [ "$s" = "$3" ] && return 0; sleep 1; done
  log "status on $1 for $2 is $s, want $3"; return 1
}
same_match() { # matchID: compare both sides' agreed records
  local a b; a=$(match "$DBG_A" "$1"); b=$(match "$DBG_B" "$1")
  A="$a" B="$b" py '
import os,json
a,b=json.loads(os.environ["A"]),json.loads(os.environ["B"])
assert a["side"]=="maker" and b["side"]=="taker", (a["side"],b["side"])
assert a["status"]==b["status"]=="agreed", (a["status"],b["status"])
assert a["match"]==b["match"], ("match fields differ", a["match"], b["match"])
assert a["agreementHash"]==b["agreementHash"] and a["agreementHash"], "hash differs"
m=a["match"]
print("match %s qty=%d rate=%d quoteQty=%d makerSell=%s initiator=%s makerLock=%d takerLock=%d hash=%s" % (
  m["id"][:16], m["qty"], m["rate"], m["quoteQty"], m["makerSell"], m["initiator"], m["makerLockTime"], m["takerLockTime"], a["agreementHash"]))
'
}

log "mesh nodes: A -> node-$NODE_A ; B -> node-$NODE_B"
start a "$ADDR_A" "$WEB_A" "$DBG_A"; start b "$ADDR_B" "$WEB_B" "$DBG_B"
wait_http "http://$WEB_A/" || fail "A web"; wait_http "http://$WEB_B/" || fail "B web"
init a "$WEB_A"; init b "$WEB_B"
wait_http "http://$DBG_A/mesh/id" || fail "A debug API"; wait_http "http://$DBG_B/mesh/id" || fail "B debug API"
PEER_A=$(curl -sf "http://$DBG_A/mesh/id" | py 'import sys,json; print(json.load(sys.stdin)["peerID"])')
PEER_B=$(curl -sf "http://$DBG_B/mesh/id" | py 'import sys,json; print(json.load(sys.stdin)["peerID"])')
for n in a b; do
  for _ in $(seq 1 "$TIMEOUT"); do grep -q 'push stream established' "$RUN/$n/console.log" && break; sleep 1; done
  grep -q 'push stream established' "$RUN/$n/console.log" || fail "bisonw $n not connected"
done
log "A (maker) $PEER_A ; B (taker) $PEER_B ; both connected"
post "$DBG_A" /mesh/subscribe "{\"market\":\"$MARKET\"}" >/dev/null
post "$DBG_B" /mesh/subscribe "{\"market\":\"$MARKET\"}" >/dev/null

O1=$(post "$DBG_A" /mesh/order "{\"market\":\"$MARKET\",\"sell\":true,\"qty\":$((4*LOT)),\"lotSize\":$LOT,\"rate\":$RATE,\"lifetimeSec\":600}" \
  | py 'import sys,json; print(json.load(sys.stdin)["id"])')
log "A posted sell 4 lots @ $RATE: order $O1"
wait_remaining "$DBG_B" "$O1" $((4*LOT)) || fail "B doesn't see order 1"
log "B sees order 1 with 4 lots"

T1=$(post "$DBG_B" /mesh/take "{\"market\":\"$MARKET\",\"sell\":false,\"qty\":$((3*LOT)),\"rate\":$RATE,\"lotSize\":$LOT}")
M1=$(echo "$T1" | py 'import sys,json; d=json.load(sys.stdin); assert len(d["proposed"])==1 and d["remain"]==0, d; print(d["proposed"][0])')
log "B take 3 lots -> proposal $M1"
wait_status "$DBG_B" "$M1" agreed || fail "B: match 1 not agreed"
wait_status "$DBG_A" "$M1" agreed || fail "A: match 1 not agreed"
S1=$(same_match "$M1") || fail "match 1 records differ"
log "PASS identical match on A and B: $S1"
match "$DBG_A" "$M1" | python3 -m json.tool > "$RUN/match1-maker-A.json"
match "$DBG_B" "$M1" | python3 -m json.tool > "$RUN/match1-taker-B.json"
echo "$S1" | grep -q "qty=$((3*LOT)) rate=$RATE" || fail "match 1 terms wrong"
M1JSON=$(match "$DBG_B" "$M1"); echo "$M1JSON" | grep -q "\"maker\": \"$PEER_A\"" && echo "$M1JSON" | grep -q "\"taker\": \"$PEER_B\"" || fail "roles wrong"
log "PASS roles: maker=A taker=B, maker initiates"
wait_remaining "$DBG_A" "$O1" $LOT || fail "A book not updated"
wait_remaining "$DBG_B" "$O1" $LOT || fail "B book not updated"
log "PASS book update: order 1 has 1 lot remaining on A and B"

T2=$(post "$DBG_B" /mesh/take "{\"market\":\"$MARKET\",\"sell\":false,\"qty\":$((2*LOT)),\"rate\":$RATE,\"lotSize\":$LOT}")
M2=$(echo "$T2" | py "import sys,json; d=json.load(sys.stdin); assert len(d['proposed'])==1 and d['remain']==$LOT, d; print(d['proposed'][0])")
log "B take 2 lots -> 1 proposal for the last lot, remainder 1 lot unmatched"
wait_status "$DBG_B" "$M2" agreed || fail "B: match 2 not agreed"
wait_status "$DBG_A" "$M2" agreed || fail "A: match 2 not agreed"
S2=$(same_match "$M2") || fail "match 2 records differ"
echo "$S2" | grep -q "qty=$LOT " || fail "match 2 qty wrong"
log "PASS partial fill: $S2"
wait_remaining "$DBG_A" "$O1" none || fail "filled order still on A's book"
wait_remaining "$DBG_B" "$O1" none || fail "filled order still on B's book"
log "PASS filled order 1 removed from both books"

O2=$(post "$DBG_A" /mesh/order "{\"market\":\"$MARKET\",\"sell\":true,\"qty\":$((2*LOT)),\"lotSize\":$LOT,\"rate\":$RATE,\"lifetimeSec\":600}" \
  | py 'import sys,json; print(json.load(sys.stdin)["id"])')
wait_remaining "$DBG_B" "$O2" $((2*LOT)) || fail "B doesn't see order 2"
M3=$(post "$DBG_B" /mesh/propose "{\"market\":\"$MARKET\",\"orderID\":\"$O2\",\"qty\":$((LOT/2))}" | py 'import sys,json; print(json.load(sys.stdin)["matchID"])')
wait_status "$DBG_B" "$M3" rejected || fail "half-lot proposal not rejected"
wait_status "$DBG_A" "$M3" rejected || fail "A didn't record rejection"
R3=$(match "$DBG_B" "$M3" | py 'import sys,json; print(json.load(sys.stdin)["reason"])')
log "PASS reject: half-lot proposal rejected ($R3)"
wait_remaining "$DBG_B" "$O2" $((2*LOT)) || fail "rejected proposal changed the book"

kill "${PID[a]}"; wait "${PID[a]}" 2>/dev/null || true; unset 'PID[a]'
log "A stopped (maker offline)"
M4=$(post "$DBG_B" /mesh/propose "{\"market\":\"$MARKET\",\"orderID\":\"$O2\",\"qty\":$LOT}" | py 'import sys,json; print(json.load(sys.stdin)["matchID"])')
wait_status "$DBG_B" "$M4" timed_out || fail "proposal to offline maker did not time out"
R4=$(match "$DBG_B" "$M4" | py 'import sys,json; print(json.load(sys.stdin)["reason"])')
log "PASS timeout: proposal to offline maker timed out ($R4)"

log "B matches: $(curl -sf "http://$DBG_B/mesh/matches" | py 'import sys,json; print(", ".join("%s:%s:%d" % (m["match"]["id"][:8], m["status"], m["match"]["qty"]) for m in json.load(sys.stdin)["matches"]))')"
log "P2 E2E PASS"
