# Testing LBC on DCRDEX (simnet)

Checklist for verifying the LBC asset end-to-end on simnet.

## What this branch adds

- LBRY Credits as BIP44 asset **140**
- Client: external **lbcwallet** RPC (`client/asset/lbc`)
- Server: **lbcd** backend (`server/asset/lbc`)
- Network params + ClaimTrie block deserializer (`dex/networks/lbc`)
- Regtest harness (`dex/testing/lbc`)
- UI coin icon `lbc.png`

## Prerequisites

| Tool | Notes |
|------|--------|
| Go 1.24+ | Matches `dcrdex` `go.mod` |
| `tmux`, `curl`, `python3` | Harness helpers |
| PostgreSQL | Only for full **dcrdex server** harness |
| `dcrd`, `dcrwallet`, `dcrctl` | DCR simnet harness ([binaries](https://github.com/decred/decred-binaries/releases)) |
| `lbcd`, `lbcctl`, `lbcwallet` | Build from [lbryio/lbcd](https://github.com/lbryio/lbcd) and [lbryio/lbcwallet](https://github.com/lbryio/lbcwallet) |

```bash
# Example: build LBC tools into PATH
git clone --depth 1 https://github.com/lbryio/lbcd.git
git clone --depth 1 https://github.com/lbryio/lbcwallet.git
(cd lbcd && go build -o ~/bin/lbcd .)
(cd lbcd/cmd/lbcctl && go build -o ~/bin/lbcctl .)
(cd lbcwallet && go build -o ~/bin/lbcwallet .)
export PATH=~/bin:$PATH
```

---

## Level 1 — Unit tests (fast, no daemons)

```bash
cd dcrdex   # this repo
go test ./dex/networks/lbc/ -count=1
go build ./client/asset/lbc/
go build ./server/asset/lbc/
```

**Expect:** all pass / build clean.

---

## Level 2 — LBC wallet harness + client livetest

This is the main LBC-specific check maintainers asked for.

### 2a. Start LBC harness

```bash
export PATH=...  # lbcd, lbcctl, lbcwallet on PATH
cd dex/testing/lbc
NOATTACH=1 ./harness.sh
# or omit NOATTACH to attach tmux session "lbc-harness"
```

Creates `~/dextest/lbc/` with:

| Path | Purpose |
|------|---------|
| `alpha/alpha.conf` | Client wallet RPC (Bison / livetest) |
| `beta/beta.conf` | Second client wallet RPC |
| `alpha/alpha-node.conf` | **Server** node RPC (`lbcd`) |
| `harness-ctl/` | `./alpha`, `./beta`, `./mine-alpha`, `./quit` |

Wallet passphrase after harness setup: **`abc`** (livetest default).

### 2b. Spot-check wallets

```bash
cd ~/dextest/lbc/harness-ctl
./alpha-node getblockcount
./alpha getbalance
./beta getbalance
./mine-alpha 1
```

### 2c. Client integration test

```bash
cd client/asset/lbc
go test -v -count=1 -tags=harness -run TestWallet -timeout 180s
```

**Expect (core path):** FundOrder, Swap, AuditContract, Redeem, FindRedemption, Refund, Send succeed.  
**Note:** `Withdraw` can flake if a prior Send still sits in mempool reusing a UTXO; mine a block and re-run if needed. That does not block the swap path.

### 2d. Stop LBC harness

```bash
~/dextest/lbc/harness-ctl/quit
```

---

## Level 3 — Full simnet: DCR + LBC + dcrdex server

Requires DCR harness + Postgres + built `dcrdex` binary.

### 3a. Postgres (once)

```bash
sudo -u postgres createuser -P dcrdex   # password e.g. dexpass
sudo -u postgres createdb -O dcrdex dcrdex_simnet_test
# or let dex/testing/dcrdex/harness.sh recreate the DB
```

### 3b. Start DCR harness

```bash
# dcrd, dcrwallet, dcrctl on PATH
cd dex/testing/dcr
./harness.sh
# leaves tmux session "dcr-harness", data under ~/dextest/dcr
```

### 3c. Start LBC harness (if not already)

```bash
cd dex/testing/lbc
NOATTACH=1 ./harness.sh
```

### 3d. Start dcrdex server harness

```bash
cd dex/testing/dcrdex
PG_PASS=dexpass ./harness.sh
# builds server with simnet lock times, writes markets.json, starts dcrdex
# RPC listen: 127.0.0.1:17273
# Admin API: 127.0.0.1:16542 (user u / pass adminpass)
```

`genmarkets.sh` auto-includes **LBC** when `~/dextest/lbc/harness-ctl/alpha-node getblockchaininfo` succeeds:

- Market: **DCR_simnet / LBC_simnet**
- Asset config: `~/dextest/lbc/alpha/alpha-node.conf`

### 3e. Verify server sees both assets

```bash
# logs in tmux window dcrdex-harness:0, or:
ls ~/dextest/dcrdex/data/simnet/logs/
# Look for LBC backend connect / market DCR_LBC without fatal errors

~/dextest/dcrdex/dexadm config   # if admin scripts present
```

### 3f. Manual client smoke (optional)

Build Bison with this branch:

```bash
cd client/cmd/bisonw
go build -o bisonw .
# Point LBC external wallet at ~/dextest/lbc/alpha/alpha.conf
# Point DCR wallet at dcr harness trading wallets
# Register on simnet DEX 127.0.0.1:17273 with ~/dextest/dcrdex/rpc.cert
# Place a small DCR/LBC order if market is live
```

### 3g. Shutdown

```bash
~/dextest/dcrdex/quit
~/dextest/lbc/harness-ctl/quit
~/dextest/dcr/harness-ctl/quit   # if present
```

---

## Architecture reminder

| Role | Process | Default mainnet RPC | Simnet harness ports (this harness) |
|------|---------|---------------------|--------------------------------------|
| Full node | `lbcd` | 9245 | alpha **39245** |
| Wallet | `lbcwallet` | 9244 | alpha **39244**, beta **39249** |
| Client (Bison) | talks to **wallet** | — | `alpha.conf` / `beta.conf` |
| Server (dcrdex) | talks to **node** | — | `alpha-node.conf` |

`lbcwallet` must be connected to `lbcd` (chain passthrough for many RPCs).

---

## Known limitations (reviewers)

1. **SegWit = false (MVP)** — `lbcwallet` `getrawchangeaddress` is account-first; DCRDEX would pass `"bech32"` as account. Non-segwit P2SH swaps still work on LBC. Follow-up: enable SegWit after RPC arg fix.
2. LBC **block headers are 112 bytes** (ClaimTrie). Custom deserializer required; `MsgBlock.BlockHash()` from deserialised headers is not the true LBC hash (RPC supplies real hashes).
3. No native SPV LBC wallet yet — external `lbcwallet` only.

---

## PR / review checklist

- [ ] Level 1 unit/build
- [ ] Level 2 harness + `TestWallet` (swap/redeem/refund)
- [ ] Level 3 dcrdex starts with DCR + LBC markets, no backend panics
- [ ] (Optional) Manual order on simnet DCR/LBC

Questions about LBC chain params / wallet RPCs: ask Ben / LBRY foundation.
