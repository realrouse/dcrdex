// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"decred.org/dcrdex/dex"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// tLegacySignWallet mimics lbcwallet: signrawtransaction signs P2PKH inputs
// but fails P2WPKH inputs with "can't sign unknown transactions" (complete =
// false), gettxout finds mempool outputs, and dumpprivkey returns the key.
type tLegacySignWallet struct {
	t        *testing.T
	params   *chaincfg.Params
	prevOuts map[wire.OutPoint]*wire.TxOut
	keys     map[string]*btcec.PrivateKey // address -> key
}

func (w *tLegacySignWallet) RawRequest(_ context.Context, method string, params []json.RawMessage) (json.RawMessage, error) {
	switch method {
	case methodSignTxLegacy:
		var txHex string
		if err := json.Unmarshal(params[0], &txHex); err != nil {
			return nil, err
		}
		b, _ := hex.DecodeString(txHex)
		tx := wire.NewMsgTx(wire.TxVersion)
		if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
			return nil, err
		}
		res := SignTxResult{Complete: true}
		for i, txIn := range tx.TxIn {
			prev := w.prevOuts[txIn.PreviousOutPoint]
			if prev == nil {
				return nil, fmt.Errorf("Input %s not found", txIn.PreviousOutPoint)
			}
			if txscript.GetScriptClass(prev.PkScript) != txscript.PubKeyHashTy {
				res.Complete = false
				res.Errors = append(res.Errors, &SignTxError{
					TxID:  txIn.PreviousOutPoint.Hash.String(),
					Vout:  txIn.PreviousOutPoint.Index,
					Error: "can't sign unknown transactions",
				})
				continue
			}
			_, addrs, _, _ := txscript.ExtractPkScriptAddrs(prev.PkScript, w.params)
			priv := w.keys[addrs[0].String()]
			sig, err := txscript.SignatureScript(tx, i, prev.PkScript, txscript.SigHashAll, priv, true)
			if err != nil {
				return nil, err
			}
			txIn.SignatureScript = sig
		}
		var buf bytes.Buffer
		tx.Serialize(&buf)
		res.Hex = buf.Bytes()
		return json.Marshal(res)
	case methodGetTxOut:
		var txid string
		var vout uint32
		json.Unmarshal(params[0], &txid)
		json.Unmarshal(params[1], &vout)
		h, _ := chainhash.NewHashFromStr(txid)
		prev := w.prevOuts[wire.OutPoint{Hash: *h, Index: vout}]
		if prev == nil {
			return json.RawMessage("null"), nil
		}
		return json.Marshal(&btcjson.GetTxOutResult{
			Confirmations: 0, // unconfirmed, e.g. a just-sent split tx
			Value:         float64(prev.Value) / 1e8,
			ScriptPubKey:  btcjson.ScriptPubKeyResult{Hex: hex.EncodeToString(prev.PkScript)},
		})
	case methodPrivKeyForAddress:
		var addr string
		json.Unmarshal(params[0], &addr)
		priv := w.keys[addr]
		if priv == nil {
			return nil, fmt.Errorf("address not found in wallet")
		}
		wif, _ := btcutil.NewWIF(priv, w.params, true)
		return json.Marshal(wif.String())
	}
	w.t.Fatalf("unexpected RPC %s", method)
	return nil, nil
}

func (w *tLegacySignWallet) addKeyOutput(t *testing.T, witness bool, value int64) wire.OutPoint {
	t.Helper()
	priv, _ := btcec.NewPrivateKey()
	pkh := btcutil.Hash160(priv.PubKey().SerializeCompressed())
	var addr btcutil.Address
	var err error
	if witness {
		addr, err = btcutil.NewAddressWitnessPubKeyHash(pkh, w.params)
	} else {
		addr, err = btcutil.NewAddressPubKeyHash(pkh, w.params)
	}
	if err != nil {
		t.Fatal(err)
	}
	script, _ := txscript.PayToAddrScript(addr)
	op := wire.OutPoint{Hash: chainhash.Hash(randBytes(32)), Index: uint32(len(w.prevOuts))}
	w.prevOuts[op] = wire.NewTxOut(value, script)
	w.keys[addr.String()] = priv
	return op
}

