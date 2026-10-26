#!/usr/bin/env bash
# SIMNET ONLY. P3 end-to-end check: two bisonw instances on a running
# bisoncraft/mesh test mesh agree a DCR/BTC match over the mesh and execute
# the atomic swap on the dcr simnet and btc regtest harnesses.
#   A (maker, sells DCR): dcrwallet trading1 + bitcoind wallet gamma (alpha)
#   B (taker, buys DCR):  dcrwallet trading2 + bitcoind wallet delta (beta)
# Run 1 (redeem): A initiates, B audits and participates, A redeems (reveals
#   the secret), B redeems. Balances change on both chains for both sides.
# Run 2 (refund): A goes silent before redeeming. B refunds after its (short)
#   lock time, A refunds after its own.
# Needs: dex/testing/dcr/harness.sh and dex/testing/btc/harness.sh running, and
# the tatanka-test mesh.
set -euo pipefail

BIN=${BIN:-/workspace/tatanka-dev/bin}
export PATH=$BIN:$PATH
BISONW=${BISONW:-$BIN/bisonw-tatanka}
MESH_RUN=${MESH_RUN:-/workspace/tatanka-test/run}
RUN=${RUN:-/workspace/tatanka-dev/run/p3}
DEXTEST=${DEXTEST:-$HOME/dextest}
DCRCTL=$DEXTEST/dcr/harness-ctl
BTCCTL=$DEXTEST/btc/harness-ctl
NODE_A=${NODE_A:-2}; PORT_A=${PORT_A:-12347}
NODE_B=${NODE_B:-3}; PORT_B=${PORT_B:-12348}
WEB_A=127.0.0.1:15791; DBG_A=127.0.0.1:15891
WEB_B=127.0.0.1:15792; DBG_B=127.0.0.1:15892
MARKET=dcr_btc
LOT=100000000   # 1 DCR
RATE=1000000    # 0.01 BTC per DCR
TIMEOUT=${TIMEOUT:-40}

log() { echo "$(date '+%H:%M:%S') $*"; }
fail() { log "FAIL: $*"; for n in a b; do [ -f "$RUN/$n/console.log" ] && grep -E 'Swap|swap' "$RUN/$n/console.log" | tail -15 | sed "s/^/  [$n] /"; done; exit 1; }
py() { python3 -c "$1"; }

