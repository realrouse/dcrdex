// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"decred.org/dcrdex/dex"
	"github.com/dgraph-io/badger/v4"
)

// newValidBadgerDB creates a badger DB in dir with some keys flushed to table
// files, closes it, and returns the paths of its *.sst files.
func newValidBadgerDB(t *testing.T, dir string, nKeys int) []string {
	t.Helper()
	opts := badger.DefaultOptions(dir).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	for i := 0; i < nKeys; i++ {
		if err := db.Update(func(txn *badger.Txn) error {
			return txn.Set([]byte(fmt.Sprintf("key%04d", i)), []byte(fmt.Sprintf("value%d", i)))
		}); err != nil {
			t.Fatalf("Update error: %v", err)
		}
	}
	if err := db.Close(); err != nil { // Close flushes the memtable to an L0 table.
		t.Fatalf("Close error: %v", err)
	}
	ssts, _ := filepath.Glob(filepath.Join(dir, "*.sst"))
	if len(ssts) == 0 {
		t.Fatalf("no table files were written")
	}
	return ssts
}

func TestCorruptTableValidDB(t *testing.T) {
	// Nonexistent and empty dirs are fine.
	if bad, _, err := CorruptTable(filepath.Join(t.TempDir(), "nope")); err != nil || bad != "" {
		t.Fatalf("nonexistent dir: bad = %q, err = %v", bad, err)
	}
	if bad, _, err := CorruptTable(t.TempDir()); err != nil || bad != "" {
		t.Fatalf("empty dir: bad = %q, err = %v", bad, err)
	}
	// Valid DBs with tiny and larger tables are not flagged.
	for _, n := range []int{1, 3, 2000} {
		dir := t.TempDir()
		newValidBadgerDB(t, dir, n)
		bad, problem, err := CorruptTable(dir)
		if err != nil || bad != "" {
			t.Fatalf("valid db with %d keys flagged: bad = %q (%s), err = %v", n, bad, problem, err)
		}
		movedTo, err := MoveAsideIfCorrupt(dir, dex.StdOutLogger("T", dex.LevelOff))
		if err != nil || movedTo != "" {
			t.Fatalf("valid db moved: %q, %v", movedTo, err)
		}
	}
}

func TestMoveAsideIfCorrupt(t *testing.T) {
	log := dex.StdOutLogger("T", dex.LevelInfo)
	corruptions := map[string]func(orig []byte) []byte{
		"zero-filled": func(orig []byte) []byte { return make([]byte, len(orig)) },
		// The exact file from the field report: 183 bytes, all zeros.
		"zero-filled-183": func([]byte) []byte { return make([]byte, 183) },
		"empty":           func([]byte) []byte { return nil },
		"tiny":            func([]byte) []byte { return []byte{1, 2, 3} },
		"truncated":       func(orig []byte) []byte { return orig[:len(orig)/2] },
		"zeroed-tail": func(orig []byte) []byte {
			b := append([]byte(nil), orig...)
			for i := len(b) - 20; i < len(b); i++ {
				b[i] = 0
			}
			return b
		},
		"flipped-index-byte": func(orig []byte) []byte {
			b := append([]byte(nil), orig...)
			b[len(b)-30] ^= 0xff
			return b
		},
	}
	for name, corrupt := range corruptions {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "txhistorydb-x_v4")
			ssts := newValidBadgerDB(t, dir, 50)
			orig, err := os.ReadFile(ssts[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ssts[0], corrupt(orig), 0600); err != nil {
				t.Fatal(err)
			}
			bad, problem, err := CorruptTable(dir)
			if err != nil || bad != ssts[0] {
				t.Fatalf("corrupt table not detected: bad = %q, err = %v", bad, err)
			}
			t.Logf("detected: %s", problem)

			movedTo, err := MoveAsideIfCorrupt(dir, log)
			if err != nil {
				t.Fatalf("MoveAsideIfCorrupt error: %v", err)
			}
			if !strings.HasPrefix(movedTo, dir+".corrupt-") {
				t.Fatalf("unexpected moved-to path %q", movedTo)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("original dir still exists: %v", err)
			}
			// Nothing was deleted.
			if b, err := os.ReadFile(filepath.Join(movedTo, filepath.Base(ssts[0]))); err != nil ||
				string(b) != string(corrupt(orig)) {
				t.Fatalf("moved table not preserved: %v", err)
			}
			// A fresh DB opens in its place.
			db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
			if err != nil {
				t.Fatalf("fresh open error: %v", err)
			}
			db.Close()
		})
	}
}

func TestCorruptTableUnreferenced(t *testing.T) {
	// A garbage table file that is not in the MANIFEST (e.g. left over from an
	// interrupted compaction) is deleted by badger on open, so it must not
	// cause the DB to be moved aside.
	dir := t.TempDir()
	newValidBadgerDB(t, dir, 10)
	orphan := filepath.Join(dir, "000099.sst")
	if err := os.WriteFile(orphan, make([]byte, 183), 0600); err != nil {
		t.Fatal(err)
	}
	if bad, problem, err := CorruptTable(dir); err != nil || bad != "" {
		t.Fatalf("unreferenced table flagged: bad = %q (%s), err = %v", bad, problem, err)
	}
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open with unreferenced table error: %v", err)
	}
	db.Close()

	// Without a readable MANIFEST, every table is checked.
	newValidBadgerDB(t, dir, 10)
	if err := os.WriteFile(orphan, make([]byte, 183), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, badger.ManifestFilename), filepath.Join(dir, "MANIFEST.bak")); err != nil {
		t.Fatal(err)
	}
	if bad, _, err := CorruptTable(dir); err != nil || bad != orphan {
		t.Fatalf("table not checked without MANIFEST: bad = %q, err = %v", bad, err)
	}
}
