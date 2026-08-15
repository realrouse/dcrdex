#!/usr/bin/env bash
# =============================================================================
# Manual Bison Wallet UI simnet for LBC on DCRDEX
#
# Starts the same infrastructure as run-full-simnet-test.sh:
#   DCR harness + LBC harness + dcrdex (dcr_lbc market)
# then builds and launches one or two bisonw instances with short simnet
# swap lock times so YOU can open the web UI in a browser and place
# orders / complete swaps yourself while chains mine in the background.
#
# Usage (from repo root or any directory):
#   ./dex/testing/lbc/run-manual-ui-simnet.sh
#   bash /path/to/dcrdex/dex/testing/lbc/run-manual-ui-simnet.sh
#
# Env / flags:
#   SKIP_INSTALL=1       skip apt/go/decred/lbc binary install
#   RUN_AUTOTEST=1       also run Level 1 + Level 2 before starting UI
#   ONE_WALLET=1         only start bisonw #1 (default: two wallets)
#   OPEN_BROWSER=1       try xdg-open / sensible-browser for UI URLs
#   KEEP_WALLETS=1       do not wipe bisonw appdata between runs
#   FORCE_SITE_BUILD=1   rebuild client/webserver/site (needed after UI JS/icon changes)
#   NO_MINER=1           do not start background block miners
#   PG_PASS=dexpass      postgres password for dcrdex (default: dexpass)
#   WORKDIR=...          clone/build dir (default: ~/lbc-dcrdex-test)
#   REPO_DIR=...         existing dcrdex checkout
#   WEB1=127.0.0.1:5760  bisonw #1 UI listen address
#   WEB2=127.0.0.1:5762  bisonw #2 UI listen address
#
# Stop everything:
#   ~/dextest/lbc-ui/quit
#   # or Ctrl+C in this script's terminal
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "${SCRIPT_DIR}/../../../go.mod" ]] && grep -q 'module decred.org/dcrdex' "${SCRIPT_DIR}/../../../go.mod" 2>/dev/null; then
  DEFAULT_REPO="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
else
  DEFAULT_REPO=""
fi

REPO_DIR="${REPO_DIR:-$DEFAULT_REPO}"
WORKDIR="${WORKDIR:-${HOME}/lbc-dcrdex-test}"
BIN_DIR="${WORKDIR}/bin"
PG_PASS="${PG_PASS:-dexpass}"
UI_ROOT="${HOME}/dextest/lbc-ui"
BW1_DIR="${UI_ROOT}/bisonw1"
BW2_DIR="${UI_ROOT}/bisonw2"
CTL_DIR="${UI_ROOT}/ctl"
WEB1="${WEB1:-127.0.0.1:5760}"
WEB2="${WEB2:-127.0.0.1:5762}"
RPC1="${RPC1:-127.0.0.1:5761}"
RPC2="${RPC2:-127.0.0.1:5763}"
SESSION_BW="lbc-bisonw"
export PATH="${BIN_DIR}:/usr/local/go/bin:${HOME}/go/bin:${PATH}"

log()  { echo -e "\n\033[1;32m==>\033[0m $*"; }
warn() { echo -e "\033[1;33mWARN:\033[0m $*"; }
die()  { echo -e "\033[1;31mERROR:\033[0m $*" >&2; exit 1; }
need_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

RUNNING=0
cleanup() {
  # Always leave stack up unless we never finished startup, or user hit Ctrl+C
  # after RUNNING=1 — then stop bisonw + optional full stack.
  if [[ "${RUNNING}" != "1" ]]; then
    return
  fi
  if [[ "${STOP_ON_EXIT:-}" == "1" ]]; then
    log "Stopping bisonw + harnesses + dcrdex..."
    "${CTL_DIR}/quit" 2>/dev/null || true
  else
    warn "Script exiting — stack left running. Stop with: ${CTL_DIR}/quit"
  fi
}
trap cleanup EXIT
trap 'STOP_ON_EXIT=1; exit 130' INT TERM

