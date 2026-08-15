#!/usr/bin/env bash
# LBC regtest harness: alpha/beta/gamma lbcd nodes + matching lbcwallet processes.
# Requires: lbcd, lbcctl, lbcwallet on PATH; tmux, curl, python3.
#
#   NOMINER=1 NOATTACH=1 ./harness.sh
#   ~/dextest/lbc/harness-ctl/quit
#
# Wallet passphrase (after setup): abc  (compatible with dcrdex livetest / simnet-trade-tests)
# createtemp starts as "password"; we change to abc.

set -e

SYMBOL="lbc"
RPC_USER="user"
RPC_PASS="pass"
CREATE_PASS="password"
WALLET_PASS="abc"

NODES_ROOT="${HOME}/dextest/${SYMBOL}"
rm -rf "${NODES_ROOT}"
mkdir -p "${NODES_ROOT}"/{alpha,beta,gamma,harness-ctl}

ALPHA_DIR="${NODES_ROOT}/alpha"
BETA_DIR="${NODES_ROOT}/beta"
GAMMA_DIR="${NODES_ROOT}/gamma"
HARNESS_DIR="${NODES_ROOT}/harness-ctl"

ALPHA_LISTEN_PORT="39246"
BETA_LISTEN_PORT="39247"
GAMMA_LISTEN_PORT="39250"
ALPHA_RPC_PORT="39245"
BETA_RPC_PORT="39248"
GAMMA_RPC_PORT="39251"
ALPHA_WALLET_RPC="39244"
BETA_WALLET_RPC="39249"
GAMMA_WALLET_RPC="39252"

SESSION="${SYMBOL}-harness"
SHELL=$(which bash)

# Client confs (wallet RPC) for Bison / simnet-trade-tests / livetest
write_client_conf() {
  local name="$1" port="$2"
  local dir="${NODES_ROOT}/${name}"
  cat > "${dir}/${name}.conf" <<EOF
rpcuser=${RPC_USER}
rpcpassword=${RPC_PASS}
rpcbind=127.0.0.1:${port}
rpcport=${port}
EOF
}

write_client_conf alpha "${ALPHA_WALLET_RPC}"
write_client_conf beta "${BETA_WALLET_RPC}"
write_client_conf gamma "${GAMMA_WALLET_RPC}"

# Server backend conf (node RPC)
cat > "${ALPHA_DIR}/alpha-node.conf" <<EOF
rpcuser=${RPC_USER}
rpcpassword=${RPC_PASS}
rpcbind=127.0.0.1:${ALPHA_RPC_PORT}
rpcport=${ALPHA_RPC_PORT}
EOF

cd "${NODES_ROOT}"
tmux new-session -d -s "${SESSION}" "${SHELL}"

start_node() {
  local win="$1" name="$2" listen="$3" rpc="$4" dir="$5"
  if [ "$win" = "0" ]; then
    tmux rename-window -t "${SESSION}:0" "${name}-node"
  else
    tmux new-window -t "${SESSION}:${win}" -n "${name}-node" "${SHELL}"
  fi
  tmux send-keys -t "${SESSION}:${win}" "set +o history; cd ${dir}" C-m
  tmux send-keys -t "${SESSION}:${win}" "lbcd --regtest --notls --txindex --addrindex \
    --datadir=${dir}/data --logdir=${dir}/logs \
    --listen=127.0.0.1:${listen} --rpclisten=127.0.0.1:${rpc} \
    --rpcuser=${RPC_USER} --rpcpass=${RPC_PASS}" C-m
}

start_wallet() {
  local win="$1" name="$2" wallet_rpc="$3" node_rpc="$4" dir="$5"
  tmux new-window -t "${SESSION}:${win}" -n "${name}-wallet" "${SHELL}"
  tmux send-keys -t "${SESSION}:${win}" "set +o history; cd ${dir}" C-m
  tmux send-keys -t "${SESSION}:${win}" "lbcwallet --createtemp --regtest --noservertls --noclienttls \
    --appdata=${dir}/wallet --datadir=${dir}/wallet \
    --rpcuser=${RPC_USER} --rpcpass=${RPC_PASS} \
    --rpclisten=127.0.0.1:${wallet_rpc} --rpcconnect=127.0.0.1:${node_rpc}" C-m
}

