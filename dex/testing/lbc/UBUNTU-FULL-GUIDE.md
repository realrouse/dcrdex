# Complete Ubuntu guide: test LBC on DCRDEX (local simnet)

This document is **Linux / Ubuntu** oriented. It covers prerequisites through
**deposits, bonds, and a successful atomic swap** on a local **LBC/DCR** market.

| Audience | Path |
|----------|------|
| **One command (automated)** | [run-full-simnet-test.sh](./run-full-simnet-test.sh) |
| **Manual Bison Wallet UI** | [run-manual-ui-simnet.sh](./run-manual-ui-simnet.sh) |
| Short overview | [README.md](./README.md) |
| Level report (what maintainers ran) | [LEVEL-TEST-REPORT.md](./LEVEL-TEST-REPORT.md) |

---

## What you will run

```
┌─────────────┐     ┌─────────────┐
│  dcrd       │     │  lbcd ×3    │  (alpha/beta/gamma)
│  dcrwallet  │     │  lbcwallet×3│
└──────┬──────┘     └──────┬──────┘
       │                   │
       └─────────┬─────────┘
                 ▼
         ┌───────────────┐
         │  dcrdex       │  market lbc_dcr @ 127.0.0.1:17273
         └───────┬───────┘
                 ▼
    simnet-trade-tests (2 clients)
    → fund wallets (deposits)
    → post bonds (DCR)
    → place orders / match / swap / redeem
```

---

## Option A2 — Manual Bison Wallet UI (you place the trades)

Starts DCR + LBC harnesses, `dcrdex` (`lbc_dcr`), background miners, and **two**
`bisonw` instances. Open the printed `http://127.0.0.1:…` URLs in a browser and
drive deposits / bonds / swaps yourself.

```bash
cd ~/projects/crypto/lbc-dcrdex/dcrdex   # or your checkout
SKIP_INSTALL=1 ./dex/testing/lbc/run-manual-ui-simnet.sh
# optional:
#   OPEN_BROWSER=1 ONE_WALLET=1 RUN_AUTOTEST=1 ./dex/testing/lbc/run-manual-ui-simnet.sh
```

| URL / path | Purpose |
|------------|---------|
| `http://127.0.0.1:5760` | Bison Wallet #1 UI |
| `http://127.0.0.1:5762` | Bison Wallet #2 UI |
| `~/dextest/dcrdex/rpc.cert` | TLS cert when adding DEX `127.0.0.1:17273` |
| `~/dextest/dcr/trading1/trading1.conf` | Wallet1 DCR (pass `abc`) |
| `~/dextest/lbc/beta/beta.conf` | Wallet1 LBC (pass `abc`) |
| `~/dextest/dcr/trading2/trading2.conf` | Wallet2 DCR (pass `abc`) |
| `~/dextest/lbc/gamma/gamma.conf` | Wallet2 LBC (pass `abc`) |
| `~/dextest/lbc-ui/ctl/GUIDE.txt` | Full manual guide written at startup |
| `~/dextest/lbc-ui/ctl/quit` | Stop everything |

Ctrl+C in the script terminal also stops the stack.

---

## Option A — Automated (recommended for CI / full verification)

From a machine with `sudo` (Ubuntu 22.04 / 24.04):

```bash
# Get the branch
git clone --branch lbc --single-branch \
  https://github.com/realrouse/dcrdex.git
cd dcrdex

# Full install + harnesses + server + tests
chmod +x dex/testing/lbc/run-full-simnet-test.sh
./dex/testing/lbc/run-full-simnet-test.sh
```

### Useful env vars

| Variable | Meaning |
|----------|---------|
| `SKIP_INSTALL=1` | Do not apt/go/download binaries (already installed) |
| `SKIP_TRADE=1` | Skip bond/swap simnet-trade-tests (only unit + wallet livetest) |
| `KEEP_RUNNING=1` | Leave harnesses and dcrdex running when the script exits |
| `PG_PASS=dexpass` | Postgres password for user `dcrdex` |
| `REPO_DIR=/path` | Use an existing checkout instead of cloning |

Example (tools already installed, leave stack up for manual browsing):

```bash
SKIP_INSTALL=1 KEEP_RUNNING=1 ./dex/testing/lbc/run-full-simnet-test.sh
```

The script will:

1. Install packages, Go 1.24, Decred tools, build `lbcd`/`lbcwallet`
2. Start **DCR** harness (`NOMINER=1`)
3. Start **LBC** harness (alpha/beta/gamma wallets)
4. Build and start **dcrdex** with market **LBC_simnet / DCR_simnet**
5. Run unit tests
6. Run LBC client `TestWallet` (deposit-like fund, swap, redeem, refund, send, withdraw)
7. Run `./run lbcdcr -t success` (bonds + full trade)

---

## Option B — Manual step-by-step

### B0. System packages

