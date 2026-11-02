#!/usr/bin/env bash
# SIMNET ONLY. P18 end-to-end check: USDC on Polygon (usdc.polygon, an ERC20
# token) traded over the Tatanka mesh against DCR on market dcr_usdc.polygon.
#   A: dcrwallet trading1 + native polygon wallet (seeded by bisonw) + usdc.polygon
#   B: dcrwallet trading2 + native polygon wallet + usdc.polygon
# Run 1: A sells 2 DCR (maker, initiates DCR), B takes paying 40 USDC (taker,
#   participates on Polygon: token approval + USDC swap contract). A redeems
#   the USDC (secret revealed on Polygon), B finds it and redeems the DCR.
# Run 2: B is the maker and buys 2 DCR with USDC (initiates on Polygon), A takes
#   (participates DCR). B redeems DCR, A finds the secret on DCR and redeems USDC.
# Run 3 (refund): like run 1 with A silent before redeeming: B refunds its USDC
#   contract after its lock time, A refunds its DCR.
# Needs: dex/testing/dcr/harness.sh, dex/testing/polygon/harness.sh, the
# tatanka-test mesh, and geth in PATH.
set -euo pipefail

BIN=${BIN:-/workspace/tatanka-dev/bin}
export PATH=$BIN:$PATH
BISONW=${BISONW:-$BIN/bisonw-tatanka-p18}
MESH_RUN=${MESH_RUN:-/workspace/tatanka-test/run}
RUN=${RUN:-/workspace/tatanka-dev/run/p18}
DEXTEST=${DEXTEST:-$HOME/dextest}
DCRCTL=$DEXTEST/dcr/harness-ctl
POLYCTL=$DEXTEST/polygon/harness-ctl
POLY_WS=ws://127.0.0.1:34983
NODE_A=${NODE_A:-2}; PORT_A=${PORT_A:-12347}
NODE_B=${NODE_B:-3}; PORT_B=${PORT_B:-12348}
WEB_A=127.0.0.1:15781; DBG_A=127.0.0.1:15881
WEB_B=127.0.0.1:15782; DBG_B=127.0.0.1:15882
MARKET=dcr_usdc.polygon
POL=966; USDC=966001
LOT=100000000     # 1 DCR
RATE=20000000     # 20 USDC per DCR (USDC has 6 decimals)
TIMEOUT=${TIMEOUT:-60}

log() { echo "$(date '+%H:%M:%S') $*"; }
fail() { log "FAIL: $*"; for n in a b; do [ -f "$RUN/$n/console.log" ] && grep -E 'Swap|swap|token|approv' "$RUN/$n/console.log" | tail -20 | sed "s/^/  [$n] /"; done; exit 1; }
py() { python3 -c "$1"; }