echo "Starting LBC nodes..."
start_node 0 alpha "${ALPHA_LISTEN_PORT}" "${ALPHA_RPC_PORT}" "${ALPHA_DIR}"
sleep 2
start_node 1 beta "${BETA_LISTEN_PORT}" "${BETA_RPC_PORT}" "${BETA_DIR}"
sleep 2
start_node 2 gamma "${GAMMA_LISTEN_PORT}" "${GAMMA_RPC_PORT}" "${GAMMA_DIR}"
sleep 2

echo "Starting LBC wallets..."
start_wallet 3 alpha "${ALPHA_WALLET_RPC}" "${ALPHA_RPC_PORT}" "${ALPHA_DIR}"
sleep 2
start_wallet 4 beta "${BETA_WALLET_RPC}" "${BETA_RPC_PORT}" "${BETA_DIR}"
sleep 2
start_wallet 5 gamma "${GAMMA_WALLET_RPC}" "${GAMMA_RPC_PORT}" "${GAMMA_DIR}"
sleep 4

tmux new-window -t "${SESSION}:6" -n 'harness-ctl' "${SHELL}"
tmux send-keys -t "${SESSION}:6" "set +o history; cd ${HARNESS_DIR}" C-m

cd "${HARNESS_DIR}"

# Node CLI wrappers (lbcctl)
make_node_cli() {
  local name="$1" port="$2"
  cat > "./${name}-node" <<EOF
#!/usr/bin/env bash
lbcctl --regtest --rpcuser=${RPC_USER} --rpcpass=${RPC_PASS} --rpcserver=127.0.0.1:${port} --notls "\$@"
EOF
  chmod +x "./${name}-node"
}
make_node_cli alpha "${ALPHA_RPC_PORT}"
make_node_cli beta "${BETA_RPC_PORT}"
make_node_cli gamma "${GAMMA_RPC_PORT}"

# Wallet CLI wrappers — bitcoin-cli style for simnet-trade-tests:
#   ./alpha sendtoaddress <addr> <amt>
#   ./alpha walletpassphrase <pass> <timeout>
#   ./alpha getnewaddress
#   ./alpha getbalance
make_wallet_cli() {
  local name="$1" port="$2"
  cat > "./${name}" <<'EOF'
#!/usr/bin/env bash
PORT="__PORT__"
USER="__USER__"
PASS="__PASS__"
METHOD="$1"
shift || true

json_rpc() {
  local params="$1"
  local resp ec
  resp=$(curl -s --user "${USER}:${PASS}" \
    --data-binary "{\"jsonrpc\":\"1.0\",\"id\":\"1\",\"method\":\"${METHOD}\",\"params\":${params}}" \
    -H 'content-type: text/plain;' \
    "http://127.0.0.1:${PORT}/") || return 1
  echo "$resp"
  # Non-zero exit when JSON-RPC error is present so callers (simnet fund) fail loudly
  echo "$resp" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin)
except Exception:
  sys.exit(0)
sys.exit(1 if d.get('error') else 0)" 2>/dev/null
}

case "${METHOD}" in
  sendtoaddress)
    # addr amount
    json_rpc "[\"$1\", $2]"
    ;;
  walletpassphrase)
    json_rpc "[\"$1\", $2]"
    ;;
  walletpassphrasechange)
    json_rpc "[\"$1\", \"$2\"]"
    ;;
  getnewaddress|getbalance|getblockcount|listunspent|getinfo|walletlock)
    if [ -n "$1" ]; then
      json_rpc "$1"
    else
      json_rpc "[]"
    fi
    ;;
  "")
    echo "usage: $0 <method> [args...]" >&2
    exit 1
    ;;
  *)
    # raw: method '[json array]'
    if [ -n "$1" ]; then
      json_rpc "$1"
    else
      json_rpc "[]"
    fi
    ;;
esac
EOF
  sed -i "s/__PORT__/${port}/g; s/__USER__/${RPC_USER}/g; s/__PASS__/${RPC_PASS}/g" "./${name}"
  chmod +x "./${name}"
}
make_wallet_cli alpha "${ALPHA_WALLET_RPC}"
make_wallet_cli beta "${BETA_WALLET_RPC}"
make_wallet_cli gamma "${GAMMA_WALLET_RPC}"