```bash
sudo apt-get update
sudo apt-get install -y build-essential curl git jq tmux python3 \
  ca-certificates postgresql postgresql-contrib tar gzip

# Go 1.24+
curl -fsSL -o /tmp/go.tgz https://go.dev/dl/go1.24.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz
echo 'export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH' >> ~/.bashrc
source ~/.bashrc
go version
```

### B1. Postgres for dcrdex

```bash
sudo service postgresql start
sudo -u postgres psql -c "CREATE USER dcrdex WITH PASSWORD 'dexpass' CREATEDB;" || true
sudo -u postgres psql -c "ALTER USER dcrdex WITH PASSWORD 'dexpass';"
sudo -u postgres psql -c "DROP DATABASE IF EXISTS dcrdex_simnet_test;"
sudo -u postgres psql -c "CREATE DATABASE dcrdex_simnet_test OWNER dcrdex;"
```

### B2. Decred tools (`dcrd`, `dcrwallet`, `dcrctl`)

```bash
mkdir -p ~/bin
cd /tmp
curl -fsSL -o decred.tar.gz \
  https://github.com/decred/decred-binaries/releases/download/v2.1.5/decred-linux-amd64-v2.1.5.tar.gz
tar -xzf decred.tar.gz
cp decred-linux-amd64-v2.1.5/{dcrd,dcrwallet,dcrctl} ~/bin/
export PATH=~/bin:$PATH
dcrd --version
```

### B3. LBC tools (`lbcd`, `lbcctl`, `lbcwallet`)

```bash
mkdir -p ~/src && cd ~/src
git clone --depth 1 https://github.com/lbryio/lbcd.git
git clone --depth 1 https://github.com/lbryio/lbcwallet.git
(cd lbcd && go build -o ~/bin/lbcd .)
(cd lbcd/cmd/lbcctl && go build -o ~/bin/lbcctl .)
(cd lbcwallet && go build -o ~/bin/lbcwallet .)
export PATH=~/bin:$PATH
```

### B4. Get this branch

```bash
cd ~
git clone --branch lbc --single-branch \
  https://github.com/realrouse/dcrdex.git
cd dcrdex
```

### B5. Start DCR harness

Background miners interfere with confirmation counting in trade tests.

```bash
export PATH=~/bin:/usr/local/go/bin:$PATH
export NOMINER=1
cd ~/dcrdex/dex/testing/dcr
# non-interactive: comment attach at end, or Ctrl-b d after start
./harness.sh
# verify:
~/dextest/dcr/harness-ctl/alpha getbalance
```

### B6. Start LBC harness

```bash
export NOMINER=1
cd ~/dcrdex/dex/testing/lbc
NOATTACH=1 ./harness.sh
# verify (beta + gamma MUST show balance > 0 — client2 uses gamma):
~/dextest/lbc/harness-ctl/alpha-node getblockcount
~/dextest/lbc/harness-ctl/beta getbalance
~/dextest/lbc/harness-ctl/gamma getbalance
# wallets use passphrase: abc
#
# If gamma is 0, top up before trade tests:
#   ADDR=$(~/dextest/lbc/harness-ctl/gamma getnewaddress | python3 -c "import sys,json;print(json.load(sys.stdin)['result'])")
#   ~/dextest/lbc/harness-ctl/alpha walletpassphrase abc 600
#   for a in 25 25 20 20; do ~/dextest/lbc/harness-ctl/alpha sendtoaddress "$ADDR" $a; done
#   ~/dextest/lbc/harness-ctl/mine-alpha 3
```

Creates:

| Path | Role |
|------|------|
| `~/dextest/lbc/alpha/alpha.conf` | Client wallet RPC |
| `~/dextest/lbc/beta/beta.conf` | Client 1 quote wallet (simnet-trade) |
| `~/dextest/lbc/gamma/gamma.conf` | Client 2 quote wallet |
| `~/dextest/lbc/alpha/alpha-node.conf` | **Server** node RPC |

### B7. Start dcrdex with LBC/DCR market

Easiest: use the automated script’s `start_dcrdex` logic, **or** the built-in harness after genmarkets sees LBC:

```bash
cd ~/dcrdex/dex/testing/dcrdex
PG_PASS=dexpass NOMINER=1 ./harness.sh
```

Or a minimal custom markets file (server binary built with short lock times):

```bash
APP=~/dextest/dcrdex
mkdir -p "$APP"
cd ~/dcrdex/server/cmd/dcrdex
go build -o "$APP/dcrdex" -ldflags \
  "-X 'decred.org/dcrdex/dex.testLockTimeTaker=3m' \
   -X 'decred.org/dcrdex/dex.testLockTimeMaker=6m'"

# write dcrdex.conf + markets.json (DCR_simnet + LBC_simnet) — see run-full-simnet-test.sh
# then:
$APP/dcrdex --appdata=$APP --configfile=$APP/dcrdex.conf \
  --marketsconfpath=$APP/markets.json --simnet --pgpass=dexpass
```

**Check market is live:**

