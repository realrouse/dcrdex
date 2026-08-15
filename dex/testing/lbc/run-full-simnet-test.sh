#!/usr/bin/env bash
# =============================================================================
# Full local simnet test for LBC on DCRDEX (Ubuntu/Debian-friendly)
#
# Installs prerequisites (optional), builds tools, starts DCR + LBC harnesses,
# starts the dcrdex server with an LBC/DCR market, runs:
#   - unit tests
#   - client LBC wallet livetest (fund / swap / redeem / refund / send / withdraw)
#   - simnet-trade-tests "success" (deposits via harness funding, bonds, full swap)
#
# Usage (from ANY directory):
#   bash /path/to/dcrdex/dex/testing/lbc/run-full-simnet-test.sh
#
# Or from repo root:
#   ./dex/testing/lbc/run-full-simnet-test.sh
#
# Flags / env:
#   SKIP_INSTALL=1     skip apt/go/decred/lbc binary install
#   SKIP_TRADE=1       skip simnet-trade-tests (only harness + TestWallet)
#   KEEP_RUNNING=1     leave harnesses + dcrdex running when finished
#   PG_PASS=dexpass    postgres password for dcrdex user (default: dexpass)
#   WORKDIR=...        override clone/build directory (default: ~/lbc-dcrdex-test)
#   REPO_DIR=...       use existing dcrdex checkout (skips git clone)
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# If run from inside a checkout, prefer that repo
if [[ -f "${SCRIPT_DIR}/../../../go.mod" ]] && grep -q 'module decred.org/dcrdex' "${SCRIPT_DIR}/../../../go.mod" 2>/dev/null; then
  DEFAULT_REPO="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
else
  DEFAULT_REPO=""
fi

REPO_DIR="${REPO_DIR:-$DEFAULT_REPO}"
WORKDIR="${WORKDIR:-${HOME}/lbc-dcrdex-test}"
BIN_DIR="${WORKDIR}/bin"
PG_PASS="${PG_PASS:-dexpass}"
export PATH="${BIN_DIR}:/usr/local/go/bin:${HOME}/go/bin:${PATH}"
export NOMINER=1   # trade tests mine their own blocks