# Helpers -----------------------------------------------------------------
rpc_result() {
  # stdin: json rpc response -> stdout: result as string (empty if null/missing)
  python3 -c "import sys,json
try:
  d=json.load(sys.stdin)
except Exception:
  sys.exit(0)
r=d.get('result')
if r is None:
  if d.get('error'):
    sys.stderr.write('rpc error: %s\n' % d.get('error'))
  sys.exit(0)
print(r if not isinstance(r, (dict, list)) else json.dumps(r))
"
}

wait_wallet_rpc() {
  local name="$1" tries="${2:-60}"
  local i
  for i in $(seq 1 "$tries"); do
    if ./$name getblockcount 2>/dev/null | grep -q '"result"'; then
      echo "  wallet $name RPC ready"
      return 0
    fi
    # getblockcount may not exist on wallet; try getbalance / getinfo
    if ./$name getbalance 2>/dev/null | grep -q '"result"'; then
      echo "  wallet $name RPC ready (getbalance)"
      return 0
    fi
    if ./$name getinfo 2>/dev/null | grep -q '"result"'; then
      echo "  wallet $name RPC ready (getinfo)"
      return 0
    fi
    sleep 1
  done
  echo "ERROR: wallet $name RPC not ready after ${tries}s" >&2
  return 1
}

wait_node_rpc() {
  local name="$1" tries="${2:-60}"
  local i
  for i in $(seq 1 "$tries"); do
    if ./$name-node getblockcount >/dev/null 2>&1; then
      echo "  node $name RPC ready"
      return 0
    fi
    sleep 1
  done
  echo "ERROR: node $name RPC not ready after ${tries}s" >&2
  return 1
}

node_height() {
  ./$1-node getblockcount 2>/dev/null | tr -d '\r' | tail -1
}

wait_height_at_least() {
  local name="$1" want="$2" tries="${3:-90}"
  local i h
  for i in $(seq 1 "$tries"); do
    h=$(node_height "$name" || echo 0)
    h=${h:-0}
    if [ "$h" -ge "$want" ] 2>/dev/null; then
      echo "  node $name height=$h (want >= $want)"
      return 0
    fi
    sleep 1
  done
  echo "ERROR: node $name height=$(node_height "$name") never reached $want" >&2
  return 1
}

wallet_balance() {
  # Prefer numeric result from getbalance
  ./$1 getbalance 2>/dev/null | python3 -c "
import sys,json
try:
  d=json.load(sys.stdin)
  r=d.get('result')
  if isinstance(r, (int,float)):
    print(r)
  elif isinstance(r, dict):
    # some wallets return {mine:{trusted:..}}
    print(r.get('mine',{}).get('trusted', r.get('total', 0)) or 0)
  else:
    print(0)
except Exception:
  print(0)
"
}

cat > ./mine-alpha <<EOF
#!/usr/bin/env bash
N="\${1:-1}"
./alpha walletpassphrase "${WALLET_PASS}" 600 >/dev/null 2>&1 || true
ADDR=\$(./alpha getnewaddress | python3 -c "import sys,json; print(json.load(sys.stdin).get('result') or '')")
if [ -z "\$ADDR" ]; then
  echo "failed to get mining address" >&2
  exit 1
fi
./alpha-node generatetoaddress "\$N" "\$ADDR"
EOF
chmod +x ./mine-alpha

cat > ./quit <<EOF
#!/usr/bin/env bash
for w in 0 1 2 3 4 5; do
  tmux send-keys -t ${SESSION}:\$w C-c 2>/dev/null || true
done
sleep 1
tmux kill-session -t ${SESSION} 2>/dev/null || true
EOF
chmod +x ./quit

echo "Waiting for node RPCs..."
wait_node_rpc alpha
wait_node_rpc beta
wait_node_rpc gamma

echo "Connecting peers..."
./beta-node addnode "127.0.0.1:${ALPHA_LISTEN_PORT}" add || true
./gamma-node addnode "127.0.0.1:${ALPHA_LISTEN_PORT}" add || true
# also connect reverse for faster gossip
./alpha-node addnode "127.0.0.1:${BETA_LISTEN_PORT}" add || true
./alpha-node addnode "127.0.0.1:${GAMMA_LISTEN_PORT}" add || true
sleep 2

echo "Waiting for wallet RPCs..."
wait_wallet_rpc alpha
wait_wallet_rpc beta
wait_wallet_rpc gamma

