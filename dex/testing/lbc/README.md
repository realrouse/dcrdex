# LBC test harness

Regtest harness for LBRY Credits: three `lbcd` nodes (`alpha`, `beta`, `gamma`)
and matching `lbcwallet` processes.

## Prerequisites

`lbcd`, `lbcctl`, and `lbcwallet` on `PATH`, plus `tmux`, `curl`, and `python3`.

```bash
git clone --depth 1 https://github.com/lbryio/lbcd.git
git clone --depth 1 https://github.com/lbryio/lbcwallet.git
(cd lbcd && go build -o ~/bin/lbcd .)
(cd lbcd/cmd/lbcctl && go build -o ~/bin/lbcctl .)
(cd lbcwallet && go build -o ~/bin/lbcwallet .)
export PATH=~/bin:$PATH
```

## Start

```bash
cd dex/testing/lbc
NOMINER=1 NOATTACH=1 ./harness.sh
```

Creates `~/dextest/lbc/` and tmux session `lbc-harness`.

LBC pays 1 LBC per block for the first 5100 heights and has 100-block coinbase
maturity. The harness mines enough blocks to fund `beta` and `gamma`.

## Layout

| Path | Role |
|------|------|
| `~/dextest/lbc/alpha/alpha.conf` | Client wallet RPC (Bison / livetest) |
| `~/dextest/lbc/beta/beta.conf` | Client 1 quote wallet |
| `~/dextest/lbc/gamma/gamma.conf` | Client 2 quote wallet |
| `~/dextest/lbc/alpha/alpha-node.conf` | Node RPC for the dcrdex backend |
| `~/dextest/lbc/harness-ctl/` | `alpha`, `beta`, `gamma`, `mine-alpha`, `quit` |

Wallet passphrase: `abc`.

Client software talks to **lbcwallet**. The server backend talks to **lbcd**.

## Livetest

```bash
cd client/asset/lbc
go test -v -count=1 -tags=harness -run TestWallet
```

## Simnet trade test

With the DCR harness, this LBC harness, and dcrdex (`dcr_lbc`) running:

```bash
cd client/cmd/simnet-trade-tests
./run dcrlbc -t success -runonce
```

## Extra scripts

- `run-full-simnet-test.sh` — install, harnesses, dcrdex, livetest, success trade
- `run-manual-ui-simnet.sh` — same stack plus Bison Wallet web UIs
- [TESTING.md](TESTING.md) — review checklist
- [UBUNTU-FULL-GUIDE.md](UBUNTU-FULL-GUIDE.md) — Ubuntu walkthrough
