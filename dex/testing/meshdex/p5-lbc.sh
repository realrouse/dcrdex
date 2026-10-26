#!/usr/bin/env bash
# REGTEST/SIMNET ONLY. P5: mesh atomic swaps on the LBC markets lbc_btc and
# lbc_dcr, on the lbc regtest harness (dex/testing/lbc), the btc regtest and
# dcr simnet harnesses, over the private 3-node mesh (mesh-p4.sh).
#   A (maker, sells LBC): lbcwallet beta  + bitcoind gamma / dcrwallet trading1, mesh node-1
#   B (taker, buys LBC):  lbcwallet gamma + bitcoind delta / dcrwallet trading2, mesh node-2
# Per market: a redeem swap (balances checked on both chains), a refund swap
# (A silent before redeeming; both refund), and a restart mid-swap
# (lbc_btc: kill A after it initiates; lbc_dcr: kill B after it participates).
# Usage: MARKETS="lbc_btc lbc_dcr" ./p5-lbc.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-/workspace/tatanka-dev/bin}
export PATH=$BIN:$PATH
BISONW=${BISONW:-$BIN/bisonw-tatanka}
RUN=${RUN:-/workspace/tatanka-dev/run/p5}
DEXTEST=${DEXTEST:-$HOME/dextest}
DCRCTL=$DEXTEST/dcr/harness-ctl
BTCCTL=$DEXTEST/btc/harness-ctl
LBCCTL=$DEXTEST/lbc/harness-ctl
MESH="$HERE/mesh-p4.sh"
WEB_A=127.0.0.1:15811; DBG_A=127.0.0.1:15911
WEB_B=127.0.0.1:15812; DBG_B=127.0.0.1:15912
MARKETS=${MARKETS:-lbc_btc lbc_dcr}
LOT=100000000   # 1 LBC
RATE=1000000    # 0.01 quote per LBC
TIMEOUT=${TIMEOUT:-60}

log() { echo "$(date '+%H:%M:%S') $*"; }
fail() { log "FAIL: $*" >&2; for n in a b; do [ -f "$RUN/$n/console.log" ] && grep -E 'Swap |lbc' "$RUN/$n/console.log" | tail -12 | cut -c1-260 | sed "s/^/  [$n] /" >&2; done; exit 1; }
py() { python3 -c "$1"; }

[ -x "$DCRCTL/trading1" ] && [ -x "$BTCCTL/gamma" ] && [ -x "$LBCCTL/beta" ] || fail "dcr/btc/lbc harnesses are not running"
"$MESH" start >/dev/null
ADDR_A=$("$MESH" addr 1); ADDR_B=$("$MESH" addr 2)

