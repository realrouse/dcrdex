//go:build !spvlive && !harness

package btc

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	dexbtc "decred.org/dcrdex/dex/networks/btc"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type tBond struct {
	priv     *btcec.PrivateKey
	script   []byte
	pkScript []byte
	tx       *wire.MsgTx
	coinID   []byte
	txHash   *chainhash.Hash
	amt      uint64
	lockTime uint32
}

// newTestBond creates a bond tx with a P2WSH (segwitBond) or P2SH bond output
// whose lock time has passed.
func newTestBond(t *testing.T, segwitBond bool) *tBond {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pkh := btcutil.Hash160(priv.PubKey().SerializeCompressed())
	lockTime := uint32(time.Now().Add(-time.Hour).Unix())
	script, err := dexbtc.MakeBondScript(0, lockTime, pkh)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := scriptHashAddress(segwitBond, script, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	pkScript, _ := txscript.PayToAddrScript(addr)
	const amt = 5000_0000_0000
	commit, err := bondPushDataScript(0, randBytes(32), int64(lockTime), pkh)
	if err != nil {
		t.Fatal(err)
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{1}, 0), []byte{0x01}, nil))
	tx.AddTxOut(wire.NewTxOut(amt, pkScript))
	tx.AddTxOut(wire.NewTxOut(0, commit))
	txHash := tx.TxHash()
	return &tBond{
		priv:     priv,
		script:   script,
		pkScript: pkScript,
		tx:       tx,
		coinID:   ToCoinID(&txHash, 0),
		txHash:   &txHash,
		amt:      amt,
		lockTime: lockTime,
	}
}

func addWalletTx(t *testing.T, node *testData, tx *wire.MsgTx, confs uint64) {
	t.Helper()
	b, err := serializeMsgTx(tx)
	if err != nil {
		t.Fatal(err)
	}
	node.blockchainMtx.Lock()
	node.getTransactionMap[tx.TxHash().String()] = &GetTransactionResult{
		TxID:          tx.TxHash().String(),
		Bytes:         b,
		Confirmations: confs,
	}
	node.blockchainMtx.Unlock()
}

// verifySpend independently checks the refund's bond input with the script
// engine.
func verifySpend(t *testing.T, tx *wire.MsgTx, prevPkScript []byte, amt uint64) error {
	t.Helper()
	prevOuts := txscript.NewCannedPrevOutputFetcher(prevPkScript, int64(amt))
	vm, err := txscript.NewEngine(prevPkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		txscript.NewTxSigHashes(tx, prevOuts), int64(amt), prevOuts)
	if err != nil {
		return err
	}
	return vm.Execute()
}

type refundBondTest struct {
	name          string
	walletSegwit  bool
	bondSegwit    bool
	inWallet      bool
	wantErr       bool
	wantWitness   bool
	corruptScript bool
	txOutScript   bool
}

func TestRefundBondOutputTypes(t *testing.T) {
	tests := []*refundBondTest{{
		// The LBC case: a P2SH bond created before SegWit was enabled,
		// refunded by a segwit wallet.
		name:         "legacy P2SH bond, segwit wallet",
		walletSegwit: true,
		bondSegwit:   false,
		inWallet:     true,
	}, {
		name:         "P2WSH bond, segwit wallet",
		walletSegwit: true,
		bondSegwit:   true,
		inWallet:     true,
		wantWitness:  true,
	}, {
		name:         "P2SH bond, legacy wallet",
		walletSegwit: false,
		bondSegwit:   false,
		inWallet:     true,
	}, {
		name:         "P2WSH bond not in wallet, segwit wallet falls back to P2WSH",
		walletSegwit: true,
		bondSegwit:   true,
		wantWitness:  true,
	}, {
		// The bond tx is not in the wallet, but the node reports the
		// unspent P2SH output.
		name:         "P2SH bond not in wallet, type from gettxout",
		walletSegwit: true,
		bondSegwit:   false,
		txOutScript:  true,
	}, {
		name:          "bond output does not pay to the bond script",
		walletSegwit:  true,
		bondSegwit:    true,
		inWallet:      true,
		corruptScript: true,
		wantErr:       true,
	}}

	for _, tt := range tests {
		for _, walletType := range []string{walletTypeRPC, walletTypeSPV} {
			if walletType == walletTypeSPV && !tt.inWallet {
				continue // SPV GetTxOut mocking is not set up for this
			}
			t.Run(walletType+"/"+tt.name, func(t *testing.T) {
				testRefundBondOutputType(t, walletType, tt)
			})
		}
	}
}

