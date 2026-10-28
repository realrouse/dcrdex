// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// signWitnessInputsLocally completes a tx that the wallet's legacy
// signrawtransaction left incomplete because it can't sign P2WPKH inputs
// (lbcwallet's SignTxOutput only knows pre-segwit script classes and fails
// them with "can't sign unknown transactions"). Each unsigned P2WPKH input is
// signed here with the key from dumpprivkey, the same way dcrdex already signs
// contract redeems and refunds. Every input is then verified with the script
// engine, so an incomplete or wrong tx is never returned.
func (wc *rpcClient) signWitnessInputsLocally(ctx context.Context, tx *wire.MsgTx) error {
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(tx.TxIn))
	for _, txIn := range tx.TxIn {
		op := txIn.PreviousOutPoint
		txOut, _, err := wc.GetTxOut(&op.Hash, op.Index, nil, time.Time{})
		if err != nil {
			return fmt.Errorf("prevout %v: %w", op, err)
		}
		if txOut == nil {
			return fmt.Errorf("prevout %v not found or spent", op)
		}
		prevOuts[op] = txOut
	}
	fetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, txIn := range tx.TxIn {
		prev := prevOuts[txIn.PreviousOutPoint]
		if len(txIn.Witness) > 0 || txscript.GetScriptClass(prev.PkScript) != txscript.WitnessV0PubKeyHashTy {
			continue
		}
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(prev.PkScript, wc.chainParams)
		if err != nil || len(addrs) != 1 {
			return fmt.Errorf("input %d: cannot decode P2WPKH script: %v", i, err)
		}
		addrStr, err := wc.stringAddr(addrs[0], wc.chainParams)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		priv, err := wc.PrivKeyForAddress(ctx, addrStr)
		if err != nil {
			return fmt.Errorf("input %d: key for %s: %w", i, addrStr, err)
		}
		wit, err := txscript.WitnessSignature(tx, sigHashes, i, prev.Value, prev.PkScript, txscript.SigHashAll, priv, true)
		priv.Zero()
		if err != nil {
			return fmt.Errorf("input %d: witness signature: %w", i, err)
		}
		txIn.Witness = wit
		txIn.SignatureScript = nil
	}
	for i, txIn := range tx.TxIn {
		prev := prevOuts[txIn.PreviousOutPoint]
		vm, err := txscript.NewEngine(prev.PkScript, tx, i, txscript.StandardVerifyFlags, nil, sigHashes, prev.Value, fetcher)
		if err == nil {
			err = vm.Execute()
		}
		if err != nil {
			return fmt.Errorf("input %d does not verify: %w", i, err)
		}
	}
	return nil
}