echo "Setting wallet passphrases to ${WALLET_PASS}..."
for w in alpha beta gamma; do
  ./$w walletpassphrasechange "${CREATE_PASS}" "${WALLET_PASS}" || true
  ./$w walletpassphrase "${WALLET_PASS}" 1000000 || true
done
sleep 1

ALPHA_ADDR=$(./alpha getnewaddress | rpc_result)
BETA_ADDR=$(./beta getnewaddress | rpc_result)
GAMMA_ADDR=$(./gamma getnewaddress | rpc_result)
echo "alpha=${ALPHA_ADDR}"
echo "beta=${BETA_ADDR}"
echo "gamma=${GAMMA_ADDR}"

if [ -z "${ALPHA_ADDR}" ]; then
  echo "ERROR: failed to get alpha mining address" >&2
  exit 1
fi
if [ -z "${BETA_ADDR}" ] || [ -z "${GAMMA_ADDR}" ]; then
  echo "ERROR: failed to get beta/gamma deposit addresses (beta='${BETA_ADDR}' gamma='${GAMMA_ADDR}')" >&2
  exit 1
fi

# LBC subsidy (lbcd CalcBlockSubsidy): heights 1..5100 pay only 1 LBC each.
# Coinbase maturity is 100 blocks. Mining only ~130 blocks leaves ~30 LBC
# spendable — not enough to fund beta+gamma (~68 each) plus later simnet
# re-funding (~124 per client). Mine enough that mature coinbases cover tests.
#
# At height H, mature subsidy ≈ max(0, H-100) LBC (for H <= 5100).
# Target: ~500 spendable LBC → height >= 600.
MINE_BLOCKS="${LBC_MINE_BLOCKS:-600}"
echo "Mining ${MINE_BLOCKS} blocks to alpha (1 LBC/block, maturity 100)..."
./alpha-node generatetoaddress "${MINE_BLOCKS}" "${ALPHA_ADDR}"
sleep 2

# Peers must catch up before funding txs are visible to beta/gamma wallets
ALPHA_H=$(node_height alpha)
echo "Waiting for beta/gamma nodes to sync to height ${ALPHA_H}..."
wait_height_at_least beta "${ALPHA_H}" 180
wait_height_at_least gamma "${ALPHA_H}" 180

# Mine extra blocks until alpha reports enough *spendable* balance.
# Each extra block matures one older 1-LBC coinbase.
ensure_alpha_spendable() {
  local need="$1"
  local bal tries=0
  ./alpha walletpassphrase "${WALLET_PASS}" 1000000 || true
  while true; do
    bal=$(wallet_balance alpha)
    if python3 -c "import sys; sys.exit(0 if float('${bal:-0}') >= float('${need}') else 1)"; then
      echo "  alpha spendable balance=$bal (need >= $need)"
      return 0
    fi
    echo "  alpha spendable=$bal < $need — mining 25 more for maturity..."
    ./alpha-node generatetoaddress 25 "${ALPHA_ADDR}" >/dev/null
    sleep 1
    tries=$((tries + 1))
    if [ "$tries" -gt 40 ]; then
      echo "ERROR: could not accumulate ${need} spendable LBC on alpha (bal=$bal)" >&2
      return 1
    fi
  done
}

fund_addr() {
  local addr="$1" label="$2"
  local amt txid ok=0 fail=0
  # Multiple UTXOs of varying size (livetest / coin selection like other harnesses).
  # Total = 68 LBC per client wallet.
  for amt in 10 10 10 10 5 5 5 5 2 2 2 2; do
    # Ensure alpha can cover this send + fee headroom
    ensure_alpha_spendable $((amt + 1)) || return 1
    resp=$(./alpha sendtoaddress "$addr" "$amt" || true)
    txid=$(echo "$resp" | rpc_result)
    if [ -n "$txid" ]; then
      ok=$((ok + 1))
    else
      fail=$((fail + 1))
      echo "  WARN: sendtoaddress $amt -> $label failed: $resp" >&2
      # Mine a bit and retry once (maturity / mempool edge cases)
      ./alpha-node generatetoaddress 5 "${ALPHA_ADDR}" >/dev/null || true
      sleep 1
      ./alpha walletpassphrase "${WALLET_PASS}" 1000000 || true
      resp=$(./alpha sendtoaddress "$addr" "$amt" || true)
      txid=$(echo "$resp" | rpc_result)
      if [ -n "$txid" ]; then
        ok=$((ok + 1))
        fail=$((fail - 1))
      fi
    fi
  done
  echo "  $label: $ok/12 sends accepted ($fail failed)"
  [ "$ok" -ge 6 ]
}