func testRefundBondOutputType(t *testing.T, walletType string, tt *refundBondTest) {
	wallet, node, shutdown := tNewWallet(tt.walletSegwit, walletType)
	defer shutdown()
	if tt.walletSegwit {
		node.newAddress = tP2WPKHAddr
	} else {
		node.newAddress = tP2PKHAddr
	}
	bond := newTestBond(t, tt.bondSegwit)
	if tt.corruptScript {
		bond.tx.TxOut[0].PkScript = append([]byte{}, bond.tx.TxOut[0].PkScript...)
		bond.tx.TxOut[0].PkScript[5] ^= 0xff
		h := bond.tx.TxHash()
		bond.txHash, bond.coinID = &h, ToCoinID(&h, 0)
	}
	if tt.inWallet {
		addWalletTx(t, node, bond.tx, 100)
	}
	if tt.txOutScript {
		node.txOutRes = newTxOutResult(bond.pkScript, bond.amt, 100)
	}
	node.sentRawTx = nil

	coin, err := wallet.RefundBond(tCtx, 0, bond.coinID, bond.script, bond.amt, bond.priv)
	if tt.wantErr {
		if err == nil {
			t.Fatal("expected error")
		}
		if node.sentRawTx != nil {
			t.Fatal("refund was broadcast despite error")
		}
		return
	}
	if err != nil {
		t.Fatalf("RefundBond error: %v", err)
	}
	sent := node.sentRawTx
	if sent == nil {
		t.Fatal("no refund broadcast")
	}
	txIn := sent.TxIn[0]
	if txIn.PreviousOutPoint.Hash != *bond.txHash || txIn.PreviousOutPoint.Index != 0 {
		t.Fatal("refund does not spend the bond")
	}
	if hasWitness := len(txIn.Witness) > 0; hasWitness != tt.wantWitness {
		t.Fatalf("witness = %v, want %v", hasWitness, tt.wantWitness)
	}
	if hasSigScript := len(txIn.SignatureScript) > 0; hasSigScript == tt.wantWitness {
		t.Fatalf("unexpected signature script presence %v", hasSigScript)
	}
	if sent.LockTime != bond.lockTime {
		t.Fatalf("wrong lock time %d", sent.LockTime)
	}
	if err := verifySpend(t, sent, bond.pkScript, bond.amt); err != nil {
		t.Fatalf("refund does not verify: %v", err)
	}
	// The returned coin is the refund output, not the bond.
	refundHash := sent.TxHash()
	if !bytes.Equal(coin.ID(), ToCoinID(&refundHash, 0)) {
		t.Fatalf("returned coin %s is not the refund output %s:0", coin, refundHash)
	}
	if coin.Value() >= bond.amt || coin.Value() < bond.amt-100_000 {
		t.Fatalf("unexpected refund value %d", coin.Value())
	}
}

