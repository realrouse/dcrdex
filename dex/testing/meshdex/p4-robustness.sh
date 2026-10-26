#!/usr/bin/env bash
# SIMNET ONLY. P4 robustness checks for mesh swaps on the dcr simnet and btc
# regtest harnesses, using a private 3-node mesh (mesh-p4.sh, tmux tatanka-p4)
# so node kills don't touch the shared test mesh.
#   A (maker, sells DCR): dcrwallet trading1 + bitcoind wallet gamma, mesh node-1
#   B (taker):            dcrwallet trading2 + bitcoind wallet delta, mesh node-2
# Scenarios:
#   policy   lock policy mismatch is rejected at proposal time
#   crash    A crashes after saving its signed contract, before broadcasting;
#            after restart it re-initiates with the same funding coins
#   init     A killed (SIGKILL) after initiating; B participates meanwhile
#   part     B killed after participating; A redeems meanwhile
#   redeem   A killed after redeeming; B redeems meanwhile
#   refund   B killed after participating with A silent; B restarts after
#            its lock time and refunds, A refunds after its own
#   failover B's mesh node dies mid-swap; B fails over to another node
#   orders   order cancel and order expiry during in-flight swaps
#   secrets  swap secrets are encrypted at rest
# Each restart is: kill -9, start the same appdata, /api/login.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-/workspace/tatanka-dev/bin}
export PATH=$BIN:$PATH
BISONW=${BISONW:-$BIN/bisonw-tatanka}
RUN=${RUN:-/workspace/tatanka-dev/run/p4}
DEXTEST=${DEXTEST:-$HOME/dextest}
DCRCTL=$DEXTEST/dcr/harness-ctl
BTCCTL=$DEXTEST/btc/harness-ctl
MESH="$HERE/mesh-p4.sh"
WEB_A=127.0.0.1:15801; DBG_A=127.0.0.1:15901
WEB_B=127.0.0.1:15802; DBG_B=127.0.0.1:15902
MARKET=dcr_btc
LOT=100000000   # 1 DCR
RATE=1000000    # 0.01 BTC per DCR
TIMEOUT=${TIMEOUT:-60}
SCENARIOS=${SCENARIOS:-policy crash init part redeem refund failover orders secrets}

log() { echo "$(date '+%H:%M:%S') $*"; }
fail() { log "FAIL: $*" >&2; for n in a b; do [ -f "$RUN/$n/console.log" ] && grep -E 'Swap ' "$RUN/$n/console.log" | tail -12 | cut -c1-260 | sed "s/^/  [$n] /" >&2; done; exit 1; }
py() { python3 -c "$1"; }

[ -x "$DCRCTL/trading1" ] && [ -x "$BTCCTL/gamma" ] || fail "dcr/btc harnesses are not running"
"$MESH" start >/dev/null
ADDR_A=$("$MESH" addr 1); ADDR_B=$("$MESH" addr 2)