```bash
curl -sk --basic -u u:adminpass https://127.0.0.1:16542/api/config | jq '.assets[].symbol, .markets[].name'
# expect: dcr, lbc, lbc_dcr
```

Logs: `~/dextest/dcrdex/dcrdex-stdout.log` or `~/dextest/dcrdex/logs/simnet/`

### B8. Level 1 — unit tests

```bash
cd ~/dcrdex
go test ./dex/networks/lbc/ -count=1
go build ./client/asset/lbc/ ./server/asset/lbc/
```

### B9. Level 2 — deposits / local swap path (no server)

Unlock wallets and run livetest:

```bash
~/dextest/lbc/harness-ctl/alpha walletpassphrase abc 1000000
~/dextest/lbc/harness-ctl/beta walletpassphrase abc 1000000
cd ~/dcrdex/client/asset/lbc
go test -v -count=1 -tags=harness -run TestWallet -timeout 180s
```

**What this exercises:** connect external wallets, balances (funded UTXOs = “deposits” into test wallets), **swap**, **redeem**, **refund**, **send**, **withdraw**.

### B10. Level 3 — bonds + full DEX swap (server)

```bash
cd ~/dcrdex/client/cmd/simnet-trade-tests
# unlock LBC wallets used by clients
for w in alpha beta gamma; do
  ~/dextest/lbc/harness-ctl/$w walletpassphrase abc 1000000
done

./run lbcdcr -t success -runonce -debug
```

**What this exercises:**

| Step | Meaning |
|------|---------|
| Wallet create / connect | DCR trading1/trading2 + LBC beta/gamma |
| Fund from harness alpha | **Deposits** into client wallets |
| Bond post with `--regasset dcr` | **Bonds** on the simnet DEX |
| Place maker/taker orders | Order book on `lbc_dcr` |
| Match → swap → redeem | **Atomic swap** success path |

Other scenarios: `./run lbcdcr --all` (see [simnet-trade-tests README](../../client/cmd/simnet-trade-tests/README.md)).

### B11. Shutdown

```bash
# if using custom pid
kill $(cat ~/dextest/dcrdex/dcrdex.pid) 2>/dev/null || true
~/dextest/dcrdex/quit 2>/dev/null || true
~/dextest/lbc/harness-ctl/quit
~/dextest/dcr/harness-ctl/quit
```

---

## Ports cheat sheet (this harness)

| Service | Host:port |
|---------|-----------|
| dcrdex RPC | `127.0.0.1:17273` |
| dcrdex admin | `127.0.0.1:16542` (u / adminpass) |
| dcrd alpha | `127.0.0.1:19561` |
| lbcd alpha (server) | `127.0.0.1:39245` |
| lbcwallet alpha/beta/gamma | `39244` / `39249` / `39252` |

Wallet passphrase for harness wallets: **`abc`**.

---

## Architecture notes

- **Client** connects to **lbcwallet** (wallet RPC).  
- **Server** connects to **lbcd** (node RPC, `txindex`).  
- **SegWit is off** for LBC in this MVP (lbcwallet change-address RPC quirk). Swaps use P2SH contracts.  
- LBC block headers include a **ClaimTrie** (112 bytes); custom deserializer is required.

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `go: module requires go 1.24` | Install Go 1.24+ as above |
| Port already in use | `tmux ls`; run harness `quit` scripts; kill leftover `lbcd`/`dcrd`/`dcrdex` |
| `getwalletinfo` / protocol version | Expected; LBC client sets `OptionalWalletInfo` + `MinProtocolVersion` |
| Trade test “coin locked” | Wait a few seconds or wipe `dcrdex_simnet_test` DB and restart server |
| Withdraw flaky in livetest | Ensure you are on a commit that mines after Send; mine: `~/dextest/lbc/harness-ctl/mine-alpha 1` |
| Postgres auth | Set `pgpass` / `PG_PASS` and ensure role `dcrdex` owns the DB |
| Background miner skews confs | Always use `NOMINER=1` for trade tests |

---

## Mapping tests → product features

| Feature | How it is tested |
|---------|------------------|
| Install / build | Script sections 1–2; Level 1 |
| External wallet connect | Level 2 `TestWallet` connect |
| Deposit / fund | Harness `sendtoaddress` + simnet-trade fund |
| Balances | Level 2 + trade test balance checks |
| Bonds | `simnet-trade-tests` registration asset DCR |
| Order placement | simnet-trade success |
| Atomic swap + redeem | simnet-trade success + Level 2 Swap/Redeem |
| Refund path | Level 2 Refund; trade tests `notakerswap` etc. |

---

## Sending this to a reviewer (Ben)

```text
Branch: https://github.com/realrouse/dcrdex/tree/lbc

One-shot on Ubuntu:
  git clone --branch lbc https://github.com/realrouse/dcrdex.git
  cd dcrdex && ./dex/testing/lbc/run-full-simnet-test.sh

Manual guide:
  dex/testing/lbc/UBUNTU-FULL-GUIDE.md
```
