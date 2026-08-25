// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	lbcchainhash "github.com/lbryio/lbcd/chaincfg/chainhash"
	dexlbc "decred.org/dcrdex/dex/networks/lbc"
)

func TestHashRoundTrip(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	var lbcHash lbcchainhash.Hash
	copy(lbcHash[:], raw)
	btcHash := hashToBTC(lbcHash)
	if hashToLBC(btcHash) != lbcHash {
		t.Fatalf("hash round trip failed")
	}
	if btcHash != chainhash.Hash(lbcHash) {
		t.Fatalf("btcsuite hash mismatch")
	}
}

func TestDeserializeBlockRejectsShortHeader(t *testing.T) {
	_, err := dexlbc.DeserializeBlock(make([]byte, 80))
	if err == nil {
		t.Fatalf("expected error deserializing 80-byte Bitcoin header as LBC")
	}
}