declare -A PID
cleanup() { for p in "${PID[@]}"; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT
rm -rf "$RUN.prev"; [ -d "$RUN" ] && mv "$RUN" "$RUN.prev"   # keep the last run (its swaps may still be refundable)
mkdir -p "$RUN/a" "$RUN/b"

web_of() { [ "$1" = a ] && echo "$WEB_A" || echo "$WEB_B"; }
dbg_of() { [ "$1" = a ] && echo "$DBG_A" || echo "$DBG_B"; }
addr_of() { [ "$1" = a ] && echo "$ADDR_A" || echo "$ADDR_B"; }
start() { # name
  TATANKA_SIMNET_ADDR=$(addr_of "$1") TATANKA_MESH_DEBUG_ADDR=$(dbg_of "$1") "$BISONW" --simnet --mesh \
    --appdata "$RUN/$1" --webaddr "$(web_of "$1")" --log=debug >> "$RUN/$1/console.log" 2>&1 &
  PID[$1]=$!
}
wait_http() { for _ in $(seq 1 "$TIMEOUT"); do curl -sf "$1" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
init() { # name
  local pw; pw=$(head -c 12 /dev/urandom | base64 | tr -d '/+=')
  printf '%s' "$pw" > "$RUN/$1/.testpw"; chmod 600 "$RUN/$1/.testpw"
  curl -sf -c "$RUN/$1/cookies" -XPOST "http://$(web_of "$1")/api/init" -H 'Content-Type: application/json' -d "{\"pass\":\"$pw\"}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert d["ok"], d' || fail "init $1"
}
login() { # name
  local pw; pw=$(cat "$RUN/$1/.testpw")
  curl -sf -c "$RUN/$1/cookies" -XPOST "http://$(web_of "$1")/api/login" -H 'Content-Type: application/json' -d "{\"pass\":\"$pw\"}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert d["ok"], d' || fail "login $1"
}
wait_mesh() { # name
  wait_http "http://$(dbg_of "$1")/mesh/id" || fail "$1 debug API"
  for _ in $(seq 1 "$TIMEOUT"); do [ "$(grep -c 'push stream established' "$RUN/$1/console.log")" -ge "$2" ] && return 0; sleep 1; done
  fail "bisonw $1 not connected to the mesh"
}
restart() { # name: SIGKILL already done; start, log in, wait for the mesh
  local n; n=$(grep -c 'push stream established' "$RUN/$1/console.log" || true)
  start "$1"; wait_http "http://$(web_of "$1")/" || fail "$1 web after restart"
  login "$1"; wait_mesh "$1" $((n + 1))
  post "$(dbg_of "$1")" /mesh/config "{\"market\":\"$MARKET\",\"makerLockSec\":${POLICY% *},\"takerLockSec\":${POLICY#* }}" >/dev/null || fail "policy $1"
  post "$(dbg_of "$1")" /mesh/subscribe "{\"market\":\"$MARKET\"}" >/dev/null || fail "subscribe $1"
  log "$1 restarted and logged in (re-subscribed, debug lock policy $POLICY re-applied)"
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
post() { curl -sf -XPOST "http://$1$2" -d "$3"; }
halt() { post "$(dbg_of "$1")" /mesh/swap/halt "{\"stage\":\"$2\"}" >/dev/null || fail "halt $1 $2"; }
swap() { # name matchID -> JSON or ""
  curl -sf "http://$(dbg_of "$1")/mesh/swaps" | py "import sys,json; m=[m for m in (json.load(sys.stdin)['swaps'] or []) if m['match']['id']=='$2']; print(json.dumps(m[0]) if m else '')"
}
swap_status() { swap "$1" "$2" | py 'import sys,json; s=sys.stdin.read().strip(); print(json.loads(s)["status"] if s else "none")'; }
wait_swap() { # name matchID status timeoutSec
  local s; for _ in $(seq 1 "${4:-$TIMEOUT}"); do s=$(swap_status "$1" "$2"); [ "$s" = "$3" ] && return 0; sleep 1; done
  log "swap on $1 for $2 is $s, want $3"; return 1
}
match_status() { curl -sf "http://$(dbg_of "$1")/mesh/matches" | py "import sys,json; m=[m for m in json.load(sys.stdin)['matches'] if m['match']['id']=='$2']; print(m[0]['status']+'|'+m[0].get('reason','') if m else 'none')"; }
wait_match() { local s; for _ in $(seq 1 "$TIMEOUT"); do s=$(match_status "$1" "$2"); [ "${s%%|*}" = "$3" ] && { echo "$s"; return 0; }; sleep 1; done; log "match $2 on $1: $s"; return 1; }
post_order() { # lots lifetimeSec -> orderID
  post "$DBG_A" /mesh/order "{\"market\":\"$MARKET\",\"sell\":true,\"qty\":$(($1*LOT)),\"lotSize\":$LOT,\"rate\":$RATE,\"lifetimeSec\":$2}" \
    | py 'import sys,json; print(json.load(sys.stdin)["id"])'
}
on_book() { curl -sf "http://$DBG_B/mesh/book?market=$MARKET" | grep -q "$1"; }
take() { # lots orderID -> matchID (agreed)
  local m
  for _ in $(seq 1 "$TIMEOUT"); do on_book "$2" && break; sleep 1; done
  m=$(post "$DBG_B" /mesh/take "{\"market\":\"$MARKET\",\"sell\":false,\"qty\":$(($1*LOT)),\"rate\":$RATE,\"lotSize\":$LOT}" \
    | py 'import sys,json; d=json.load(sys.stdin); assert len(d["proposed"])==1, d; print(d["proposed"][0])')
  wait_match b "$m" agreed >/dev/null || fail "match $m not agreed on B"
  wait_match a "$m" agreed >/dev/null || fail "match $m not agreed on A"
  echo "$m"
}
trade() { take "$1" "$(post_order "$1" 600)"; }
field() { swap "$1" "$2" | py "import sys,json; r=json.load(sys.stdin); v=r.get('$3'); print(v['txid'] if isinstance(v,dict) else (json.dumps(v) if v is not None else ''))"; }
events() { # name matchID label
  swap "$1" "$2" | L="$3" py '
import sys,json,os,datetime
r=json.load(sys.stdin)
print("%s %s side=%s status=%s" % (os.environ["L"], r["match"]["id"][:16], r["side"], r["status"]))
for e in r["events"]:
    print("    %s %s" % (datetime.datetime.fromtimestamp(e["time"]/1000).strftime("%H:%M:%S"), e["msg"][:200]))'
}
both_complete() { # matchID label
  wait_swap a "$1" complete 120 || fail "$2: A not complete"
  wait_swap b "$1" complete 120 || fail "$2: B not complete"
  events a "$1" "  A" >> "$RUN/events.txt"; events b "$1" "  B" >> "$RUN/events.txt"
  log "PASS $2: both complete (maker swap $(field a "$1" makerSwap), taker redeem $(field b "$1" takerRedeem))"
}
dcr_tx_exists() { dcrctl -C "$DEXTEST/dcr/alpha/alpha-ctl.conf" getrawtransaction "$1" >/dev/null 2>&1; }
dcr_bal() { "$DCRCTL/$1" getbalance | py 'import sys,json; print("%.8f" % json.load(sys.stdin)["cumulativetotal"])'; }
balances() { echo "$1 A_dcr=$(dcr_bal trading1) A_btc=$("$BTCCTL/gamma" getbalance) B_dcr=$(dcr_bal trading2) B_btc=$("$BTCCTL/delta" getbalance)"; }
start_miner() {
  ( while true; do "$DCRCTL/mine-alpha" 1 >/dev/null 2>&1; "$BTCCTL/mine-alpha" 1 >/dev/null 2>&1; sleep 3; done ) > "$RUN/miner.log" 2>&1 &
  PID[miner]=$!
}
POLICY="7200 3600"
policy() { POLICY="$1 $2"; for n in a b; do post "$(dbg_of $n)" /mesh/config "{\"market\":\"$MARKET\",\"makerLockSec\":$1,\"takerLockSec\":$2}" >/dev/null || fail "policy $n"; done; }

# ---- setup ----
log "versions: $(dcrd --version | head -1); $(bitcoind -version | head -1)"
log "private mesh tatanka-p4: A -> node-1 $ADDR_A ; B -> node-2 $ADDR_B"
start a; start b
wait_http "http://$WEB_A/" || fail "A web"; wait_http "http://$WEB_B/" || fail "B web"
init a; init b; wait_mesh a 1; wait_mesh b 1
new_wallet a 42 dcrwalletRPC abc "$(conf_json "$DEXTEST/dcr/trading1/trading1.conf" account=default)"
new_wallet a 0 bitcoindRPC "" "$(conf_json "$DEXTEST/btc/alpha/alpha.conf" walletname=gamma)"
new_wallet b 42 dcrwalletRPC abc "$(conf_json "$DEXTEST/dcr/trading2/trading2.conf" account=default)"
new_wallet b 0 bitcoindRPC "" "$(conf_json "$DEXTEST/btc/beta/beta.conf" walletname=delta)"
post "$DBG_A" /mesh/subscribe "{\"market\":\"$MARKET\"}" >/dev/null
post "$DBG_B" /mesh/subscribe "{\"market\":\"$MARKET\"}" >/dev/null
start_miner
BAL0=$(balances start); log "$BAL0"
: > "$RUN/events.txt"

for sc in $SCENARIOS; do
case $sc in
policy)
  log "== policy: default $(curl -sf "http://$DBG_A/mesh/policy?market=$MARKET")"
  post "$DBG_A" /mesh/config "{\"market\":\"$MARKET\",\"makerLockSec\":600,\"takerLockSec\":300}" >/dev/null
  O=$(post_order 1 600)
  for _ in $(seq 1 "$TIMEOUT"); do on_book "$O" && break; sleep 1; done
  M=$(post "$DBG_B" /mesh/take "{\"market\":\"$MARKET\",\"sell\":false,\"qty\":$LOT,\"rate\":$RATE,\"lotSize\":$LOT}" | py 'import sys,json; print(json.load(sys.stdin)["proposed"][0])')
  R=$(wait_match b "$M" rejected) || fail "policy mismatch not rejected"
  echo "$R" | grep -q "lock policy" || fail "wrong reject reason: $R"
  log "PASS policy: A (600s/300s) rejected B's default-policy proposal: ${R#*|}"
  post "$DBG_A" /mesh/cancel "{\"id\":\"$O\"}" >/dev/null || true
  policy 1200 600
  log "both sides now use lock policy 1200s/600s"
  ;;
crash)
  log "== crash: A saves its signed contract and dies before broadcasting"
  halt a prebroadcast
  M=$(trade 1)
  wait_swap a "$M" init_pending || fail "A not init_pending"
  PEND=$(field a "$M" makerSwap); FUND=$(field a "$M" funding)
  log "A saved contract tx $PEND (funding $FUND), not broadcast"
  crash a
  dcr_tx_exists "${PEND%%:*}" && fail "pending tx was broadcast"
  restart a
  for _ in $(seq 1 60); do case $(swap_status a "$M") in init_pending|none) sleep 1;; *) break;; esac; done
  case $(swap_status a "$M") in init_pending|none) fail "A did not re-initiate";; esac
  NEW=$(field a "$M" makerSwap); FUND2=$(field a "$M" funding)
  [ "$FUND" = "$FUND2" ] || fail "re-initiation used different funding coins: $FUND vs $FUND2"
  dcr_tx_exists "${NEW%%:*}" || fail "contract tx not on chain"
  if [ "$NEW" = "$PEND" ]; then
    log "A re-initiated with the same funding coins and secret; deterministic signing gave the identical tx $NEW"
  else
    dcr_tx_exists "${PEND%%:*}" && fail "old pending tx on chain as well as $NEW"
    log "A re-initiated with the same funding coins: tx $NEW (unsent $PEND never reached the chain)"
  fi
  both_complete "$M" crash
  ;;