[ -x "$DCRCTL/trading1" ] || fail "dcr harness is not running"
[ -x "$POLYCTL/sendUSDC" ] || fail "polygon harness is not running"
USDC_ADDR=$(cat "$DEXTEST/polygon/test_usdc_contract_address.txt")
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
api() { # name web path json
  curl -s -b "$RUN/$1/cookies" -c "$RUN/$1/cookies" -XPOST "http://$2$3" -H 'Content-Type: application/json' -d "$4"
}
apiget() { curl -s -b "$RUN/$1/cookies" "http://$2$3"; }
new_wallet() { # name web assetID walletType walletPass configJSON
  local pw; pw=$(cat "$RUN/$1/.testpw")
  local body; body=$(PW="$pw" WP="$5" CFG="$6" py "import os,json; print(json.dumps({'assetID':$3,'walletType':'$4','config':json.loads(os.environ['CFG']),'pass':os.environ['WP'],'appPass':os.environ['PW']}))")
  local res; res=$(api "$1" "$2" /api/newwallet "$body")
  echo "$res" | py 'import sys,json; d=json.load(sys.stdin); assert d.get("ok") or "already have" in d.get("msg",""), d' || fail "newwallet $1 asset $3: $res"
}
wallet() { # name web assetID -> "address available synced"
  apiget "$1" "$2" /api/user | py "import sys,json; a=json.load(sys.stdin)['user']['assets'].get('$3') or {}; w=a.get('wallet') or {}; print(w.get('address',''), (w.get('balance') or {}).get('available',0), w.get('synced',False), w.get('running',False))"
}
wait_wallet() { # name web assetID
  local s; for _ in $(seq 1 "$TIMEOUT"); do s=$(wallet "$1" "$2" "$3"); [ "$(echo "$s" | awk '{print $3}')" = True ] && return 0; sleep 2; done
  fail "wallet $3 on $1 not synced: $s"
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
cat > "$RUN/bal.js" <<JS
function usdcBal(a) { return contract.at("$USDC_ADDR").balanceOf.call(a).toString() }
function polBal(a) { return eth.getBalance(a).toString() }
function rcpt(h) { var r = eth.getTransactionReceipt(h); return r ? JSON.stringify({status: r.status, block: r.blockNumber, gasUsed: r.gasUsed, to: r.to}) : "null" }
JS
usdc_bal() { (cd "$POLYCTL" && ./alpha attach --preload "loadTestToken.js,$RUN/bal.js" --exec "usdcBal(\"$1\")") | tr -d '"' | py 'import sys; print("%.6f" % (int(sys.stdin.read().strip())/1e6))'; }
pol_bal() { (cd "$POLYCTL" && ./alpha attach --preload "$RUN/bal.js" --exec "polBal(\"$1\")") | tr -d '"' | py 'import sys; print("%.9f" % (int(sys.stdin.read().strip())/1e18))'; }
balances() { # label  (on-chain: dcrwallet totals, Polygon token / native balances of the bisonw addresses)
  echo "$1 A_dcr=$(dcr_bal trading1) A_usdc=$(usdc_bal "$EVM_A") A_pol=$(pol_bal "$EVM_A") B_dcr=$(dcr_bal trading2) B_usdc=$(usdc_bal "$EVM_B") B_pol=$(pol_bal "$EVM_B")"
}
delta() { # before after -> per-wallet deltas
  B="$1" A="$2" py '
import os
def p(s): return {kv.split("=")[0]:float(kv.split("=")[1]) for kv in s.split()[1:]}
b,a=p(os.environ["B"]),p(os.environ["A"])
print("delta " + " ".join("%s=%+.8f" % (k, a[k]-b[k]) for k in b))'
}
check() { # deltaLine python-asserts-on-d
  D="$1" py "
import os
d={kv.split('=')[0]:float(kv.split('=')[1]) for kv in os.environ['D'].split()[1:]}
$2"
}
settle() { "$DCRCTL/mine-alpha" 2 >/dev/null 2>&1; sleep 12; }   # polygon --dev mines every 10 s by itself
start_miner() {
  ( while true; do "$DCRCTL/mine-alpha" 1 >/dev/null 2>&1; sleep 3; done ) > "$RUN/miner.log" 2>&1 &
  PID[miner]=$!
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
dcr_confs() { dcrctl -C "$DEXTEST/dcr/alpha/alpha-ctl.conf" getrawtransaction "$1" 1 2>/dev/null | py 'import sys,json; s=sys.stdin.read(); print(json.loads(s).get("confirmations",0) if s.strip() else "0 (mempool)")'; }
poly_receipt() { (cd "$POLYCTL" && ./alpha attach --preload "$RUN/bal.js" --exec "rcpt(\"$1\")") | py 'import sys,json; print(json.loads(sys.stdin.read()))'; }

log "versions: $(dcrd --version | head -1); $(geth version | grep -m1 ^Version)"
log "mesh nodes: A -> node-$NODE_A ; B -> node-$NODE_B ; market $MARKET ; USDC token $USDC_ADDR"
start a "$ADDR_A" "$WEB_A" "$DBG_A"; start b "$ADDR_B" "$WEB_B" "$DBG_B"
wait_http "http://$WEB_A/" || fail "A web"; wait_http "http://$WEB_B/" || fail "B web"
init a "$WEB_A"; init b "$WEB_B"
wait_http "http://$DBG_A/mesh/id" || fail "A debug API"; wait_http "http://$DBG_B/mesh/id" || fail "B debug API"
for n in a b; do
  for _ in $(seq 1 "$TIMEOUT"); do grep -q 'push stream established' "$RUN/$n/console.log" && break; sleep 1; done
  grep -q 'push stream established' "$RUN/$n/console.log" || fail "bisonw $n not connected"
done
log "A and B are on the mesh"

new_wallet a "$WEB_A" 42 dcrwalletRPC abc "$(conf_json "$DEXTEST/dcr/trading1/trading1.conf" account=default)"
new_wallet b "$WEB_B" 42 dcrwalletRPC abc "$(conf_json "$DEXTEST/dcr/trading2/trading2.conf" account=default)"
for n in a:$WEB_A b:$WEB_B; do
  new_wallet "${n%%:*}" "${n#*:}" $POL rpc "" "{\"providers\":\"$POLY_WS\"}"
  new_wallet "${n%%:*}" "${n#*:}" $USDC token "" "{}"
done
for n in a:$WEB_A b:$WEB_B; do for id in 42 $POL $USDC; do wait_wallet "${n%%:*}" "${n#*:}" $id; done; done
EVM_A=$(wallet a "$WEB_A" $POL | awk '{print $1}'); EVM_B=$(wallet b "$WEB_B" $POL | awk '{print $1}')
log "wallets: A dcr=trading1 polygon=$EVM_A ; B dcr=trading2 polygon=$EVM_B"
(cd "$POLYCTL" && ./sendtoaddress "$EVM_A" 10 && ./sendtoaddress "$EVM_B" 10 && ./sendUSDC "$EVM_A" 100 && ./sendUSDC "$EVM_B" 500) >/dev/null
log "funded from the polygon harness: A 10 POL + 100 USDC, B 10 POL + 500 USDC"
for _ in $(seq 1 30); do
  a=$(wallet a "$WEB_A" $USDC | awk '{print $2}'); b=$(wallet b "$WEB_B" $USDC | awk '{print $2}')
  [ "${a:-0}" -gt 0 ] && [ "${b:-0}" -gt 0 ] && break; sleep 2
done
log "bisonw USDC balances (atoms): A $(wallet a "$WEB_A" $USDC | awk '{print $2}') B $(wallet b "$WEB_B" $USDC | awk '{print $2}')"

post "$DBG_A" /mesh/subscribe "{\"market\":\"$MARKET\"}" >/dev/null
post "$DBG_B" /mesh/subscribe "{\"market\":\"$MARKET\"}" >/dev/null
start_miner
log "background miner: 1 dcr block every ~3s (polygon dev chain: 1 block / 10 s)"

trade() { # makerDbg takerDbg makerSells lots -> matchID
  local o m tsell
  [ "$3" = true ] && tsell=false || tsell=true
  o=$(post "$1" /mesh/order "{\"market\":\"$MARKET\",\"sell\":$3,\"qty\":$(($4*LOT)),\"lotSize\":$LOT,\"rate\":$RATE,\"lifetimeSec\":900}" \
    | py 'import sys,json; print(json.load(sys.stdin)["id"])')
  for _ in $(seq 1 "$TIMEOUT"); do curl -sf "http://$2/mesh/book?market=$MARKET" | grep -q "$o" && break; sleep 1; done
  curl -sf "http://$2/mesh/book?market=$MARKET" > "$RUN/book-$o.json"
  m=$(post "$2" /mesh/take "{\"market\":\"$MARKET\",\"sell\":$tsell,\"qty\":$(($4*LOT)),\"rate\":$RATE,\"lotSize\":$LOT}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert len(d["proposed"])==1, d; print(d["proposed"][0])')
  wait_match "$2" "$m" agreed || fail "match $m not agreed on taker"
  wait_match "$1" "$m" agreed || fail "match $m not agreed on maker"
  echo "$m"
}
evidence() { # run matchID makerDbg takerDbg
  show_txs "$3" "$2" "maker" | tee "$RUN/$1-maker.txt"
  show_txs "$4" "$2" "taker" | tee "$RUN/$1-taker.txt"
  swap "$3" "$2" | python3 -m json.tool > "$RUN/$1-swap-maker.json"
  swap "$4" "$2" | python3 -m json.tool > "$RUN/$1-swap-taker.json"
}
onchain() { # run matchID dbg keys...
  local r=$1 m=$2 d=$3; shift 3
  for k in "$@"; do
    t=$(txid_of "$d" "$m" "$k"); [ -n "$t" ] || { log "$r $k: none"; continue; }
    if [[ $t == 0x* ]]; then log "$r on-chain polygon $k $t receipt=$(poly_receipt "$t")"; else log "$r on-chain dcr $k $t confirmations=$(dcr_confs "$t")"; fi
  done
}

for d in "$DBG_A" "$DBG_B"; do post "$d" /mesh/config "{\"market\":\"$MARKET\",\"makerLockSec\":1200,\"takerLockSec\":600}" >/dev/null; done
log "balances are on-chain: dcrwallet totals and the Polygon addresses of the bisonw wallets"

# ---- Run 1: A (maker) sells DCR, B (taker) pays USDC ----
settle; BAL0=$(balances before_run1); log "$BAL0"
M1=$(trade "$DBG_A" "$DBG_B" true 2)
log "run 1: match $M1 agreed (2 DCR for 40 USDC; maker A initiates DCR, taker B approves the token and participates with USDC)"
log "receive addresses: $(swap "$DBG_A" "$M1" | py 'import sys,json; m=json.load(sys.stdin)["match"]; print("maker (USDC) %s, taker (DCR) %s" % (m["makerAddr"], m["takerAddr"])); assert m["makerAddr"] and m["takerAddr"]')"
wait_swap "$DBG_A" "$M1" complete 420 || fail "run 1: maker swap not complete"
wait_swap "$DBG_B" "$M1" complete 180 || fail "run 1: taker swap not complete"
settle; BAL1=$(balances after_run1)
evidence run1 "$M1" "$DBG_A" "$DBG_B"
onchain run1 "$M1" "$DBG_A" makerSwap takerSwap makerRedeem takerRedeem
log "$BAL0"; log "$BAL1"; D1=$(delta "$BAL0" "$BAL1"); log "$D1"
check "$D1" '
assert -2.01 < d["A_dcr"] < -2+1e-9, d
assert abs(d["A_usdc"]-40) < 1e-6, d
assert 2-0.01 < d["B_dcr"] <= 2, d
assert abs(d["B_usdc"]+40) < 1e-6, d
assert -0.1 < d["A_pol"] < 0 and -0.1 < d["B_pol"] < 0, d' || fail "run 1 balance changes wrong: $D1"
log "PASS run 1: A paid 2 DCR and got 40 USDC, B paid 40 USDC and got 2 DCR (POL gas only)"

# ---- Run 2: B (maker) buys DCR paying USDC, A (taker) sells DCR ----
BAL2=$(balances before_run2); log "$BAL2"
M2=$(trade "$DBG_B" "$DBG_A" false 3)
log "run 2: match $M2 agreed (3 DCR for 60 USDC; maker B initiates on Polygon, taker A participates DCR)"
wait_swap "$DBG_B" "$M2" complete 420 || fail "run 2: maker swap not complete"
wait_swap "$DBG_A" "$M2" complete 180 || fail "run 2: taker swap not complete"
settle; BAL3=$(balances after_run2)
evidence run2 "$M2" "$DBG_B" "$DBG_A"
onchain run2 "$M2" "$DBG_B" makerSwap takerSwap makerRedeem takerRedeem
log "$BAL2"; log "$BAL3"; D2=$(delta "$BAL2" "$BAL3"); log "$D2"
check "$D2" '
assert -3.01 < d["A_dcr"] < -3+1e-9, d
assert abs(d["A_usdc"]-60) < 1e-6, d
assert 3-0.01 < d["B_dcr"] <= 3, d
assert abs(d["B_usdc"]+60) < 1e-6, d' || fail "run 2 balance changes wrong: $D2"
log "PASS run 2: B paid 60 USDC and got 3 DCR, A paid 3 DCR and got 60 USDC"

# ---- Run 3: refund path, A (maker, DCR) silent before redeeming ----
for d in "$DBG_A" "$DBG_B"; do post "$d" /mesh/config "{\"market\":\"$MARKET\",\"makerLockSec\":240,\"takerLockSec\":120}" >/dev/null; done
post "$DBG_A" /mesh/swap/halt '{"stage":"redeem"}' >/dev/null
BAL4=$(balances before_run3); log "$BAL4"
M3=$(trade "$DBG_A" "$DBG_B" true 1)
log "run 3: match $M3 agreed (1 DCR for 20 USDC; lock times maker 240s / taker 120s); A will go silent before redeeming"
wait_swap "$DBG_B" "$M3" part_sent 240 || fail "run 3: taker did not participate"
log "run 3: both contracts on chain; A silent"
wait_swap "$DBG_B" "$M3" refunded 300 || fail "run 3: taker did not refund its USDC"
log "run 3: B refunded its USDC contract after its lock time"
wait_swap "$DBG_A" "$M3" refunded 300 || fail "run 3: maker did not refund"
log "run 3: A refunded its DCR after its lock time"
post "$DBG_A" /mesh/swap/halt '{"stage":""}' >/dev/null
settle; BAL5=$(balances after_run3)
evidence run3 "$M3" "$DBG_A" "$DBG_B"
onchain run3 "$M3" "$DBG_A" makerSwap makerRefund
onchain run3 "$M3" "$DBG_B" takerSwap takerRefund
log "$BAL4"; log "$BAL5"; D3=$(delta "$BAL4" "$BAL5"); log "$D3 (fees only)"
check "$D3" '
assert -0.01 < d["A_dcr"] <= 0 and -0.01 < d["B_dcr"] <= 0, d
assert abs(d["A_usdc"]) < 1e-6 and abs(d["B_usdc"]) < 1e-6, d' || fail "run 3 balances not restored: $D3"
log "PASS run 3 (refund): both sides refunded, USDC back in full"
log "P18 USDC.polygon E2E PASS"
