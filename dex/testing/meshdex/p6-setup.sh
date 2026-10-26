#!/usr/bin/env bash
# SIMNET/REGTEST ONLY. P6 wallet setup (idempotent): fresh bitcoind wallets
# p6w1..p6w4 on the btc harness alpha node, a "p6" account in dcrwallet
# trading1 and trading2, and LBC top-ups for lbcwallet beta and gamma, all
# funded from the harness miners. Peers (see p6-campaign.py):
#   p1: lbc beta,  btc p6w1, dcr trading1/default   markets dcr_btc lbc_btc lbc_dcr
#   p2: lbc gamma, btc p6w2, dcr trading2/default   markets dcr_btc lbc_btc lbc_dcr
#   p3:            btc p6w3, dcr trading1/p6        market  dcr_btc
#   p4:            btc p6w4, dcr trading2/p6        market  dcr_btc
set -euo pipefail
DEXTEST=${DEXTEST:-$HOME/dextest}
BTC="bitcoin-cli -rpcport=20556 -regtest=1 -rpcuser=user -rpcpassword=pass"
DCRCTL=$DEXTEST/dcr/harness-ctl
LBCCTL=$DEXTEST/lbc/harness-ctl
BTC_FUND=${BTC_FUND:-60}; DCR_FUND=${DCR_FUND:-600}; LBC_FUND=${LBC_FUND:-300}

for i in 1 2 3 4; do
  w=p6w$i
  if ! $BTC listwallets | grep -q "\"$w\""; then
    $BTC loadwallet "$w" >/dev/null 2>&1 || $BTC createwallet "$w" >/dev/null
  fi
  bal=$($BTC -rpcwallet=$w getbalance)
  if python3 -c "import sys; sys.exit(0 if $bal < $BTC_FUND/2 else 1)"; then
    $BTC -rpcwallet=default sendtoaddress "$($BTC -rpcwallet=$w getnewaddress)" "$BTC_FUND" >/dev/null
    echo "funded btc $w with $BTC_FUND"
  fi
done

for t in trading1 trading2; do
  "$DCRCTL/$t" walletpassphrase abc 0 >/dev/null 2>&1 || true
  "$DCRCTL/$t" getaccountaddress p6 >/dev/null 2>&1 || "$DCRCTL/$t" createnewaccount p6 >/dev/null
  bal=$("$DCRCTL/$t" getbalance p6 0 | python3 -c 'import sys,json; print(json.load(sys.stdin)["balances"][0]["total"])')
  if python3 -c "import sys; sys.exit(0 if $bal < $DCR_FUND/2 else 1)"; then
    "$DCRCTL/$t" sendfrom default "$("$DCRCTL/$t" getnewaddress p6)" "$DCR_FUND" >/dev/null
    echo "funded dcr $t/p6 with $DCR_FUND"
  fi
done

"$LBCCTL/alpha" walletpassphrase abc 600 >/dev/null
for w in beta gamma; do
  bal=$("$LBCCTL/$w" getbalance | python3 -c 'import sys,json; print(json.load(sys.stdin)["result"])')
  if python3 -c "import sys; sys.exit(0 if $bal < $LBC_FUND/2 else 1)"; then
    addr=$("$LBCCTL/$w" getnewaddress | python3 -c 'import sys,json; print(json.load(sys.stdin)["result"])')
    "$LBCCTL/alpha" sendtoaddress "$addr" "$LBC_FUND" >/dev/null
    echo "funded lbc $w with $LBC_FUND"
  fi
done
sleep 5
"$DCRCTL/mine-alpha" 2 >/dev/null; "$DEXTEST/btc/harness-ctl/mine-alpha" 2 >/dev/null; "$LBCCTL/mine-alpha" 3 >/dev/null
sleep 3
echo "balances:"
for i in 1 2 3 4; do echo "  btc p6w$i $($BTC -rpcwallet=p6w$i getbalance)"; done
for t in trading1 trading2; do for a in default p6; do
  echo "  dcr $t/$a $("$DCRCTL/$t" getbalance $a 0 | python3 -c 'import sys,json; print(json.load(sys.stdin)["balances"][0]["total"])')"; done; done
for w in beta gamma; do echo "  lbc $w $("$LBCCTL/$w" getbalance | python3 -c 'import sys,json; print(json.load(sys.stdin)["result"])')"; done