log()  { echo -e "\n\033[1;32m==>\033[0m $*"; }
warn() { echo -e "\033[1;33mWARN:\033[0m $*"; }
die()  { echo -e "\033[1;31mERROR:\033[0m $*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

cleanup() {
  if [[ "${KEEP_RUNNING:-}" == "1" ]]; then
    warn "KEEP_RUNNING=1 — not stopping harnesses/dcrdex"
    return
  fi
  log "Stopping processes..."
  [[ -f "${HOME}/dextest/dcrdex/dcrdex.pid" ]] && kill "$(cat "${HOME}/dextest/dcrdex/dcrdex.pid")" 2>/dev/null || true
  [[ -x "${HOME}/dextest/dcrdex/quit" ]] && "${HOME}/dextest/dcrdex/quit" 2>/dev/null || true
  [[ -x "${HOME}/dextest/lbc/harness-ctl/quit" ]] && "${HOME}/dextest/lbc/harness-ctl/quit" 2>/dev/null || true
  [[ -x "${HOME}/dextest/dcr/harness-ctl/quit" ]] && "${HOME}/dextest/dcr/harness-ctl/quit" 2>/dev/null || true
  tmux kill-session -t lbc-harness 2>/dev/null || true
  tmux kill-session -t dcr-harness 2>/dev/null || true
  tmux kill-session -t dcrdex-harness 2>/dev/null || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 1. Install system packages + Go + tools
# ---------------------------------------------------------------------------
install_prereqs() {
  log "Installing system packages (sudo required)..."
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    build-essential curl git jq tmux python3 ca-certificates \
    postgresql postgresql-contrib tar gzip

  if ! command -v go >/dev/null 2>&1 || [[ "$(go env GOVERSION 2>/dev/null)" != go1.24* && "$(go env GOVERSION 2>/dev/null)" != go1.25* && "$(go env GOVERSION 2>/dev/null)" != go1.26* ]]; then
    log "Installing Go 1.24.5..."
    local go_tgz=/tmp/go1.24.5.linux-amd64.tar.gz
    curl -fsSL -o "$go_tgz" https://go.dev/dl/go1.24.5.linux-amd64.tar.gz
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$go_tgz"
  fi
  export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"
  go version

  # Postgres: ensure running + role
  sudo service postgresql start || sudo systemctl start postgresql || true
  sudo -u postgres psql -tc "SELECT 1 FROM pg_roles WHERE rolname='dcrdex'" | grep -q 1 \
    || sudo -u postgres psql -c "CREATE USER dcrdex WITH PASSWORD '${PG_PASS}' CREATEDB;"
  sudo -u postgres psql -c "ALTER USER dcrdex WITH PASSWORD '${PG_PASS}';" >/dev/null
}

install_decred_bins() {
  mkdir -p "${BIN_DIR}"
  if [[ -x "${BIN_DIR}/dcrd" && -x "${BIN_DIR}/dcrwallet" && -x "${BIN_DIR}/dcrctl" ]]; then
    log "Decred tools already in ${BIN_DIR}"
    return
  fi
  log "Downloading Decred v2.1.5 binaries..."
  local url=https://github.com/decred/decred-binaries/releases/download/v2.1.5/decred-linux-amd64-v2.1.5.tar.gz
  curl -fsSL -o /tmp/decred-tools.tar.gz "$url"
  tar -xzf /tmp/decred-tools.tar.gz -C /tmp
  cp -a /tmp/decred-linux-amd64-v2.1.5/{dcrd,dcrwallet,dcrctl} "${BIN_DIR}/"
  dcrd --version | head -1
}

install_lbc_bins() {
  mkdir -p "${BIN_DIR}"
  if [[ -x "${BIN_DIR}/lbcd" && -x "${BIN_DIR}/lbcctl" && -x "${BIN_DIR}/lbcwallet" ]]; then
    log "LBC tools already in ${BIN_DIR}"
    return
  fi
  log "Building lbcd / lbcctl / lbcwallet (this can take a few minutes)..."
  mkdir -p "${WORKDIR}/src"
  if [[ ! -d "${WORKDIR}/src/lbcd/.git" ]]; then
    git clone --depth 1 https://github.com/lbryio/lbcd.git "${WORKDIR}/src/lbcd"
  fi
  if [[ ! -d "${WORKDIR}/src/lbcwallet/.git" ]]; then
    git clone --depth 1 https://github.com/lbryio/lbcwallet.git "${WORKDIR}/src/lbcwallet"
  fi
  (cd "${WORKDIR}/src/lbcd" && go build -o "${BIN_DIR}/lbcd" .)
  (cd "${WORKDIR}/src/lbcd/cmd/lbcctl" && go build -o "${BIN_DIR}/lbcctl" .)
  (cd "${WORKDIR}/src/lbcwallet" && go build -o "${BIN_DIR}/lbcwallet" .)
  lbcd --version 2>&1 | head -1 || true
}

# ---------------------------------------------------------------------------
# 2. Resolve dcrdex source
# ---------------------------------------------------------------------------
resolve_repo() {
  if [[ -n "${REPO_DIR}" && -f "${REPO_DIR}/go.mod" ]]; then
    log "Using existing repo: ${REPO_DIR}"
    return
  fi
  log "Cloning realrouse/dcrdex lbc into ${WORKDIR}/dcrdex..."
  mkdir -p "${WORKDIR}"
  if [[ -d "${WORKDIR}/dcrdex/.git" ]]; then
    git -C "${WORKDIR}/dcrdex" fetch origin lbc || true
    git -C "${WORKDIR}/dcrdex" checkout lbc
    git -C "${WORKDIR}/dcrdex" pull --ff-only origin lbc || true
  else
    git clone --branch lbc --single-branch \
      https://github.com/realrouse/dcrdex.git "${WORKDIR}/dcrdex"
  fi
  REPO_DIR="${WORKDIR}/dcrdex"
}

# ---------------------------------------------------------------------------
# 3. Start harnesses
# ---------------------------------------------------------------------------
start_dcr_harness() {
  log "Starting DCR harness (NOMINER=1)..."
  tmux kill-session -t dcr-harness 2>/dev/null || true
  local dcr_dir="${REPO_DIR}/dex/testing/dcr"
  # Patch attach away for non-interactive run
  if ! grep -q 'NOATTACH' "${dcr_dir}/harness.sh"; then
    cp "${dcr_dir}/harness.sh" "${dcr_dir}/harness.sh.bak-fulltest"
    sed -i 's/tmux attach-session/#tmux attach-session/' "${dcr_dir}/harness.sh"
  fi
  (cd "${dcr_dir}" && NOMINER=1 bash ./harness.sh) || true
  # restore if we patched
  [[ -f "${dcr_dir}/harness.sh.bak-fulltest" ]] && mv "${dcr_dir}/harness.sh.bak-fulltest" "${dcr_dir}/harness.sh"
  sleep 2
  "${HOME}/dextest/dcr/harness-ctl/alpha" getbalance >/dev/null \
    || die "DCR harness failed to start (alpha getbalance)"
  log "DCR harness OK"
}

lbc_wallet_balance() {
  # numeric balance or 0
  local name="$1"
  "${HOME}/dextest/lbc/harness-ctl/${name}" getbalance 2>/dev/null | python3 -c "
import sys,json
try:
  d=json.load(sys.stdin)
  r=d.get('result')
  if isinstance(r,(int,float)): print(r)
  elif isinstance(r,dict): print(float(r.get('mine',{}).get('trusted',0) or 0))
  else: print(0)
except Exception:
  print(0)
"
}

lbc_mine_until_alpha() {
  # LBC pays 1 LBC/block (h<=5100) with 100-block maturity. Mine until alpha
  # has enough *spendable* balance for funding sends.
  local need="${1:-50}"
  local ctl="${HOME}/dextest/lbc/harness-ctl"
  local bal i
  "${ctl}/alpha" walletpassphrase abc 1000000 >/dev/null 2>&1 || true
  for i in $(seq 1 40); do
    bal=$(lbc_wallet_balance alpha)
    if python3 -c "import sys; sys.exit(0 if float('${bal:-0}') >= float('${need}') else 1)"; then
      return 0
    fi
    log "alpha spendable=$bal < $need — mining 25 blocks for coinbase maturity"
    "${ctl}/mine-alpha" 25 >/dev/null || true
    sleep 1
  done
  die "could not accumulate ${need} spendable LBC on alpha (bal=$(lbc_wallet_balance alpha))"
}

lbc_fund_wallet() {
  # Send several UTXOs from alpha to a deposit address and mine + wait for sync.
  local name="$1"
  local ctl="${HOME}/dextest/lbc/harness-ctl"
  local addr bal resp txid ok=0 amt
  "${ctl}/alpha" walletpassphrase abc 1000000 >/dev/null 2>&1 || true
  "${ctl}/${name}" walletpassphrase abc 1000000 >/dev/null 2>&1 || true
  addr=$("${ctl}/${name}" getnewaddress 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('result') or '')")
  [[ -n "$addr" ]] || die "could not getnewaddress for LBC $name"
  # 8*~12 ≈ 100 LBC total; ensure alpha can cover it first
  lbc_mine_until_alpha 120
  log "Topping up LBC $name at $addr ..."
  for amt in 15 15 15 15 10 10 10 10; do
    resp=$("${ctl}/alpha" sendtoaddress "$addr" "$amt" 2>/dev/null || true)
    txid=$(echo "$resp" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin); print(d.get('result') or '')
except Exception:
  print('')")
    if [[ -n "$txid" ]]; then
      ok=$((ok + 1))
    else
      warn "sendtoaddress $amt -> $name failed: $resp"
      lbc_mine_until_alpha "$((amt + 5))"
      "${ctl}/alpha" walletpassphrase abc 1000000 >/dev/null 2>&1 || true
      resp=$("${ctl}/alpha" sendtoaddress "$addr" "$amt" 2>/dev/null || true)
      txid=$(echo "$resp" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin); print(d.get('result') or '')
except Exception:
  print('')")
      [[ -n "$txid" ]] && ok=$((ok + 1))
    fi
  done
  "${ctl}/mine-alpha" 3 >/dev/null || true
  # wait until wallet reports a usable balance
  for _ in $(seq 1 60); do
    bal=$(lbc_wallet_balance "$name")
    python3 -c "import sys; sys.exit(0 if float('${bal:-0}') >= 10 else 1)" && {
      log "LBC $name balance=$bal ($ok sends accepted)"
      return 0
    }
    sleep 1
  done
  die "LBC $name still underfunded after top-up (balance=${bal:-?}, sends_ok=$ok)"
}

ensure_lbc_funded() {
  local name="$1" min="${2:-10}"
  local bal
  bal=$(lbc_wallet_balance "$name")
  if python3 -c "import sys; sys.exit(0 if float('${bal:-0}') >= float('${min}') else 1)"; then
    log "LBC $name already funded (balance=$bal)"
    return 0
  fi
  warn "LBC $name balance=$bal < $min — sending more from alpha"
  lbc_fund_wallet "$name"
}

start_lbc_harness() {
  log "Starting LBC harness (NOMINER=1)..."
  tmux kill-session -t lbc-harness 2>/dev/null || true
  (cd "${REPO_DIR}/dex/testing/lbc" && NOMINER=1 NOATTACH=1 bash ./harness.sh)
  sleep 2
  curl -s --user user:pass \
    --data-binary '{"jsonrpc":"1.0","id":"1","method":"getblockcount","params":[]}' \
    http://127.0.0.1:39245/ | grep -q result \
    || die "LBC harness failed (node RPC)"
  # Ensure both trade clients have spendable LBC (beta=client1, gamma=client2)
  ensure_lbc_funded beta 20
  ensure_lbc_funded gamma 20
  log "LBC harness OK (alpha=$(lbc_wallet_balance alpha) beta=$(lbc_wallet_balance beta) gamma=$(lbc_wallet_balance gamma))"
}

# ---------------------------------------------------------------------------
# 4. Start dcrdex server with LBC/DCR market
# ---------------------------------------------------------------------------
start_dcrdex() {
  log "Building and starting dcrdex server (simnet LBC/DCR)..."
  local app="${HOME}/dextest/dcrdex"
  mkdir -p "${app}"
  sudo -u postgres psql -c "DROP DATABASE IF EXISTS dcrdex_simnet_test;" \
    -c "CREATE DATABASE dcrdex_simnet_test OWNER dcrdex;" >/dev/null

  (cd "${REPO_DIR}/server/cmd/dcrdex" && go build -o "${app}/dcrdex" -ldflags \
    "-X 'decred.org/dcrdex/dex.testLockTimeTaker=3m' \
     -X 'decred.org/dcrdex/dex.testLockTimeMaker=6m'")

  # certs from official harness (same fixed test certs)
  python3 - <<PY
from pathlib import Path
import re
hs = Path("${REPO_DIR}/dex/testing/dcrdex/harness.sh").read_text()
data = Path("${app}")
for name in ("rpc.cert", "rpc.key"):
    m = re.search(r'cat > "\./'+name+r'" <<EOF\n(.*?)\nEOF', hs, re.S)
    if m:
        (data/name).write_text(m.group(1)+"\n")
print("certs ok")
PY

  cat > "${app}/dcrdex.conf" <<EOF
pgdbname=dcrdex_simnet_test
pgpass=${PG_PASS}
simnet=1
rpclisten=127.0.0.1:17273
debuglevel=info
loglocal=true
signingkeypass=keypass
adminsrvon=1
adminsrvpass=adminpass
adminsrvaddr=127.0.0.1:16542
bcasttimeout=1m
maxepochcancels=128
EOF

  cat > "${app}/markets.json" <<EOF
{
  "markets": [
    {
      "base": "LBC_simnet",
      "quote": "DCR_simnet",
      "lotSize": 100000000,
      "rateStep": 1000000,
      "epochDuration": 15000,
      "marketBuyBuffer": 1.2,
      "parcelSize": 4
    }
  ],
  "assets": {
    "DCR_simnet": {
      "bip44symbol": "dcr",
      "network": "simnet",
      "maxFeeRate": 10,
      "swapConf": 1,
      "configPath": "${HOME}/dextest/dcr/alpha/dcrd.conf",
      "regConfs": 1,
      "regFee": 100000000,
      "regXPub": "spubVWKGn9TGzyo7M4b5xubB5UV4joZ5HBMNBmMyGvYEaoZMkSxVG4opckpmQ26E85iHg8KQxrSVTdex56biddqtXBerG9xMN8Dvb3eNQVFFwpE",
      "bondAmt": 50000000,
      "bondConfs": 1
    },
    "LBC_simnet": {
      "bip44symbol": "lbc",
      "network": "simnet",
      "maxFeeRate": 100,
      "swapConf": 1,
      "configPath": "${HOME}/dextest/lbc/alpha/alpha-node.conf",
      "bondAmt": 1000000000,
      "bondConfs": 1
    }
  }
}
EOF

  # stop leftover
  if [[ -f "${app}/dcrdex.pid" ]]; then
    kill "$(cat "${app}/dcrdex.pid")" 2>/dev/null || true
    rm -f "${app}/dcrdex.pid"
  fi

  nohup "${app}/dcrdex" \
    --appdata="${app}" \
    --configfile="${app}/dcrdex.conf" \
    --marketsconfpath="${app}/markets.json" \
    --simnet \
    --pgpass="${PG_PASS}" \
    > "${app}/dcrdex-stdout.log" 2>&1 &
  echo $! > "${app}/dcrdex.pid"

  # wait for market
  for i in $(seq 1 60); do
    if grep -q 'Market lbc_dcr now accepting orders' "${app}/dcrdex-stdout.log" 2>/dev/null; then
      log "dcrdex: market lbc_dcr accepting orders"
      return 0
    fi
    if ! kill -0 "$(cat "${app}/dcrdex.pid")" 2>/dev/null; then
      tail -50 "${app}/dcrdex-stdout.log" || true
      die "dcrdex exited early"
    fi
    sleep 1
  done
  tail -50 "${app}/dcrdex-stdout.log" || true
  die "timeout waiting for lbc_dcr market"
}

# ---------------------------------------------------------------------------
# 5. Tests
# ---------------------------------------------------------------------------
run_level1() {
  log "LEVEL 1: unit tests + package builds"
  cd "${REPO_DIR}"
  go test ./dex/networks/lbc/ -count=1
  go build -o /tmp/lbc-cli-check ./client/asset/lbc/
  go build -o /tmp/lbc-srv-check ./server/asset/lbc/
  log "LEVEL 1 PASS"
}

run_level2() {
  log "LEVEL 2: LBC wallet livetest (fund/swap/redeem/refund/send/withdraw)"
  "${HOME}/dextest/lbc/harness-ctl/alpha" walletpassphrase abc 1000000 >/dev/null || true
  "${HOME}/dextest/lbc/harness-ctl/beta" walletpassphrase abc 1000000 >/dev/null || true
  cd "${REPO_DIR}/client/asset/lbc"
  go test -v -count=1 -tags=harness -run TestWallet -timeout 180s
  log "LEVEL 2 PASS"
  # Livetest spends from alpha/beta; top up before trade tests so client1
  # still has plenty of LBC. Gamma is untouched by livetest but re-check anyway.
  ensure_lbc_funded beta 20
  ensure_lbc_funded gamma 20
}

run_level3_trade() {
  log "LEVEL 3: simnet-trade-tests (deposits/bonds/swap) — lbcdcr success"
  cd "${REPO_DIR}/client/cmd/simnet-trade-tests"
  # ensure wallets unlocked (lbcwallet locks after timeout)
  for w in alpha beta gamma; do
    "${HOME}/dextest/lbc/harness-ctl/$w" walletpassphrase abc 1000000 >/dev/null || true
  done
  # Final balance gate — the previous failure mode was client2/gamma with 0 UTXOs
  ensure_lbc_funded beta 15
  ensure_lbc_funded gamma 15
  log "Pre-trade LBC balances: alpha=$(lbc_wallet_balance alpha) beta=$(lbc_wallet_balance beta) gamma=$(lbc_wallet_balance gamma)"
  ./run lbcdcr -t success -runonce -debug
  log "LEVEL 3 PASS (success trade)"
}

verify_admin_config() {
  log "Checking admin /api/config for dcr + lbc..."
  local cfg
  cfg=$(curl -sk --basic -u u:adminpass https://127.0.0.1:16542/api/config)
  echo "$cfg" | python3 -c "
import sys,json
c=json.load(sys.stdin)
syms={a['symbol'] for a in c['assets']}
assert 'dcr' in syms and 'lbc' in syms, syms
assert any(m['name']=='lbc_dcr' for m in c['markets']), c['markets']
print('admin config OK: assets', sorted(syms), 'markets', [m['name'] for m in c['markets']])
"
}

# ---------------------------------------------------------------------------
main() {
  log "Full LBC simnet test starting"
  echo "REPO_DIR=${REPO_DIR:-"(will clone)"} WORKDIR=${WORKDIR}"

  if [[ "${SKIP_INSTALL:-}" != "1" ]]; then
    install_prereqs
    install_decred_bins
    install_lbc_bins
  else
    export PATH="/usr/local/go/bin:${HOME}/go/bin:${BIN_DIR}:${PATH}"
    need_cmd go; need_cmd tmux; need_cmd curl; need_cmd python3
    need_cmd dcrd; need_cmd dcrwallet; need_cmd dcrctl
    need_cmd lbcd; need_cmd lbcctl; need_cmd lbcwallet
  fi

  resolve_repo
  [[ -f "${REPO_DIR}/go.mod" ]] || die "invalid REPO_DIR=${REPO_DIR}"

  start_dcr_harness
  start_lbc_harness
  start_dcrdex
  verify_admin_config

  run_level1
  run_level2

  if [[ "${SKIP_TRADE:-}" != "1" ]]; then
    run_level3_trade
  else
    warn "SKIP_TRADE=1 — not running simnet-trade-tests"
  fi

  log "ALL REQUESTED TESTS COMPLETED SUCCESSFULLY"
  echo ""
  echo "Server:   https://127.0.0.1:17273  (cert: ~/dextest/dcrdex/rpc.cert)"
  echo "Admin:    https://127.0.0.1:16542  (u / adminpass)"
  echo "Logs:     ~/dextest/dcrdex/dcrdex-stdout.log"
  echo "Harness:  tmux attach -t lbc-harness | dcr-harness"
  if [[ "${KEEP_RUNNING:-}" == "1" ]]; then
    echo "(left running — stop with ~/dextest/lbc/harness-ctl/quit etc.)"
  fi
}

main "$@"
