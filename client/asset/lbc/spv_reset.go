// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"os"
	"path/filepath"
	"strings"

	"decred.org/dcrdex/dex"
)

// resetStaleNeutrinoChain deletes compact-filter / header files left by
// earlier Native LBC builds that could not sync past genesis. wallet.db is
// not touched: keys, accounts, and DEX registration live there.
//
// A one-time stamp (neutrino.chainformat) prevents repeating the reset after
// a healthy sync has started.
func resetStaleNeutrinoChain(dir string, log dex.Logger) error {
	if dir == "" {
		return nil
	}
	formatPath := filepath.Join(dir, neutrinoChainFormatFile)
	if current, err := os.ReadFile(formatPath); err == nil &&
		strings.TrimSpace(string(current)) == neutrinoChainFormat {
		return nil
	}

	if stuck, nHeaders := neutrinoChainLooksStuck(dir); stuck {
		if log != nil {
			log.Warnf("Resetting native LBC compact-filter data at %s "+
				"(on-disk headers=%d). Wallet seed and keys are kept.", dir, nHeaders)
		}
		if err := removeNeutrinoChainFiles(dir); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(formatPath, []byte(neutrinoChainFormat+"\n"), 0o600)
}

func neutrinoChainLooksStuck(dir string) (bool, int64) {
	path := filepath.Join(dir, "block_headers.bin")
	info, err := os.Stat(path)
	if err != nil {
		return false, 0
	}
	if info.Size() <= 0 {
		return true, 0
	}
	n := info.Size() / int64(lbcBlockHeaderSize)
	return n <= neutrinoStuckHeaderCount, n
}

func removeNeutrinoChainFiles(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "neutrino.db*"))
	if err != nil {
		return err
	}
	for _, name := range append(matches,
		filepath.Join(dir, "block_headers.bin"),
		filepath.Join(dir, "reg_filter_headers.bin"),
	) {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