init)
  log "== init: kill A after it initiates"
  halt b participate
  M=$(trade 1)
  wait_swap a "$M" init_sent || fail "A not init_sent"
  wait_swap b "$M" init_audited || fail "B not init_audited"
  crash a
  halt b ""
  wait_swap b "$M" part_sent 60 || fail "B did not participate while A was down"
  log "B participated while A was down"
  restart a
  both_complete "$M" "kill maker after init"
  ;;
part)
  log "== part: kill B after it participates"
  halt a redeem
  M=$(trade 1)
  wait_swap b "$M" part_sent || fail "B not part_sent"
  wait_swap a "$M" part_audited || fail "A not part_audited"
  crash b
  halt a ""
  wait_swap a "$M" redeemed 60 || fail "A did not redeem while B was down"
  log "A redeemed while B was down"
  restart b
  both_complete "$M" "kill taker after participate"
  ;;
redeem)
  log "== redeem: kill A after it redeems"
  halt b taker_redeem
  M=$(trade 1)
  wait_swap a "$M" redeemed 60 || fail "A not redeemed"
  crash a
  halt b ""
  wait_swap b "$M" complete 60 || fail "B did not redeem while A was down"
  log "B redeemed while A was down"
  restart a
  both_complete "$M" "kill maker after redeem"
  ;;