// TestOldSegwitRefundOfP2SHBondRejected reproduces the original bug: a segwit
// style refund of a P2SH bond is invalid, and verifyBondRefund catches it.
func TestOldSegwitRefundOfP2SHBondRejected(t *testing.T) {
	wallet, node, shutdown := tNewWallet(true, walletTypeRPC)
	defer shutdown()
	node.newAddress = tP2WPKHAddr
	bond := newTestBond(t, false)
	badTx, err := wallet.makeBondRefundTxV0(bond.txHash, 0, bond.amt, bond.script, bond.priv, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySpend(t, badTx, bond.pkScript, bond.amt); err == nil {
		t.Fatal("segwit refund of a P2SH bond unexpectedly verified")
	}
	if err := wallet.verifyBondRefund(badTx, bond.txHash, 0, bond.pkScript, bond.amt, false); err == nil {
		t.Fatal("verifyBondRefund did not catch the invalid refund")
	}
}

func TestRefundBondPriorRefund(t *testing.T) {
	wallet, node, shutdown := tNewWallet(true, walletTypeRPC)
	defer shutdown()
	node.newAddress = tP2WPKHAddr
	bond := newTestBond(t, false) // legacy P2SH bond
	addWalletTx(t, node, bond.tx, 100)
	pkh := btcutil.Hash160(bond.priv.PubKey().SerializeCompressed())

	addPending := func(tx *wire.MsgTx) {
		h := tx.TxHash()
		addWalletTx(t, node, tx, 0)
		wallet.pendingTxsMtx.Lock()
		wallet.pendingTxs[h] = ExtendedWalletTx{
			WalletTransaction: &asset.WalletTransaction{
				Type: asset.RedeemBond,
				ID:   h.String(),
				BondInfo: &asset.BondTxInfo{
					LockTime: uint64(bond.lockTime),
					BondID:   pkh,
				},
			},
			Submitted: true,
		}
		wallet.pendingTxsMtx.Unlock()
	}

	// An invalid prior refund (the old segwit-style spend, like the stuck
	// LBC tx) must be ignored, and a new valid refund created.
	badTx, err := wallet.makeBondRefundTxV0(bond.txHash, 0, bond.amt, bond.script, bond.priv, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	addPending(badTx)
	coin, err := wallet.RefundBond(tCtx, 0, bond.coinID, bond.script, bond.amt, bond.priv)
	if err != nil {
		t.Fatal(err)
	}
	newTx := node.sentRawTx
	if newTx.TxHash() == badTx.TxHash() {
		t.Fatal("invalid prior refund was rebroadcast")
	}
	if err := verifySpend(t, newTx, bond.pkScript, bond.amt); err != nil {
		t.Fatalf("new refund does not verify: %v", err)
	}

	// A valid prior refund is rebroadcast as is, even if the network
	// already has it.
	addPending(newTx)
	node.sentRawTx = nil
	node.sendErr = errors.New("txn-already-in-mempool")
	coin2, err := wallet.RefundBond(tCtx, 0, bond.coinID, bond.script, bond.amt, bond.priv)
	if err != nil {
		t.Fatalf("rebroadcast error: %v", err)
	}
	if node.sentRawTx == nil || node.sentRawTx.TxHash() != newTx.TxHash() {
		t.Fatal("valid prior refund not rebroadcast")
	}
	if !bytes.Equal(coin.ID(), coin2.ID()) {
		t.Fatal("rebroadcast returned a different coin")
	}

	// A real send error for a new refund is an error.
	wallet.pendingTxsMtx.Lock()
	wallet.pendingTxs = make(map[chainhash.Hash]ExtendedWalletTx)
	wallet.pendingTxsMtx.Unlock()
	node.sendErr = errors.New("bad-txns-inputs-missingorspent")
	if _, err := wallet.RefundBond(tCtx, 0, bond.coinID, bond.script, bond.amt, bond.priv); err == nil {
		t.Fatal("expected send error")
	}
}

func TestBondRefundChecker(t *testing.T) {
	wallet, node, shutdown := tNewWallet(true, walletTypeRPC)
	defer shutdown()
	var _ asset.BondRefundChecker = wallet

	bond := newTestBond(t, false)
	addWalletTx(t, node, bond.tx, 100)

	// Unspent.
	node.txOutRes = &btcjson.GetTxOutResult{Value: float64(bond.amt) / 1e8, Confirmations: 100}
	spent, err := wallet.BondSpent(tCtx, 0, bond.coinID, bond.script)
	if err != nil {
		t.Fatal(err)
	}
	if spent {
		t.Fatal("unspent bond reported spent")
	}
	// Spent.
	node.txOutRes = nil
	if spent, err = wallet.BondSpent(tCtx, 0, bond.coinID, bond.script); err != nil {
		t.Fatal(err)
	} else if !spent {
		t.Fatal("spent bond reported unspent")
	}
	// Error.
	node.txOutErr = errors.New("boom")
	if _, err = wallet.BondSpent(tCtx, 0, bond.coinID, bond.script); err == nil {
		t.Fatal("expected error")
	}
	node.txOutErr = nil

	// Refund confirmations.
	refundTx := wire.NewMsgTx(wire.TxVersion)
	refundTx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(bond.txHash, 0), nil, nil))
	refundTx.AddTxOut(wire.NewTxOut(1, tP2WPKH))
	refundHash := refundTx.TxHash()
	refundCoinID := ToCoinID(&refundHash, 0)
	if _, err := wallet.BondRefundConfirmations(tCtx, refundCoinID); err == nil {
		t.Fatal("expected error for unknown refund")
	}
	addWalletTx(t, node, refundTx, 2)
	if confs, err := wallet.BondRefundConfirmations(tCtx, refundCoinID); err != nil || confs != 2 {
		t.Fatalf("wrong confs %d, err = %v", confs, err)
	}
}

// TestFindLegacyBondSegwitWallet checks that a segwit wallet can decode a P2SH
// bond created before the wallet was switched to segwit.
func TestFindLegacyBondSegwitWallet(t *testing.T) {
	for _, bondSegwit := range []bool{false, true} {
		wallet, node, shutdown := tNewWallet(true, walletTypeRPC)
		bond := newTestBond(t, bondSegwit)
		addWalletTx(t, node, bond.tx, 100)
		bd, err := wallet.FindBond(tCtx, bond.coinID, time.Time{})
		shutdown()
		if err != nil {
			t.Fatalf("segwit bond %v: FindBond error: %v", bondSegwit, err)
		}
		if !bytes.Equal(bd.Data, bond.script) || bd.Amount != bond.amt {
			t.Fatalf("segwit bond %v: wrong bond details", bondSegwit)
		}
	}
}