declare -A PID
cleanup() { for p in "${PID[@]}"; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT
# Never delete appdata that may hold unrefunded contracts.
[ -d "$RUN" ] && mv "$RUN" "$RUN.$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RUN/a" "$RUN/b"

web_of() { [ "$1" = a ] && echo "$WEB_A" || echo "$WEB_B"; }
dbg_of() { [ "$1" = a ] && echo "$DBG_A" || echo "$DBG_B"; }
addr_of() { [ "$1" = a ] && echo "$ADDR_A" || echo "$ADDR_B"; }
start() {
  TATANKA_SIMNET_ADDR=$(addr_of "$1") TATANKA_MESH_DEBUG_ADDR=$(dbg_of "$1") "$BISONW" --simnet --mesh \
    --appdata "$RUN/$1" --webaddr "$(web_of "$1")" --log=debug >> "$RUN/$1/console.log" 2>&1 &
  PID[$1]=$!
}
wait_http() { for _ in $(seq 1 "$TIMEOUT"); do curl -sf "$1" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
init() {
  local pw; pw=$(head -c 12 /dev/urandom | base64 | tr -d '/+=')
  printf '%s' "$pw" > "$RUN/$1/.testpw"; chmod 600 "$RUN/$1/.testpw"
  curl -sf -c "$RUN/$1/cookies" -XPOST "http://$(web_of "$1")/api/init" -H 'Content-Type: application/json' -d "{\"pass\":\"$pw\"}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert d["ok"], d' || fail "init $1"
}
login() {
  local pw; pw=$(cat "$RUN/$1/.testpw")
  curl -sf -c "$RUN/$1/cookies" -XPOST "http://$(web_of "$1")/api/login" -H 'Content-Type: application/json' -d "{\"pass\":\"$pw\"}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert d["ok"], d' || fail "login $1"
}
streams() { grep -c 'push stream established' "$RUN/$1/console.log" || true; }
wait_mesh() {
  wait_http "http://$(dbg_of "$1")/mesh/id" || fail "$1 debug API"
  for _ in $(seq 1 "$TIMEOUT"); do [ "$(streams "$1")" -ge "$2" ] && return 0; sleep 1; done
  fail "bisonw $1 not connected to the mesh"
}
post() { curl -sf -XPOST "http://$1$2" -d "$3"; }
POLICY=""
set_policy() { # market maker taker (both sides)
  POLICY="$1 $2 $3"
  for n in a b; do post "$(dbg_of $n)" /mesh/config "{\"market\":\"$1\",\"makerLockSec\":$2,\"takerLockSec\":$3}" >/dev/null || fail "policy $n"; done
}
subscribe_all() { for mk in $MARKETS; do post "$(dbg_of "$1")" /mesh/subscribe "{\"market\":\"$mk\"}" >/dev/null || fail "subscribe $1 $mk"; done; }
restart() {
  local n; n=$(streams "$1")
  start "$1"; wait_http "http://$(web_of "$1")/" || fail "$1 web after restart"
  login "$1"; wait_mesh "$1" $((n + 1)); subscribe_all "$1"
  if [ -n "$POLICY" ]; then set -- "$1" $POLICY; post "$(dbg_of "$1")" /mesh/config "{\"market\":\"$2\",\"makerLockSec\":$3,\"takerLockSec\":$4}" >/dev/null; fi
  log "$1 restarted and logged in (re-subscribed${POLICY:+, debug lock policy $POLICY re-applied})"
}
crash() { kill -9 "${PID[$1]}"; wait "${PID[$1]}" 2>/dev/null || true; unset "PID[$1]"; log "$1 killed (SIGKILL)"; }
conf_json() {
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
new_wallet() { # name assetID walletType walletPass configJSON
  local pw body res; pw=$(cat "$RUN/$1/.testpw")
  body=$(PW="$pw" WP="$4" CFG="$5" py "import os,json; print(json.dumps({'assetID':$2,'walletType':'$3','config':json.loads(os.environ['CFG']),'pass':os.environ['WP'],'appPass':os.environ['PW']}))")
  res=$(curl -s -b "$RUN/$1/cookies" -XPOST "http://$(web_of "$1")/api/newwallet" -H 'Content-Type: application/json' -d "$body")
  echo "$res" | py 'import sys,json; d=json.load(sys.stdin); assert d.get("ok"), d' || fail "newwallet $1 asset $2: $res"
}
halt() { post "$(dbg_of "$1")" /mesh/swap/halt "{\"stage\":\"$2\"}" >/dev/null || fail "halt $1 $2"; }
swap() { curl -sf "http://$(dbg_of "$1")/mesh/swaps" | py "import sys,json; m=[m for m in (json.load(sys.stdin)['swaps'] or []) if m['match']['id']=='$2']; print(json.dumps(m[0]) if m else '')"; }
swap_status() { swap "$1" "$2" | py 'import sys,json; s=sys.stdin.read().strip(); print(json.loads(s)["status"] if s else "none")'; }
wait_swap() { local s; for _ in $(seq 1 "${4:-$TIMEOUT}"); do s=$(swap_status "$1" "$2"); [ "$s" = "$3" ] && return 0; sleep 1; done; log "swap on $1 for $2 is $s, want $3"; return 1; }
match_status() { curl -sf "http://$(dbg_of "$1")/mesh/matches" | py "import sys,json; m=[m for m in json.load(sys.stdin)['matches'] if m['match']['id']=='$2']; print(m[0]['status'] if m else 'none')"; }
wait_match() { local s; for _ in $(seq 1 "$TIMEOUT"); do s=$(match_status "$1" "$2"); [ "$s" = "$3" ] && return 0; sleep 1; done; log "match $2 on $1: $s"; return 1; }
trade() { # market lots -> matchID
  local o m
  o=$(post "$DBG_A" /mesh/order "{\"market\":\"$1\",\"sell\":true,\"qty\":$(($2*LOT)),\"lotSize\":$LOT,\"rate\":$RATE,\"lifetimeSec\":600}" | py 'import sys,json; print(json.load(sys.stdin)["id"])')
  for _ in $(seq 1 "$TIMEOUT"); do curl -sf "http://$DBG_B/mesh/book?market=$1" | grep -q "$o" && break; sleep 1; done
  m=$(post "$DBG_B" /mesh/take "{\"market\":\"$1\",\"sell\":false,\"qty\":$(($2*LOT)),\"rate\":$RATE,\"lotSize\":$LOT}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert len(d["proposed"] or [])==1, d; print(d["proposed"][0])') || fail "take on $1"
  wait_match b "$m" agreed || fail "match $m not agreed on B"
  wait_match a "$m" agreed || fail "match $m not agreed on A"
  echo "$m"
}
show() { # name matchID label -> txids + events
  swap "$1" "$2" | L="$3" py '
import sys,json,os,datetime
r=json.load(sys.stdin); m=r["match"]
print("%s %s side=%s status=%s" % (os.environ["L"], m["id"][:16], r["side"], r["status"]))
for k in ("makerSwap","takerSwap","makerRedeem","takerRedeem","makerRefund","takerRefund"):
    if r.get(k): print("    %-12s %s" % (k, r[k]["txid"]))
for e in r["events"]:
    print("    %s %s" % (datetime.datetime.fromtimestamp(e["time"]/1000).strftime("%H:%M:%S"), e["msg"][:220]))'
}
record() { # matchID label
  show a "$1" "  A $2" | tee -a "$RUN/events.txt"; show b "$1" "  B $2" | tee -a "$RUN/events.txt" >/dev/null
  swap a "$1" | python3 -m json.tool > "$RUN/swap-$2-A.json"; swap b "$1" | python3 -m json.tool > "$RUN/swap-$2-B.json"
}
lbc_bal() { "$LBCCTL/$1" getbalance | py 'import sys,json; print("%.8f" % json.load(sys.stdin)["result"])'; }
dcr_bal() { "$DCRCTL/$1" getbalance | py 'import sys,json; print("%.8f" % json.load(sys.stdin)["cumulativetotal"])'; }
btc_bal() { "$BTCCTL/$1" getbalance; }
balances() { # label quote
  if [ "$2" = btc ]; then echo "$1 A_lbc=$(lbc_bal beta) A_btc=$(btc_bal gamma) B_lbc=$(lbc_bal gamma) B_btc=$(btc_bal delta)"
  else echo "$1 A_lbc=$(lbc_bal beta) A_dcr=$(dcr_bal trading1) B_lbc=$(lbc_bal gamma) B_dcr=$(dcr_bal trading2)"; fi
}
delta() { B="$1" A="$2" py '
import os
def p(s): return {kv.split("=")[0]:float(kv.split("=")[1]) for kv in s.split()[1:]}
b,a=p(os.environ["B"]),p(os.environ["A"])
print("delta " + " ".join("%s=%+.8f" % (k, a[k]-b[k]) for k in b))'; }
check_delta() { # deltaLine baseAmt quoteAmt
  D="$1" BA="$2" QA="$3" py '
import os
d={kv.split("=")[0][2:]+"_"+kv.split("=")[0][0]:float(kv.split("=")[1]) for kv in os.environ["D"].split()[1:]}
ba,qa,fee=float(os.environ["BA"]),float(os.environ["QA"]),0.02
q=[k for k in d if not k.startswith("lbc")][0][:3]
assert -ba-fee < d["lbc_A"] <= -ba+1e-9, ("A lbc", d)
assert qa-fee < d[q+"_A"] <= qa+1e-9, ("A quote", d)
assert ba-fee < d["lbc_B"] <= ba+1e-9, ("B lbc", d)
assert -qa-fee < d[q+"_B"] <= -qa+1e-9, ("B quote", d)'
}
settle() { "$DCRCTL/mine-alpha" 2 >/dev/null 2>&1; "$BTCCTL/mine-alpha" 2 >/dev/null 2>&1; "$LBCCTL/mine-alpha" 2 >/dev/null 2>&1; sleep 4; }
start_miner() {
  ( while true; do "$DCRCTL/mine-alpha" 1 >/dev/null 2>&1; "$BTCCTL/mine-alpha" 1 >/dev/null 2>&1; "$LBCCTL/mine-alpha" 1 >/dev/null 2>&1; sleep 3; done ) > "$RUN/miner.log" 2>&1 &
  PID[miner]=$!
}

# ---- setup ----
log "versions: $(lbcd --version | head -1); $(dcrd --version | head -1); $(bitcoind -version | head -1)"
log "private mesh tatanka-p4: A -> node-1, B -> node-2; markets: $MARKETS"
start a; start b
wait_http "http://$WEB_A/" || fail "A web"; wait_http "http://$WEB_B/" || fail "B web"
init a; init b; wait_mesh a 1; wait_mesh b 1
new_wallet a 140 lbcwalletRPC abc "$(conf_json "$DEXTEST/lbc/beta/beta.conf")"
new_wallet b 140 lbcwalletRPC abc "$(conf_json "$DEXTEST/lbc/gamma/gamma.conf")"
new_wallet a 0 bitcoindRPC "" "$(conf_json "$DEXTEST/btc/alpha/alpha.conf" walletname=gamma)"
new_wallet b 0 bitcoindRPC "" "$(conf_json "$DEXTEST/btc/beta/beta.conf" walletname=delta)"
new_wallet a 42 dcrwalletRPC abc "$(conf_json "$DEXTEST/dcr/trading1/trading1.conf" account=default)"
new_wallet b 42 dcrwalletRPC abc "$(conf_json "$DEXTEST/dcr/trading2/trading2.conf" account=default)"
log "wallets: A lbc=beta btc=gamma dcr=trading1 ; B lbc=gamma btc=delta dcr=trading2"
subscribe_all a; subscribe_all b
start_miner
log "background miner: 1 dcr + 1 btc + 1 lbc block every ~3s"
: > "$RUN/events.txt"

for MK in $MARKETS; do
  Q=${MK#lbc_}
  log "==== market $MK; default policy $(curl -sf "http://$DBG_A/mesh/policy?market=$MK")"
  # -- redeem --
  set_policy "$MK" 1200 600
  settle; B0=$(balances before "$Q"); log "$B0"
  M=$(trade "$MK" 3)
  log "$MK redeem: match $M agreed (3 LBC for 0.03 ${Q^^}; locks 1200s/600s)"
  wait_swap a "$M" complete 240 || fail "$MK redeem: A not complete"
  wait_swap b "$M" complete 60 || fail "$MK redeem: B not complete"
  settle; B1=$(balances after "$Q"); log "$B1"; D=$(delta "$B0" "$B1"); log "$D"
  record "$M" "$MK-redeem"
  check_delta "$D" 3 0.03 || fail "$MK redeem balances wrong: $D"
  log "PASS $MK redeem: A paid 3 LBC and got 0.03 ${Q^^}, B the reverse (less fees)"

  # -- restart mid-swap --
  if [ "$Q" = btc ]; then
    halt b participate
    M=$(trade "$MK" 1)
    wait_swap a "$M" init_sent || fail "A not init_sent"
    wait_swap b "$M" init_audited || fail "B not init_audited"
    crash a; halt b ""
    wait_swap b "$M" part_sent 60 || fail "B did not participate while A was down"
    log "B participated while A was down"
    restart a
    WHAT="kill maker after LBC init"
  else
    halt a redeem
    M=$(trade "$MK" 1)
    wait_swap b "$M" part_sent || fail "B not part_sent"
    wait_swap a "$M" part_audited || fail "A not part_audited"
    crash b; halt a ""
    wait_swap a "$M" redeemed 60 || fail "A did not redeem while B was down"
    log "A redeemed B's DCR contract while B was down"
    restart b
    WHAT="kill taker after participate"
  fi
  wait_swap a "$M" complete 120 || fail "$MK restart: A not complete"
  wait_swap b "$M" complete 120 || fail "$MK restart: B not complete"
  record "$M" "$MK-restart"
  log "PASS $MK restart mid-swap ($WHAT): both complete"

  # -- refund --
  set_policy "$MK" 200 100
  halt a redeem
  settle; B2=$(balances before "$Q"); log "$B2"
  M=$(trade "$MK" 2)
  log "$MK refund: match $M agreed (2 LBC; locks 200s/100s); A silent before redeeming"
  wait_swap b "$M" part_sent 60 || fail "B not part_sent"
  wait_swap b "$M" refunded 300 || fail "$MK refund: B did not refund"
  log "B refunded after its lock time"
  wait_swap a "$M" refunded 300 || fail "$MK refund: A did not refund"
  halt a ""
  settle; B3=$(balances after "$Q"); log "$B3"; D=$(delta "$B2" "$B3"); log "$D (fees only)"
  record "$M" "$MK-refund"
  check_delta "$D" 0 0 || fail "$MK refund balances wrong: $D"
  log "PASS $MK refund: both sides refunded"
done
for n in a b; do curl -sf "http://$(dbg_of $n)/mesh/swaps" | python3 -m json.tool > "$RUN/swaps-$n.json"; done
log "P5 E2E PASS"
