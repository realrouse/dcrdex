package btc

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
)

// TestSwapBeforeBroadcast checks the BeforeBroadcast hook: it sees the final
// receipts before the broadcast and an error from it aborts the broadcast.
func TestSwapBeforeBroadcast(t *testing.T) {
	wallet, node, shutdown := tNewWallet(true, walletTypeRPC)
	defer shutdown()
	node.newAddress = tP2WPKHAddr
	node.changeAddr = tP2WPKHAddr
	privBytes, _ := hex.DecodeString("b07209eec1a8fb6cfe5cb6ace36567406971a75c330db7101fb21bc679bc5330")
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	node.privKeyForAddr, _ = btcutil.NewWIF(privKey, &chaincfg.MainNetParams, true)
	node.signFunc = func(tx *wire.MsgTx) { signFunc(tx, 0, true) }
	secretHash, _ := hex.DecodeString("5124208c80d33507befa517c08ed01aa8d33adbf37ecd70fb5f9352f7a51a88d")
	mkSwaps := func(hook func([]asset.Receipt) error) *asset.Swaps {
		return &asset.Swaps{
			Inputs:          asset.Coins{NewOutput(tTxHash, 0, toSatoshi(3)), NewOutput(tTxHash, 1, toSatoshi(3))},
			Contracts:       []*asset.Contract{{Address: tP2WPKHAddr, Value: toSatoshi(5), SecretHash: secretHash, LockTime: uint64(time.Now().Unix())}},
			FeeRate:         tBTC.MaxFeeRate,
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