# ---------------------------------------------------------------------------
# Prereqs (same idea as run-full-simnet-test.sh)
# ---------------------------------------------------------------------------
install_prereqs() {
  log "Installing system packages (sudo required)..."
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    build-essential curl git jq tmux python3 ca-certificates \
    postgresql postgresql-contrib tar gzip \
    nodejs npm

  if ! command -v go >/dev/null 2>&1 || [[ "$(go env GOVERSION 2>/dev/null)" != go1.24* && "$(go env GOVERSION 2>/dev/null)" != go1.25* && "$(go env GOVERSION 2>/dev/null)" != go1.26* ]]; then
    log "Installing Go 1.24.5..."
    local go_tgz=/tmp/go1.24.5.linux-amd64.tar.gz
    curl -fsSL -o "$go_tgz" https://go.dev/dl/go1.24.5.linux-amd64.tar.gz
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$go_tgz"
  fi
  export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"
  go version

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
  log "Building lbcd / lbcctl / lbcwallet..."
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
# Harnesses + dcrdex
# ---------------------------------------------------------------------------
start_dcr_harness() {
  log "Starting DCR harness (NOMINER=1 for setup; we start miners later)..."
  tmux kill-session -t dcr-harness 2>/dev/null || true
  local dcr_dir="${REPO_DIR}/dex/testing/dcr"
  if ! grep -q 'NOATTACH' "${dcr_dir}/harness.sh" 2>/dev/null; then
    cp "${dcr_dir}/harness.sh" "${dcr_dir}/harness.sh.bak-ui"
    sed -i 's/tmux attach-session/#tmux attach-session/' "${dcr_dir}/harness.sh"
  fi
  (cd "${dcr_dir}" && NOMINER=1 bash ./harness.sh) || true
  [[ -f "${dcr_dir}/harness.sh.bak-ui" ]] && mv "${dcr_dir}/harness.sh.bak-ui" "${dcr_dir}/harness.sh"
  sleep 2
  "${HOME}/dextest/dcr/harness-ctl/alpha" getbalance >/dev/null \
    || die "DCR harness failed to start"
  log "DCR harness OK"
}

start_lbc_harness() {
  log "Starting LBC harness (NOMINER=1 for setup)..."
  tmux kill-session -t lbc-harness 2>/dev/null || true
  (cd "${REPO_DIR}/dex/testing/lbc" && NOMINER=1 NOATTACH=1 bash ./harness.sh)
  sleep 2
  curl -s --user user:pass \
    --data-binary '{"jsonrpc":"1.0","id":"1","method":"getblockcount","params":[]}' \
    http://127.0.0.1:39245/ | grep -q result \
    || die "LBC harness failed (node RPC)"
  # Ensure client wallets have funds for manual trading
  for w in beta gamma; do
    local bal
    bal=$("${HOME}/dextest/lbc/harness-ctl/${w}" getbalance 2>/dev/null | python3 -c "
import sys,json
try:
  r=json.load(sys.stdin).get('result')
  print(float(r) if isinstance(r,(int,float)) else 0)
except Exception:
  print(0)
")
    log "LBC $w balance=$bal"
  done
  # Unlock wallets for long-lived UI session
  for w in alpha beta gamma; do
    "${HOME}/dextest/lbc/harness-ctl/$w" walletpassphrase abc 100000000 >/dev/null 2>&1 || true
  done
  log "LBC harness OK"
}

start_dcrdex() {
  log "Building and starting dcrdex server (simnet DCR/LBC)..."
  local app="${HOME}/dextest/dcrdex"
  mkdir -p "${app}"
  sudo -u postgres psql -c "DROP DATABASE IF EXISTS dcrdex_simnet_ui;" \
    -c "CREATE DATABASE dcrdex_simnet_ui OWNER dcrdex;" >/dev/null

  (cd "${REPO_DIR}/server/cmd/dcrdex" && go build -o "${app}/dcrdex" -ldflags \
    "-X 'decred.org/dcrdex/dex.testLockTimeTaker=3m' \
     -X 'decred.org/dcrdex/dex.testLockTimeMaker=6m'")

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
pgdbname=dcrdex_simnet_ui
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

  # Slightly longer epochs than automated tests so manual order entry is easier
  cat > "${app}/markets.json" <<EOF
{
  "markets": [
    {
      "base": "DCR_simnet",
      "quote": "LBC_simnet",
      "lotSize": 100000000,
      "rateStep": 1000000,
      "epochDuration": 20000,
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

  for i in $(seq 1 60); do
    if grep -q 'Market dcr_lbc now accepting orders' "${app}/dcrdex-stdout.log" 2>/dev/null; then
      log "dcrdex: market dcr_lbc accepting orders"
      return 0
    fi
    if ! kill -0 "$(cat "${app}/dcrdex.pid")" 2>/dev/null; then
      tail -50 "${app}/dcrdex-stdout.log" || true
      die "dcrdex exited early"
    fi
    sleep 1
  done
  tail -50 "${app}/dcrdex-stdout.log" || true
  die "timeout waiting for dcr_lbc market"
}

start_background_miners() {
  if [[ "${NO_MINER:-}" == "1" ]]; then
    warn "NO_MINER=1 — mine manually with ${CTL_DIR}/mine-dcr and ${CTL_DIR}/mine-lbc"
    return
  fi
  log "Starting background miners (DCR + LBC every ~15s) for swap confirmations..."
  # DCR miner in a dedicated tmux window if possible
  if tmux has-session -t dcr-harness 2>/dev/null; then
    tmux kill-window -t dcr-harness:miner 2>/dev/null || true
    tmux new-window -t dcr-harness -n miner bash
    tmux send-keys -t dcr-harness:miner "cd ${HOME}/dextest/dcr/harness-ctl && watch -n 15 ./mine-alpha 1" C-m
  fi
  if tmux has-session -t lbc-harness 2>/dev/null; then
    tmux kill-window -t lbc-harness:miner 2>/dev/null || true
    tmux new-window -t lbc-harness -n miner bash
    tmux send-keys -t lbc-harness:miner "cd ${HOME}/dextest/lbc/harness-ctl && watch -n 15 ./mine-alpha 1" C-m
  fi
}

# ---------------------------------------------------------------------------
# Optional automated checks
# ---------------------------------------------------------------------------
run_autotests() {
  log "RUN_AUTOTEST=1: Level 1 unit + Level 2 TestWallet"
  cd "${REPO_DIR}"
  go test ./dex/networks/lbc/ -count=1
  go build -o /tmp/lbc-cli-check ./client/asset/lbc/
  go build -o /tmp/lbc-srv-check ./server/asset/lbc/
  "${HOME}/dextest/lbc/harness-ctl/alpha" walletpassphrase abc 1000000 >/dev/null || true
  "${HOME}/dextest/lbc/harness-ctl/beta" walletpassphrase abc 1000000 >/dev/null || true
  (cd "${REPO_DIR}/client/asset/lbc" && go test -v -count=1 -tags=harness -run TestWallet -timeout 180s)
  log "Autotests PASS"
}

# ---------------------------------------------------------------------------
# Bison Wallet
# ---------------------------------------------------------------------------
# Prefer system NodeSource/apt node over nvm/fnm/old user installs (PATH order).
pick_node_bin() {
  local cand
  for cand in /usr/bin/node /usr/local/bin/node; do
    if [[ -x "$cand" ]]; then
      local major
      major=$("$cand" -p "process.versions.node.split('.')[0]" 2>/dev/null || echo 0)
      if [[ "${major}" -ge 18 ]]; then
        echo "$cand"
        return 0
      fi
    fi
  done
  if command -v node >/dev/null 2>&1; then
    local major
    major=$(node -p "process.versions.node.split('.')[0]" 2>/dev/null || echo 0)
    if [[ "${major}" -ge 18 ]]; then
      command -v node
      return 0
    fi
  fi
  return 1
}

ensure_node() {
  local node_bin npm_bin major
  # Drop stale hashed paths so newly installed /usr/bin/node is visible.
  hash -r 2>/dev/null || true
  # Put system bins first so nvm's old Node 16 does not win.
  export PATH="/usr/bin:/usr/local/bin:${PATH}"

  if node_bin=$(pick_node_bin); then
    npm_bin="$(dirname "$node_bin")/npm"
    [[ -x "$npm_bin" ]] || npm_bin=$(command -v npm || true)
    export NODE_BIN="$node_bin"
    export NPM_BIN="${npm_bin:-npm}"
    log "Using Node $($NODE_BIN --version) at $NODE_BIN"
    return 0
  fi

  warn "No Node >= 18 on PATH (have: $(command -v node 2>/dev/null || echo none) $(node --version 2>/dev/null || true)) — installing Node 20..."
  # NodeSource Node 20 (works on Ubuntu 22.04/24.04)
  curl -fsSL https://deb.nodesource.com/setup_20.x | sudo -E bash -
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs
  hash -r 2>/dev/null || true
  export PATH="/usr/bin:/usr/local/bin:${PATH}"

  node_bin=$(pick_node_bin) || die "need Node >= 18 for site build.
  Installed package may be shadowed by nvm/old node.
  Try:  /usr/bin/node --version
        type -a node
        export PATH=/usr/bin:\$PATH
  Then re-run this script."
  npm_bin="$(dirname "$node_bin")/npm"
  [[ -x "$npm_bin" ]] || npm_bin=/usr/bin/npm
  export NODE_BIN="$node_bin"
  export NPM_BIN="$npm_bin"
  log "Using Node $($NODE_BIN --version) at $NODE_BIN (npm=$NPM_BIN)"
}

build_site() {
  # go:embed requires client/webserver/site/dist (webpack output).
  # Coin icons under site/src/img are embedded separately, but JS (BipIDs etc.)
  # is bundled into dist — rebuild when FORCE_SITE_BUILD=1 or dist is missing.
  local site_dir="${REPO_DIR}/client/webserver/site"
  local need_build=0
  if [[ "${SKIP_SITE_BUILD:-}" == "1" ]]; then
    [[ -d "${site_dir}/dist" ]] || die "SKIP_SITE_BUILD=1 but site/dist missing"
    log "SKIP_SITE_BUILD=1 — using existing site/dist"
    return 0
  fi
  if [[ "${FORCE_SITE_BUILD:-}" == "1" ]]; then
    need_build=1
  elif [[ ! -d "${site_dir}/dist" ]] || [[ -z "$(ls -A "${site_dir}/dist" 2>/dev/null)" ]]; then
    need_build=1
  else
    # Rebuild if JS/CSS/icons newer than any dist artifact (LBC BipIDs, logo, etc.)
    local newest_src newest_dist
    newest_src=$(find "${site_dir}/src" -type f \( -name '*.ts' -o -name '*.js' -o -name '*.css' -o -name '*.scss' -o -name '*.png' -o -name '*.svg' \) -printf '%T@\n' 2>/dev/null | sort -n | tail -1)
    newest_dist=$(find "${site_dir}/dist" -type f -printf '%T@\n' 2>/dev/null | sort -n | tail -1)
    if [[ -n "$newest_src" && -n "$newest_dist" ]] && python3 -c "import sys; sys.exit(0 if float('${newest_src}') > float('${newest_dist}') else 1)"; then
      need_build=1
      log "site/src newer than site/dist — rebuilding UI bundle"
    fi
  fi
  if [[ "$need_build" != "1" ]]; then
    log "Web UI site/dist up to date — skipping npm build (FORCE_SITE_BUILD=1 to force)"
    return 0
  fi
  ensure_node
  log "Building Bison Wallet web UI (npm in client/webserver/site)..."
  echo "  node=$($NODE_BIN --version) ($NODE_BIN)  npm=$($NPM_BIN --version 2>/dev/null) ($NPM_BIN)"
  # Prefer clean-install for lockfile reproducibility; fall back to install.
  # Always invoke the Node >= 18 binaries explicitly (nvm may still own `node`/`npm` names).
  (
    cd "${site_dir}"
    if [[ -f package-lock.json ]]; then
      "$NPM_BIN" clean-install --no-fund --no-audit || "$NPM_BIN" install --no-fund --no-audit
    else
      "$NPM_BIN" install --no-fund --no-audit
    fi
    "$NPM_BIN" run build
  )
  [[ -d "${site_dir}/dist" ]] && [[ -n "$(ls -A "${site_dir}/dist" 2>/dev/null)" ]] \
    || die "site/dist missing after npm run build — bisonw cannot embed UI assets"
  log "Web UI site/dist OK"
}

build_bisonw() {
  mkdir -p "${UI_ROOT}"
  build_site
  log "Building bisonw (simnet lock times: taker 3m, maker 6m)..."
  local bw_dir="${REPO_DIR}/client/cmd/bisonw"
  # Default build is !systray — UI is browser-only (no tray). Perfect for this use case.
  (cd "${bw_dir}" && go build -o "${UI_ROOT}/bisonw" -ldflags \
    "-X 'decred.org/dcrdex/dex.testLockTimeTaker=3m' \
     -X 'decred.org/dcrdex/dex.testLockTimeMaker=6m'")
  [[ -x "${UI_ROOT}/bisonw" ]] || die "bisonw binary not produced at ${UI_ROOT}/bisonw"
  "${UI_ROOT}/bisonw" --version 2>&1 | head -1 || true
}

write_ctl_helpers() {
  mkdir -p "${CTL_DIR}"

  cat > "${CTL_DIR}/mine-dcr" <<'EOF'
#!/usr/bin/env bash
N="${1:-1}"
exec ~/dextest/dcr/harness-ctl/mine-alpha "$N"
EOF
  chmod +x "${CTL_DIR}/mine-dcr"

  cat > "${CTL_DIR}/mine-lbc" <<'EOF'
#!/usr/bin/env bash
N="${1:-1}"
exec ~/dextest/lbc/harness-ctl/mine-alpha "$N"
EOF
  chmod +x "${CTL_DIR}/mine-lbc"

  cat > "${CTL_DIR}/unlock-lbc" <<'EOF'
#!/usr/bin/env bash
for w in alpha beta gamma; do
  ~/dextest/lbc/harness-ctl/$w walletpassphrase abc 100000000 || true
done
echo "LBC wallets unlocked (passphrase abc, long timeout)"
EOF
  chmod +x "${CTL_DIR}/unlock-lbc"

  cat > "${CTL_DIR}/open-ui" <<EOF
#!/usr/bin/env bash
URL1="http://${WEB1}"
URL2="http://${WEB2}"
echo "Wallet 1: \$URL1"
echo "Wallet 2: \$URL2"
if command -v xdg-open >/dev/null 2>&1; then
  xdg-open "\$URL1" >/dev/null 2>&1 || true
  xdg-open "\$URL2" >/dev/null 2>&1 || true
elif command -v sensible-browser >/dev/null 2>&1; then
  sensible-browser "\$URL1" >/dev/null 2>&1 || true
  sensible-browser "\$URL2" >/dev/null 2>&1 || true
fi
EOF
  chmod +x "${CTL_DIR}/open-ui"

  cat > "${CTL_DIR}/balances" <<'EOF'
#!/usr/bin/env bash
echo "=== DCR trading wallets ==="
~/dextest/dcr/harness-ctl/trading1 getbalance 2>/dev/null || true
~/dextest/dcr/harness-ctl/trading2 getbalance 2>/dev/null || true
echo "=== LBC wallets ==="
for w in alpha beta gamma; do
  echo -n "$w: "
  ~/dextest/lbc/harness-ctl/$w getbalance 2>/dev/null | python3 -c "
import sys,json
try:
  r=json.load(sys.stdin).get('result')
  print(r)
except Exception as e:
  print('?', e)
" || echo "?"
done
EOF
  chmod +x "${CTL_DIR}/balances"

  cat > "${CTL_DIR}/quit" <<EOF
#!/usr/bin/env bash
set +e
echo "Stopping bisonw..."
tmux kill-session -t ${SESSION_BW} 2>/dev/null
if [[ -f "${UI_ROOT}/bisonw1.pid" ]]; then kill "\$(cat "${UI_ROOT}/bisonw1.pid")" 2>/dev/null; fi
if [[ -f "${UI_ROOT}/bisonw2.pid" ]]; then kill "\$(cat "${UI_ROOT}/bisonw2.pid")" 2>/dev/null; fi
echo "Stopping dcrdex..."
if [[ -f "${HOME}/dextest/dcrdex/dcrdex.pid" ]]; then
  kill "\$(cat "${HOME}/dextest/dcrdex/dcrdex.pid")" 2>/dev/null
fi
echo "Stopping harnesses..."
~/dextest/lbc/harness-ctl/quit 2>/dev/null
~/dextest/dcr/harness-ctl/quit 2>/dev/null
tmux kill-session -t lbc-harness 2>/dev/null
tmux kill-session -t dcr-harness 2>/dev/null
tmux kill-session -t dcrdex-harness 2>/dev/null
echo "All stopped."
EOF
  chmod +x "${CTL_DIR}/quit"

  cat > "${CTL_DIR}/GUIDE.txt" <<EOF
LBC + DCR manual UI simnet
==========================

Bison Wallet UI
---------------
  Wallet 1 (maker-ish):  http://${WEB1}
  Wallet 2 (taker-ish):  http://${WEB2}

  Logs:  ${BW1_DIR}/simnet/  and  ${BW2_DIR}/simnet/
  Open:  ${CTL_DIR}/open-ui

DEX server
----------
  Host:   127.0.0.1:17273
  Cert:   ${HOME}/dextest/dcrdex/rpc.cert
  Market: dcr_lbc (lot size 1 DCR)
  Admin:  https://127.0.0.1:16542  (user u / pass adminpass)

Connect wallets in the UI (simnet / external RPC)
-------------------------------------------------
Wallet 1 — DCR (trading1, funded by harness):
  Type:     dcrwalletRPC / Decred
  Config:   ${HOME}/dextest/dcr/trading1/trading1.conf
  Password: abc

Wallet 1 — LBC (beta node wallet):
  Type:     lbcwalletRPC / LBRY Credits
  Config:   ${HOME}/dextest/lbc/beta/beta.conf
  Password: abc

Wallet 2 — DCR (trading2):
  Config:   ${HOME}/dextest/dcr/trading2/trading2.conf
  Password: abc

Wallet 2 — LBC (gamma):
  Config:   ${HOME}/dextest/lbc/gamma/gamma.conf
  Password: abc

Suggested flow
--------------
  1. Open both UI URLs; create app password (any; local only).
  2. Create/connect DCR + LBC wallets as above for each instance.
  3. Add DEX server 127.0.0.1:17273 — paste/select rpc.cert above.
  4. Post bond with DCR (server bondAmt is 0.5 DCR, 1 conf).
  5. Place orders on market dcr_lbc (e.g. sell 2 DCR @ 1.5 LBC/DCR).
  6. Background miners confirm swaps; or run:
       ${CTL_DIR}/mine-dcr 1
       ${CTL_DIR}/mine-lbc 1

Helpers
-------
  ${CTL_DIR}/mine-dcr [n]
  ${CTL_DIR}/mine-lbc [n]
  ${CTL_DIR}/unlock-lbc
  ${CTL_DIR}/balances
  ${CTL_DIR}/open-ui
  ${CTL_DIR}/quit

Tmux
----
  tmux attach -t lbc-harness
  tmux attach -t dcr-harness
  tmux attach -t ${SESSION_BW}
EOF
}

start_bisonw_instances() {
  log "Preparing bisonw appdata under ${UI_ROOT}..."
  mkdir -p "${UI_ROOT}"
  if [[ "${KEEP_WALLETS:-}" != "1" ]]; then
    rm -rf "${BW1_DIR}" "${BW2_DIR}"
  fi
  mkdir -p "${BW1_DIR}" "${BW2_DIR}"

  # Conf files (web + optional RPC for bwctl)
  cat > "${BW1_DIR}/dexc.conf" <<EOF
webaddr=${WEB1}
rpc=1
rpcuser=user
rpcpass=pass
rpcaddr=${RPC1}
rpccert=${BW1_DIR}/rpc.cert
rpckey=${BW1_DIR}/rpc.key
simnet=1
log=info
loglocal=true
EOF

  cat > "${BW2_DIR}/dexc.conf" <<EOF
webaddr=${WEB2}
rpc=1
rpcuser=user
rpcpass=pass
rpcaddr=${RPC2}
rpccert=${BW2_DIR}/rpc.cert
rpckey=${BW2_DIR}/rpc.key
simnet=1
log=info
loglocal=true
EOF

  write_ctl_helpers

  tmux kill-session -t "${SESSION_BW}" 2>/dev/null || true
  tmux new-session -d -s "${SESSION_BW}" -n harness-ctl bash
  tmux send-keys -t "${SESSION_BW}:0" "cd ${CTL_DIR}; clear; cat GUIDE.txt" C-m

  log "Starting bisonw #1 on http://${WEB1} ..."
  tmux new-window -t "${SESSION_BW}:1" -n bisonw1 bash
  tmux send-keys -t "${SESSION_BW}:1" "cd ${BW1_DIR}" C-m
  tmux send-keys -t "${SESSION_BW}:1" \
    "${UI_ROOT}/bisonw --appdata=${BW1_DIR} --config=${BW1_DIR}/dexc.conf --simnet" C-m

  if [[ "${ONE_WALLET:-}" != "1" ]]; then
    log "Starting bisonw #2 on http://${WEB2} ..."
    tmux new-window -t "${SESSION_BW}:2" -n bisonw2 bash
    tmux send-keys -t "${SESSION_BW}:2" "cd ${BW2_DIR}" C-m
    tmux send-keys -t "${SESSION_BW}:2" \
      "${UI_ROOT}/bisonw --appdata=${BW2_DIR} --config=${BW2_DIR}/dexc.conf --simnet" C-m
  fi

  # Wait until web ports accept connections
  wait_http() {
    local hostport="$1" name="$2" i
    local host="${hostport%:*}"
    local port="${hostport##*:}"
    for i in $(seq 1 90); do
      if curl -sf "http://${hostport}/" >/dev/null 2>&1 \
        || curl -sf "http://${hostport}/api/config" >/dev/null 2>&1 \
        || (echo >/dev/tcp/"${host}"/"${port}") 2>/dev/null; then
        log "${name} UI ready: http://${hostport}"
        return 0
      fi
      sleep 1
    done
    warn "${name} UI not responding yet at http://${hostport} — check tmux ${SESSION_BW}"
    return 1
  }

  wait_http "${WEB1}" "bisonw1" || true
  if [[ "${ONE_WALLET:-}" != "1" ]]; then
    wait_http "${WEB2}" "bisonw2" || true
  fi
}

print_banner() {
  cat <<EOF

╔══════════════════════════════════════════════════════════════════════╗
║  LBC manual UI simnet is READY                                       ║
╠══════════════════════════════════════════════════════════════════════╣
║  Open in browser:                                                    ║
║    Wallet 1:  http://${WEB1}
║    Wallet 2:  http://${WEB2}
║                                                                      ║
║  Or run:  ${CTL_DIR}/open-ui
║                                                                      ║
║  DEX:     127.0.0.1:17273                                            ║
║  Cert:    ${HOME}/dextest/dcrdex/rpc.cert
║  Market:  dcr_lbc                                                    ║
║                                                                      ║
║  Miners are running in the background (unless NO_MINER=1).           ║
║  Manual mine:  ${CTL_DIR}/mine-dcr 1
║                ${CTL_DIR}/mine-lbc 1
║                                                                      ║
║  Full guide:   ${CTL_DIR}/GUIDE.txt
║  Stop all:     ${CTL_DIR}/quit
║  bisonw tmux:  tmux attach -t ${SESSION_BW}
╚══════════════════════════════════════════════════════════════════════╝

Tip: create an app password in each UI, connect DCR trading1/trading2 and
LBC beta/gamma via the conf paths in GUIDE.txt, then add the DEX cert.

This terminal will stay up (Ctrl+C stops bisonw+harnesses+dcrdex).
EOF
}

# ---------------------------------------------------------------------------
main() {
  log "Manual LBC Bison Wallet UI simnet starting"
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
    # node/npm required only if site/dist is missing (first bisonw build)
    if [[ ! -d "${REPO_DIR:-/nonexistent}/client/webserver/site/dist" ]]; then
      ensure_node
    fi
  fi

  resolve_repo
  [[ -f "${REPO_DIR}/go.mod" ]] || die "invalid REPO_DIR=${REPO_DIR}"

  # Clean any previous full-test leftovers that would conflict
  tmux kill-session -t lbc-bisonw 2>/dev/null || true

  start_dcr_harness
  start_lbc_harness
  start_dcrdex

  if [[ "${RUN_AUTOTEST:-}" == "1" ]]; then
    run_autotests
  fi

  start_background_miners
  build_bisonw
  start_bisonw_instances

  RUNNING=1
  print_banner

  if [[ "${OPEN_BROWSER:-}" == "1" ]]; then
    "${CTL_DIR}/open-ui" || true
  fi

  # Block until Ctrl+C — stack stays up for manual testing
  log "Idle — press Ctrl+C when finished testing (runs ${CTL_DIR}/quit)"
  STOP_ON_EXIT=1
  local unlock_every=300 tick=0
  while true; do
    # keep LBC wallets unlocked (lbcwallet may re-lock)
    if (( tick % unlock_every == 0 )); then
      for w in alpha beta gamma; do
        "${HOME}/dextest/lbc/harness-ctl/$w" walletpassphrase abc 100000000 >/dev/null 2>&1 || true
      done
    fi
    if ! tmux has-session -t "${SESSION_BW}" 2>/dev/null; then
      warn "bisonw tmux session ended"
      break
    fi
    sleep 5
    tick=$((tick + 5))
  done
}

main "$@"