func tLBCLikeRPCClient(w *tLegacySignWallet, localWitnessSign bool) *rpcClient {
	core := &rpcCore{
		segwit:           true,
		chainParams:      w.params,
		legacySignTx:     true,
		localWitnessSign: localWitnessSign,
		log:              dex.StdOutLogger("T", dex.LevelOff),
		decodeAddr:       btcutil.DecodeAddress,
		stringAddr: func(a btcutil.Address, _ *chaincfg.Params) (string, error) {
			return a.String(), nil
		},
		deserializeTx: func(b []byte) (*wire.MsgTx, error) {
			tx := wire.NewMsgTx(wire.TxVersion)
			return tx, tx.Deserialize(bytes.NewReader(b))
		},
		serializeTx: func(tx *wire.MsgTx) ([]byte, error) {
			var buf bytes.Buffer
			err := tx.Serialize(&buf)
			return buf.Bytes(), err
		},
	}
	core.requesterV.Store(RawRequester(w))
	return newRPCClient(core)
}

// TestLocalWitnessSignSplitFunding is a regression test for the lbcwallet
// RPC wallet with SegWit on: a swap funded by a split tx output (P2WPKH, still
// unconfirmed) plus an old P2PKH coin. lbcwallet's signrawtransaction leaves
// the P2WPKH input unsigned; with localWitnessSign the tx must come back fully
// signed and valid, and without it SignTx must fail rather than return an
// incomplete tx.
func TestLocalWitnessSignSplitFunding(t *testing.T) {
	w := &tLegacySignWallet{
		t:        t,
		params:   &chaincfg.MainNetParams,
		prevOuts: make(map[wire.OutPoint]*wire.TxOut),
		keys:     make(map[string]*btcec.PrivateKey),
	}
	splitOut := w.addKeyOutput(t, true, 5e8)
	legacyOut := w.addKeyOutput(t, false, 1e8)
	changeOut := w.addKeyOutput(t, true, 2e8) // bech32 change from an earlier tx

	newTx := func() *wire.MsgTx {
		tx := wire.NewMsgTx(wire.TxVersion)
		for _, op := range []wire.OutPoint{splitOut, legacyOut, changeOut} {
			op := op
			tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
		}
		contractScript := make([]byte, 34)
		contractScript[0], contractScript[1] = txscript.OP_0, 32
		tx.AddTxOut(wire.NewTxOut(7e8, contractScript))
		return tx
	}

	verify := func(tx *wire.MsgTx) error {
		fetcher := txscript.NewMultiPrevOutFetcher(w.prevOuts)
		sigHashes := txscript.NewTxSigHashes(tx, fetcher)
		for i, txIn := range tx.TxIn {
			prev := w.prevOuts[txIn.PreviousOutPoint]
			vm, err := txscript.NewEngine(prev.PkScript, tx, i, txscript.StandardVerifyFlags, nil, sigHashes, prev.Value, fetcher)
			if err == nil {
				err = vm.Execute()
			}
			if err != nil {
				return fmt.Errorf("input %d: %w", i, err)
			}
		}
		return nil
	}

	// Without local signing (old behaviour): signing is incomplete.
	cl := tLBCLikeRPCClient(w, false)
	if _, err := cl.SignTx(context.Background(), newTx()); err == nil ||
		!strings.Contains(err.Error(), "can't sign unknown transactions") {
		t.Fatalf("expected incomplete signing error, got %v", err)
	}

	// With local signing: every input is signed and verifies.
	cl = tLBCLikeRPCClient(w, true)
	signed, err := cl.SignTx(context.Background(), newTx())
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}
	if err := verify(signed); err != nil {
		t.Fatalf("signed tx does not verify: %v", err)
	}
	if len(signed.TxIn[1].Witness) != 0 || len(signed.TxIn[1].SignatureScript) == 0 {
		t.Fatalf("P2PKH input should keep the wallet's scriptSig")
	}
	for _, i := range []int{0, 2} {
		if len(signed.TxIn[i].Witness) != 2 || len(signed.TxIn[i].SignatureScript) != 0 {
			t.Fatalf("P2WPKH input %d not witness-signed", i)
		}
	}

	// A P2WPKH coin whose key the wallet doesn't have must fail, never
	// return an incomplete tx.
	foreign := w.addKeyOutput(t, true, 1e8)
	_, foreignAddrs, _, _ := txscript.ExtractPkScriptAddrs(w.prevOuts[foreign].PkScript, w.params)
	delete(w.keys, foreignAddrs[0].String())
	tx := newTx()
	tx.AddTxIn(wire.NewTxIn(&foreign, nil, nil))
	if _, err := cl.SignTx(context.Background(), tx); err == nil {
		t.Fatal("expected error signing an input without a wallet key")
	}
}