echo "Funding beta and gamma (need ~68 LBC each; alpha must have mature coinbases)..."
./alpha walletpassphrase "${WALLET_PASS}" 1000000 || true
ensure_alpha_spendable 150
fund_addr "${BETA_ADDR}" "beta"
# Confirm beta batch so change returns to alpha before funding gamma
./alpha-node generatetoaddress 2 "${ALPHA_ADDR}" >/dev/null
sleep 1
ensure_alpha_spendable 80
fund_addr "${GAMMA_ADDR}" "gamma"

./alpha-node generatetoaddress 6 "${ALPHA_ADDR}"
sleep 2

ALPHA_H=$(node_height alpha)
echo "Waiting for beta/gamma to see funding blocks (height ${ALPHA_H})..."
wait_height_at_least beta "${ALPHA_H}" 120
wait_height_at_least gamma "${ALPHA_H}" 120
# give wallets a moment to process tip
sleep 3

echo "Balances after initial fund:"
for w in alpha beta gamma; do
  echo -n "  $w: "
  wallet_balance "$w"
done

# Top-up any client wallet that still shows low balance
topup_if_empty() {
  local name="$1" addr="$2"
  local bal empty
  bal=$(wallet_balance "$name")
  empty=$(python3 -c "print(1 if float('${bal:-0}') < 20.0 else 0)")
  if [ "$empty" != "1" ]; then
    return 0
  fi
  echo "Topping up $name (balance=$bal) -> $addr ..."
  ensure_alpha_spendable 80 || return 1
  ./alpha walletpassphrase "${WALLET_PASS}" 1000000 || true
  local ok=0 amt
  for amt in 15 15 15 10 10 10; do
    resp=$(./alpha sendtoaddress "$addr" "$amt" || true)
    txid=$(echo "$resp" | rpc_result)
    if [ -n "$txid" ]; then
      ok=$((ok + 1))
    else
      echo "  WARN: topup send $amt -> $name failed: $resp" >&2
    fi
  done
  ./alpha-node generatetoaddress 3 "${ALPHA_ADDR}" >/dev/null
  sleep 2
  ALPHA_H=$(node_height alpha)
  wait_height_at_least "$name" "${ALPHA_H}" 90 || true
  sleep 2
  echo "  $name balance now: $(wallet_balance "$name") ($ok top-up sends)"
}

topup_if_empty beta "${BETA_ADDR}"
topup_if_empty gamma "${GAMMA_ADDR}"

echo "Final balances:"
for w in alpha beta gamma; do
  bal=$(wallet_balance "$w")
  echo "  $w: $bal"
done
echo "  chain heights: alpha=$(node_height alpha) beta=$(node_height beta) gamma=$(node_height gamma)"

# Hard fail if client wallets still empty — catches silent fund failures early
for w in beta gamma; do
  bal=$(wallet_balance "$w")
  empty=$(python3 -c "print(1 if float('${bal:-0}') < 20.0 else 0)")
  if [ "$empty" = "1" ]; then
    echo "ERROR: $w wallet still underfunded (balance=$bal). Check peer sync / sendtoaddress / subsidy." >&2
    echo "  beta height=$(node_height beta) gamma height=$(node_height gamma) alpha height=$(node_height alpha)" >&2
    echo "  alpha balance=$(wallet_balance alpha)" >&2
    exit 1
  fi
done

# Optional background miner
if [ -z "$NOMINER" ]; then
  tmux new-window -t "${SESSION}:7" -n miner "${SHELL}"
  tmux send-keys -t "${SESSION}:7" "cd ${HARNESS_DIR}" C-m
  tmux send-keys -t "${SESSION}:7" "watch -n 15 ./mine-alpha 1" C-m
fi

tmux send-keys -t "${SESSION}:6" "set -o history" C-m
tmux select-window -t "${SESSION}:6"
echo "LBC harness ready (session ${SESSION}). Attach: tmux attach -t ${SESSION}"
if [ -z "${NOATTACH}" ]; then
  tmux attach-session -t "${SESSION}"
fi
