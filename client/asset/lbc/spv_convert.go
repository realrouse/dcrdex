// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"bytes"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	lbcchainhash "github.com/lbryio/lbcd/chaincfg/chainhash"
	lbcwire "github.com/lbryio/lbcd/wire"
	lbcutil "github.com/lbryio/lbcutil"
)

func hashToBTC(h lbcchainhash.Hash) chainhash.Hash {
	return chainhash.Hash(h)
}

func hashToLBC(h chainhash.Hash) lbcchainhash.Hash {
	return lbcchainhash.Hash(h)
}

func hashPtrToBTC(h *lbcchainhash.Hash) *chainhash.Hash {
	if h == nil {
		return nil
	}
	bh := hashToBTC(*h)
	return &bh
}

func convertMsgTxToBTC(tx *lbcwire.MsgTx) (*wire.MsgTx, error) {
	buf := new(bytes.Buffer)
	if err := tx.Serialize(buf); err != nil {
		return nil, err
	}
	btcTx := new(wire.MsgTx)
	if err := btcTx.Deserialize(buf); err != nil {
		return nil, err
	}
	return btcTx, nil
}

func convertMsgTxToLBC(tx *wire.MsgTx) (*lbcwire.MsgTx, error) {
	buf := new(bytes.Buffer)
	if err := tx.Serialize(buf); err != nil {
		return nil, err
	}
	lbcTx := new(lbcwire.MsgTx)
	if err := lbcTx.Deserialize(buf); err != nil {
		return nil, err
	}
	return lbcTx, nil
}

func (w *lbcSPVWallet) addrLBC2BTC(addr lbcutil.Address) (btcutil.Address, error) {
	return btcutil.DecodeAddress(addr.String(), w.btcParams)
}

func (w *lbcSPVWallet) addrBTC2LBC(addr btcutil.Address) (lbcutil.Address, error) {
	return lbcutil.DecodeAddress(addr.String(), w.chainParams)
}

func headerToBTC(hdr *lbcwire.BlockHeader) *wire.BlockHeader {
	if hdr == nil {
		return nil
	}
	return &wire.BlockHeader{
		Version:    hdr.Version,
		PrevBlock:  hashToBTC(hdr.PrevBlock),
		MerkleRoot: hashToBTC(hdr.MerkleRoot),
		Timestamp:  hdr.Timestamp,
		Bits:       hdr.Bits,
		Nonce:      hdr.Nonce,
	}
}

// cloneParamsName maps a btcsuite clone-params name onto lbcd params.
func cloneParamsName(p *chaincfg.Params) string {
	if p == nil {
		return ""
	}
	return p.Name
}
