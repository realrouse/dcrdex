// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"decred.org/dcrdex/dex"
	dexlbc "decred.org/dcrdex/dex/networks/lbc"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	lbcchaincfg "github.com/lbryio/lbcd/chaincfg"
	lbcchainhash "github.com/lbryio/lbcd/chaincfg/chainhash"
	lbchdkeychain "github.com/lbryio/lbcutil/hdkeychain"
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

func TestResetStaleNeutrinoChain(t *testing.T) {
	log := dex.StdOutLogger("T", dex.LevelOff)

	write := func(t *testing.T, dir, name string, nBytes int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, nBytes), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(dir, name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}

	t.Run("fresh dir is stamped and left alone", func(t *testing.T) {
		dir := t.TempDir()
		if err := resetStaleNeutrinoChain(dir, log); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir, neutrinoChainFormatFile))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != neutrinoChainFormat {
			t.Fatalf("format=%q", got)
		}
	})

	t.Run("genesis-only headers are deleted, wallet.db is kept", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "wallet.db", 32)
		write(t, dir, "neutrino.db", 32)
		write(t, dir, "block_headers.bin", lbcBlockHeaderSize) // height 0
		write(t, dir, "reg_filter_headers.bin", 32)
		if err := resetStaleNeutrinoChain(dir, log); err != nil {
			t.Fatal(err)
		}
		if !exists(dir, "wallet.db") {
			t.Fatal("wallet.db was removed")
		}
		for _, name := range []string{"neutrino.db", "block_headers.bin", "reg_filter_headers.bin"} {
			if exists(dir, name) {
				t.Fatalf("%s should have been removed", name)
			}
		}
	})

	t.Run("progressed headers are kept", func(t *testing.T) {
		dir := t.TempDir()
		n := (neutrinoStuckHeaderCount + 1) * lbcBlockHeaderSize
		write(t, dir, "neutrino.db", 8)
		write(t, dir, "block_headers.bin", n)
		if err := resetStaleNeutrinoChain(dir, log); err != nil {
			t.Fatal(err)
		}
		if !exists(dir, "neutrino.db") || !exists(dir, "block_headers.bin") {
			t.Fatal("healthy chain files were removed")
		}
	})

	t.Run("already stamped genesis files are not reset again", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, neutrinoChainFormatFile, 0)
		if err := os.WriteFile(filepath.Join(dir, neutrinoChainFormatFile), []byte(neutrinoChainFormat+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		write(t, dir, "block_headers.bin", lbcBlockHeaderSize)
		write(t, dir, "neutrino.db", 8)
		if err := resetStaleNeutrinoChain(dir, log); err != nil {
			t.Fatal(err)
		}
		if !exists(dir, "neutrino.db") || !exists(dir, "block_headers.bin") {
			t.Fatal("stamped chain files were removed")
		}
	})
}

func TestConvertAccountPubKey(t *testing.T) {
	if convertAccountPubKey(nil) != nil {
		t.Fatalf("nil key not converted to nil")
	}
	seed := bytes.Repeat([]byte{7}, 32)
	master, err := lbchdkeychain.NewMaster(seed, &lbcchaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	acct := master
	for _, i := range []uint32{84 + lbchdkeychain.HardenedKeyStart, 140 + lbchdkeychain.HardenedKeyStart, lbchdkeychain.HardenedKeyStart} {
		if acct, err = acct.Derive(i); err != nil {
			t.Fatal(err)
		}
	}
	pub, err := acct.Neuter()
	if err != nil {
		t.Fatal(err)
	}
	conv := convertAccountPubKey(pub)
	if conv == nil {
		t.Fatalf("key not converted")
	}
	// Same serialization, so the same key and metadata.
	if conv.String() != pub.String() {
		t.Fatalf("converted key %s != %s", conv, pub)
	}
	lbcPub, _ := pub.ECPubKey()
	btcPub, err := conv.ECPubKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lbcPub.SerializeCompressed(), btcPub.SerializeCompressed()) {
		t.Fatalf("public keys differ")
	}
}