refund)
  log "== refund: A silent, B killed after participating and restarted after its lock time"
  policy 200 100
  halt a redeem
  M=$(trade 1)
  wait_swap b "$M" part_sent || fail "B not part_sent"
  crash b
  TL=$(swap a "$M" | py 'import sys,json; print(json.load(sys.stdin)["match"]["takerLockTime"])')
  while [ "$(date +%s)" -lt $((TL + 30)) ]; do sleep 5; done
  log "B's lock time ($TL) has passed; restarting B"
  restart b
  wait_swap b "$M" refunded 120 || fail "B did not refund after restart"
  log "B refunded after restart: $(field b "$M" takerRefund)"
  wait_swap a "$M" refunded 240 || fail "A did not refund"
  log "PASS refund after restart: A refunded $(field a "$M" makerRefund)"
  events a "$M" "  A" >> "$RUN/events.txt"; events b "$M" "  B" >> "$RUN/events.txt"
  halt a ""
  policy 1200 600
  ;;
failover)
  log "== failover: B's mesh node (node-2) dies mid-swap"
  halt b participate
  M=$(trade 1)
  wait_swap b "$M" init_audited || fail "B not init_audited"
  N0=$(grep -c 'push stream established' "$RUN/b/console.log")
  "$MESH" kill 2; log "mesh node-2 stopped"
  halt b ""
  for _ in $(seq 1 "$TIMEOUT"); do [ "$(grep -c 'push stream established' "$RUN/b/console.log")" -gt "$N0" ] && break; sleep 1; done
  [ "$(grep -c 'push stream established' "$RUN/b/console.log")" -gt "$N0" ] || fail "B did not reconnect"
  log "B reconnected: $(grep -E 'push stream established|onnected to mesh node|failover' "$RUN/b/console.log" | tail -1 | cut -c1-200)"
  both_complete "$M" "failover during swap"
  "$MESH" restart 2; log "mesh node-2 restarted"
  ;;