[ -x "$DCRCTL/trading1" ] && [ -x "$BTCCTL/gamma" ] || fail "dcr/btc harnesses are not running"
node_id() { grep -ohE 'Node ID: 12D3KooW[A-Za-z0-9]+' "$MESH_RUN/tatanka-$1"/logs/* | tail -1 | awk '{print $3}'; }
ID_A=$(node_id "$NODE_A"); ID_B=$(node_id "$NODE_B")
[ -n "$ID_A" ] && [ -n "$ID_B" ] || fail "could not read mesh node IDs from $MESH_RUN"
ADDR_A=/ip4/127.0.0.1/tcp/$PORT_A/p2p/$ID_A
ADDR_B=/ip4/127.0.0.1/tcp/$PORT_B/p2p/$ID_B

declare -A PID
cleanup() { for p in "${PID[@]}"; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT
rm -rf "$RUN"; mkdir -p "$RUN/a" "$RUN/b"

start() { # name addr web dbg
  TATANKA_SIMNET_ADDR=$2 TATANKA_MESH_DEBUG_ADDR=$4 "$BISONW" --simnet --mesh \
    --appdata "$RUN/$1" --webaddr "$3" --log=debug > "$RUN/$1/console.log" 2>&1 &
  PID[$1]=$!
}
wait_http() { for _ in $(seq 1 "$TIMEOUT"); do curl -sf "$1" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
init() { # name web
  local pw; pw=$(head -c 12 /dev/urandom | base64 | tr -d '/+=')
  printf '%s' "$pw" > "$RUN/$1/.testpw"; chmod 600 "$RUN/$1/.testpw"
  curl -sf -c "$RUN/$1/cookies" -XPOST "http://$2/api/init" -H 'Content-Type: application/json' -d "{\"pass\":\"$pw\"}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert d["ok"], d' || fail "init $1"
}
conf_json() { # conf file, extra k=v... -> JSON object
  python3 - "$@" <<'PY'
import sys,json
cfg={}
for line in open(sys.argv[1]):
    line=line.strip()
    if line and not line.startswith(('#',';','[')) and '=' in line:
        k,v=line.split('=',1); cfg[k.strip()]=v.strip()
for kv in sys.argv[2:]:
    k,v=kv.split('=',1); cfg[k]=v
print(json.dumps(cfg))
PY
}
new_wallet() { # name web assetID walletType walletPass configJSON
  local pw; pw=$(cat "$RUN/$1/.testpw")
  local body; body=$(PW="$pw" WP="$5" CFG="$6" py "import os,json; print(json.dumps({'assetID':$3,'walletType':'$4','config':json.loads(os.environ['CFG']),'pass':os.environ['WP'],'appPass':os.environ['PW']}))")
  local res; res=$(curl -s -b "$RUN/$1/cookies" -XPOST "http://$2/api/newwallet" -H 'Content-Type: application/json' -d "$body")
  echo "$res" | py 'import sys,json; d=json.load(sys.stdin); assert d.get("ok"), d' || fail "newwallet $1 asset $3: $res"
}
post() { curl -sf -XPOST "http://$1$2" -d "$3"; }
swap() { # dbg matchID -> JSON record or ""
  curl -sf "http://$1/mesh/swaps" | py "import sys,json; m=[m for m in (json.load(sys.stdin)['swaps'] or []) if m['match']['id']=='$2']; print(json.dumps(m[0]) if m else '')"
}
swap_status() { swap "$1" "$2" | py 'import sys,json; s=sys.stdin.read().strip(); print(json.loads(s)["status"] if s else "none")'; }
wait_swap() { # dbg matchID status timeoutSec
  local s; for _ in $(seq 1 "$4"); do s=$(swap_status "$1" "$2"); [ "$s" = "$3" ] && return 0; sleep 1; done
  log "swap on $1 for $2 is $s, want $3"; return 1
}
match_status() {
  curl -sf "http://$1/mesh/matches" | py "import sys,json; m=[m for m in json.load(sys.stdin)['matches'] if m['match']['id']=='$2']; print(m[0]['status'] if m else 'none')"
}
wait_match() { local s; for _ in $(seq 1 "$TIMEOUT"); do s=$(match_status "$1" "$2"); [ "$s" = "$3" ] && return 0; sleep 1; done; return 1; }
dcr_bal() { "$DCRCTL/$1" getbalance | py 'import sys,json; d=json.load(sys.stdin); print("%.8f" % d["cumulativetotal"])'; }
btc_bal() { "$BTCCTL/$1" getbalance; }
balances() { # label
  local a_dcr a_btc b_dcr b_btc
  a_dcr=$(dcr_bal trading1); a_btc=$(btc_bal gamma); b_dcr=$(dcr_bal trading2); b_btc=$(btc_bal delta)
  echo "$1 A_dcr=$a_dcr A_btc=$a_btc B_dcr=$b_dcr B_btc=$b_btc"
}
delta() { # before after -> per-wallet deltas
  B="$1" A="$2" py '
import os
def p(s): return {kv.split("=")[0]:float(kv.split("=")[1]) for kv in s.split()[1:]}
b,a=p(os.environ["B"]),p(os.environ["A"])
print("delta " + " ".join("%s=%+.8f" % (k, a[k]-b[k]) for k in b))'
}
settle() { # mine a few blocks so balances are confirmed
  "$DCRCTL/mine-alpha" 2 >/dev/null 2>&1; "$BTCCTL/mine-alpha" 2 >/dev/null 2>&1; sleep 3
}
MINER_PID=
start_miner() {
  ( while true; do "$DCRCTL/mine-alpha" 1 >/dev/null 2>&1; "$BTCCTL/mine-alpha" 1 >/dev/null 2>&1; sleep 3; done ) > "$RUN/miner.log" 2>&1 &
  MINER_PID=$!; PID[miner]=$MINER_PID
}
show_txs() { # dbg matchID label
  swap "$1" "$2" | L="$3" py '
import sys,json,os,datetime
r=json.load(sys.stdin); m=r["match"]
print("%s %s side=%s status=%s" % (os.environ["L"], m["id"][:16], r["side"], r["status"]))
for k in ("makerSwap","takerSwap","makerRedeem","takerRedeem","makerRefund","takerRefund"):
    if r.get(k): print("  %-12s %s" % (k, r[k]["txid"]))
for e in r["events"]:
    print("  %s %s" % (datetime.datetime.fromtimestamp(e["time"]/1000).strftime("%H:%M:%S"), e["msg"]))'
}
txid_of() { swap "$1" "$2" | py "import sys,json; r=json.load(sys.stdin); c=r.get('$3'); print(c['txid'].split(':')[0] if c else '')"; }
dcr_confs() { dcrctl -C "$DEXTEST/dcr/alpha/alpha-ctl.conf" getrawtransaction "$1" 1 | py 'import sys,json; print(json.load(sys.stdin).get("confirmations",0))'; }
btc_confs() { "$BTCCTL/alpha" getrawtransaction "$1" 1 | py 'import sys,json; print(json.load(sys.stdin).get("confirmations",0))'; }

log "versions: $(dcrd --version | head -1); $(bitcoind -version | head -1)"
log "mesh nodes: A -> node-$NODE_A ; B -> node-$NODE_B"
start a "$ADDR_A" "$WEB_A" "$DBG_A"; start b "$ADDR_B" "$WEB_B" "$DBG_B"
wait_http "http://$WEB_A/" || fail "A web"; wait_http "http://$WEB_B/" || fail "B web"
init a "$WEB_A"; init b "$WEB_B"
wait_http "http://$DBG_A/mesh/id" || fail "A debug API"; wait_http "http://$DBG_B/mesh/id" || fail "B debug API"
for n in a b; do
  for _ in $(seq 1 "$TIMEOUT"); do grep -q 'push stream established' "$RUN/$n/console.log" && break; sleep 1; done
  grep -q 'push stream established' "$RUN/$n/console.log" || fail "bisonw $n not connected"
done
PEER_A=$(curl -sf "http://$DBG_A/mesh/id" | py 'import sys,json; print(json.load(sys.stdin)["peerID"])')
PEER_B=$(curl -sf "http://$DBG_B/mesh/id" | py 'import sys,json; print(json.load(sys.stdin)["peerID"])')
log "A (maker) $PEER_A ; B (taker) $PEER_B ; both on the mesh"

new_wallet a "$WEB_A" 42 dcrwalletRPC abc "$(conf_json "$DEXTEST/dcr/trading1/trading1.conf" account=default)"
new_wallet a "$WEB_A" 0 bitcoindRPC "" "$(conf_json "$DEXTEST/btc/alpha/alpha.conf" walletname=gamma)"
new_wallet b "$WEB_B" 42 dcrwalletRPC abc "$(conf_json "$DEXTEST/dcr/trading2/trading2.conf" account=default)"
new_wallet b "$WEB_B" 0 bitcoindRPC "" "$(conf_json "$DEXTEST/btc/beta/beta.conf" walletname=delta)"
log "wallets: A dcr=trading1 btc=gamma ; B dcr=trading2 btc=delta"

post "$DBG_A" /mesh/subscribe "{\"market\":\"$MARKET\"}" >/dev/null
post "$DBG_B" /mesh/subscribe "{\"market\":\"$MARKET\"}" >/dev/null
start_miner
log "background miner: 1 dcr + 1 btc block every ~3s"

trade() { # lots -> matchID (A sells, B takes)
  local o m
  o=$(post "$DBG_A" /mesh/order "{\"market\":\"$MARKET\",\"sell\":true,\"qty\":$(($1*LOT)),\"lotSize\":$LOT,\"rate\":$RATE,\"lifetimeSec\":600}" \
    | py 'import sys,json; print(json.load(sys.stdin)["id"])')
  for _ in $(seq 1 "$TIMEOUT"); do curl -sf "http://$DBG_B/mesh/book?market=$MARKET" | grep -q "$o" && break; sleep 1; done
  m=$(post "$DBG_B" /mesh/take "{\"market\":\"$MARKET\",\"sell\":false,\"qty\":$(($1*LOT)),\"rate\":$RATE,\"lotSize\":$LOT}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert len(d["proposed"])==1, d; print(d["proposed"][0])')
  wait_match "$DBG_B" "$m" agreed || fail "match $m not agreed on B"
  wait_match "$DBG_A" "$m" agreed || fail "match $m not agreed on A"
  echo "$m"
}

# ---- Run 1: redeem path ----
for d in "$DBG_A" "$DBG_B"; do post "$d" /mesh/config '{"makerLockSec":1200,"takerLockSec":600}' >/dev/null; done
log "balances are harness wallet totals (labels: A_dcr=trading1 A_btc=gamma B_dcr=trading2 B_btc=delta)"
settle
BAL0=$(balances before_run1); log "$BAL0"
M1=$(trade 10)
log "run 1: match $M1 agreed (10 DCR for 0.1 BTC; lock times maker 20m / taker 10m)"
log "receive addresses: $(swap "$DBG_A" "$M1" | py 'import sys,json; m=json.load(sys.stdin)["match"]; print("maker (BTC) %s, taker (DCR) %s" % (m["makerAddr"], m["takerAddr"])); assert m["makerAddr"] and m["takerAddr"]')" 
wait_swap "$DBG_A" "$M1" complete 240 || fail "run 1: maker swap not complete"
wait_swap "$DBG_B" "$M1" complete 60 || fail "run 1: taker swap not complete"
settle
BAL1=$(balances after_run1)
show_txs "$DBG_A" "$M1" "maker A" | tee "$RUN/run1-maker.txt"
show_txs "$DBG_B" "$M1" "taker B" | tee "$RUN/run1-taker.txt"
swap "$DBG_A" "$M1" | python3 -m json.tool > "$RUN/run1-swap-maker-A.json"
swap "$DBG_B" "$M1" | python3 -m json.tool > "$RUN/run1-swap-taker-B.json"
for k in makerSwap takerRedeem; do t=$(txid_of "$DBG_A" "$M1" "$k"); log "on-chain dcr $k $t confirmations=$(dcr_confs "$t")"; done
for k in takerSwap makerRedeem; do t=$(txid_of "$DBG_A" "$M1" "$k"); log "on-chain btc $k $t confirmations=$(btc_confs "$t")"; done
log "$BAL0"; log "$BAL1"
D1=$(delta "$BAL0" "$BAL1"); log "$D1"
D="$D1" py '
import os
d={kv.split("=")[0]:float(kv.split("=")[1]) for kv in os.environ["D"].split()[1:]}
fee=0.01
assert -10-fee < d["A_dcr"] < -10+1e-9, ("A dcr", d)
assert 0.1-fee < d["A_btc"] <= 0.1, ("A btc", d)
assert 10-fee < d["B_dcr"] <= 10, ("B dcr", d)
assert -0.1-fee < d["B_btc"] < -0.1+1e-9, ("B btc", d)
' || fail "run 1 balance changes wrong: $D1"
log "PASS balances: A paid 10 DCR and got 0.1 BTC, B paid 0.1 BTC and got 10 DCR (less tx fees)"
log "PASS run 1 (redeem): both sides complete"

# ---- Run 2: refund path ----
for d in "$DBG_A" "$DBG_B"; do post "$d" /mesh/config '{"makerLockSec":200,"takerLockSec":100}' >/dev/null; done
post "$DBG_A" /mesh/swap/halt '{"stage":"redeem"}' >/dev/null
BAL2=$(balances before_run2); log "$BAL2"
M2=$(trade 5)
log "run 2: match $M2 agreed (5 DCR for 0.05 BTC; lock times maker 200s / taker 100s); A will go silent before redeeming"
wait_swap "$DBG_B" "$M2" part_sent 120 || fail "run 2: taker did not participate"
log "run 2: both contracts on chain; A silent"
wait_swap "$DBG_B" "$M2" refunded 240 || fail "run 2: taker did not refund"
log "run 2: B refunded after its lock time"
wait_swap "$DBG_A" "$M2" refunded 240 || fail "run 2: maker did not refund"
log "run 2: A refunded after its lock time"
post "$DBG_A" /mesh/swap/halt '{"stage":""}' >/dev/null
settle
BAL3=$(balances after_run2)
show_txs "$DBG_A" "$M2" "maker A" | tee "$RUN/run2-maker.txt"
show_txs "$DBG_B" "$M2" "taker B" | tee "$RUN/run2-taker.txt"
swap "$DBG_A" "$M2" | python3 -m json.tool > "$RUN/run2-swap-maker-A.json"
swap "$DBG_B" "$M2" | python3 -m json.tool > "$RUN/run2-swap-taker-B.json"
for k in makerSwap makerRefund; do t=$(txid_of "$DBG_A" "$M2" "$k"); log "on-chain dcr $k $t confirmations=$(dcr_confs "$t")"; done
for k in takerSwap takerRefund; do t=$(txid_of "$DBG_B" "$M2" "$k"); log "on-chain btc $k $t confirmations=$(btc_confs "$t")"; done
log "$BAL2"; log "$BAL3"
D2=$(delta "$BAL2" "$BAL3"); log "$D2 (fees only)"
D="$D2" py '
import os
d={kv.split("=")[0]:float(kv.split("=")[1]) for kv in os.environ["D"].split()[1:]}
assert all(-0.01 < v <= 0 for v in d.values()), d
' || fail "run 2 balances not restored: $D2"
log "PASS run 2 (refund): both sides refunded"
log "P3 E2E PASS"
