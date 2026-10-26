package mesh

import (
	"bytes"
	"testing"

	dexbtc "decred.org/dcrdex/dex/networks/btc"
	dexlbc "decred.org/dcrdex/dex/networks/lbc"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
)

// TestSwapMessageSizesLBC builds a real LBC (non-segwit P2SH, as the lbc
// asset uses) swap contract and checks every swap message carrying it fits
// the mesh 1 KiB publish cap with room to spare.
func TestSwapMessageSizesLBC(t *testing.T) {
	const budget = 1024 - 200
	for name, cp := range map[string]*chaincfg.Params{"mainnet": dexlbc.MainNetParams, "regtest": dexlbc.RegressionNetParams} {
		r, err := btcutil.NewAddressPubKeyHash(bytes.Repeat([]byte{1}, 20), cp)
		if err != nil {
			t.Fatal(err)
		}
		s, err := btcutil.NewAddressPubKeyHash(bytes.Repeat([]byte{2}, 20), cp)
		if err != nil {
			t.Fatal(err)
		}
		contract, err := dexbtc.MakeContract(r, s, bytes.Repeat([]byte{3}, 32), 1<<31, false, cp)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s LBC contract: %d bytes, address %s", name, len(contract), r)
		m := newFakeMesh()
		a, b := newTestBooks(t, m, nil), newTestBooks(t, m, nil)
		var id MatchID
		coin := make([]byte, 36) // txid + vout, as for BTC
		for msg, payload := range map[string]any{
			swapMsgInit:   &SwapInitMsg{MatchID: id, CoinID: coin, Contract: contract, SecretHash: make([]byte, 32)},
			swapMsgPart:   &SwapPartMsg{MatchID: id, CoinID: coin, Contract: contract},
			swapMsgRedeem: &SwapRedeemMsg{MatchID: id, CoinID: coin, Secret: make([]byte, 32)},
			swapMsgDone:   &SwapDoneMsg{MatchID: id, CoinID: coin},
		} {
			sealed, err := a.sealInbox(b.self, b.encPriv.PubKey(), msg, payload)
			if err != nil {
				t.Fatal(err)
			}
			n := len(sealed) + len(InboxTopic(b.self))
			t.Logf("  %s: %d bytes", msg, n)
			if n > budget {
				t.Fatalf("%s is %d bytes, budget %d", msg, n, budget)
			}
		}
	}
}
