//go:build !harness && !vspd

package dcr

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex/networks/dcr"
	"github.com/decred/dcrd/dcrec"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/wire"
)

// TestSwapBeforeBroadcast checks the BeforeBroadcast hook: it sees the final
// receipts before the broadcast and an error from it aborts the broadcast.
func TestSwapBeforeBroadcast(t *testing.T) {
	wallet, node, shutdown := tNewWallet()
	defer shutdown()
	privBytes, _ := hex.DecodeString("b07209eec1a8fb6cfe5cb6ace36567406971a75c330db7101fb21bc679bc5330")
	var err error
	node.privWIF, err = dcrutil.NewWIF(privBytes, tChainParams.PrivateKeyID, dcrec.STEcdsaSecp256k1)
	if err != nil {
		t.Fatal(err)
	}
	node.newAddr = tPKHAddr
	node.changeAddr = tPKHAddr
	node.signFunc = func(msgTx *wire.MsgTx) (*wire.MsgTx, bool, error) { return signFunc(msgTx, dcr.P2PKHSigScriptSize) }
	defer func() { node.signFunc = defaultSignFunc }()
	secretHash, _ := hex.DecodeString("5124208c80d33507befa517c08ed01aa8d33adbf37ecd70fb5f9352f7a51a88d")
	mkSwaps := func(hook func([]asset.Receipt) error) *asset.Swaps {
		return &asset.Swaps{
			Inputs: asset.Coins{newOutput(tTxHash, 0, toAtoms(3), wire.TxTreeRegular),
				newOutput(tTxHash, 1, toAtoms(3), wire.TxTreeRegular)},
			Contracts:       []*asset.Contract{{Address: tPKHAddr.String(), Value: toAtoms(5), SecretHash: secretHash, LockTime: uint64(time.Now().Unix())}},
			FeeRate:         tDCR.MaxFeeRate,
			BeforeBroadcast: hook,
		}
	}
	node.sentRawTx = nil
	if _, _, _, err := wallet.Swap(t.Context(), mkSwaps(func([]asset.Receipt) error { return errors.New("no") })); err == nil {
		t.Fatal("hook error did not abort")
	}
	if node.sentRawTx != nil {
		t.Fatal("broadcast despite hook error")
	}
	var seen []asset.Receipt
	receipts, _, _, err := wallet.Swap(t.Context(), mkSwaps(func(r []asset.Receipt) error {
		if node.sentRawTx != nil {
			t.Fatal("hook called after broadcast")
		}
		seen = r
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Coin().TxID() != node.sentRawTx.TxHash().String() ||
		string(seen[0].Contract()) != string(receipts[0].Contract()) {
		t.Fatal("hook receipts don't match the broadcast tx")
	}
}