orders)
  log "== orders: cancel and expiry during in-flight swaps"
  halt b participate
  O1=$(post_order 4 600)
  M1=$(take 2 "$O1")
  wait_swap b "$M1" init_audited || fail "B not init_audited (1)"
  post "$DBG_A" /mesh/cancel "{\"id\":\"$O1\"}" >/dev/null || fail "cancel"
  for _ in $(seq 1 "$TIMEOUT"); do on_book "$O1" || break; sleep 1; done
  on_book "$O1" && fail "cancelled order still on B's book"
  log "A cancelled the rest of order $O1 while its 2-lot swap is in flight"
  O2=$(post_order 2 25)
  M2=$(take 1 "$O2")
  wait_swap b "$M2" init_audited || fail "B not init_audited (2)"
  for _ in $(seq 1 60); do on_book "$O2" || break; sleep 1; done
  on_book "$O2" && fail "expired order still on B's book"
  log "order $O2 expired while its 1-lot swap is in flight"
  halt b ""
  both_complete "$M1" "swap of cancelled order"
  both_complete "$M2" "swap of expired order"
  ;;
secrets)
  log "== secrets: at rest"
  F=$(ls "$RUN"/a/simnet/mesh/swaps/*.json | head -1)
  py "
import json,glob,sys
for f in glob.glob('$RUN/a/simnet/mesh/swaps/*.json'):
    r=json.load(open(f))
    assert 'secret' not in r, f
    if r['side']=='maker' and r['status']!='new': assert r.get('encSecret'), f
print('ok')" >/dev/null || fail "plaintext secret on disk"
  KEYLEN=$(stat -c %s "$RUN/a/simnet/mesh/swapstore.key")
  [ "$KEYLEN" -gt 32 ] || fail "store key not encrypted"
  log "PASS secrets: $(ls "$RUN"/a/simnet/mesh/swaps/*.json | wc -l) swap records, maker secrets stored only as encSecret; swapstore.key is crypter-encrypted ($KEYLEN bytes)"
  ;;
esac
done
sleep 5
BAL1=$(balances end); log "$BAL0"; log "$BAL1"
for n in a b; do curl -sf "http://$(dbg_of $n)/mesh/swaps" | python3 -m json.tool > "$RUN/swaps-$n.json"; done
log "P4 E2E PASS"
