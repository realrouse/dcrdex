// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dgraph-io/badger/v4"
)

func TestDefaultOptions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db_v4")
	key, val := []byte("k"), bytes.Repeat([]byte{1}, 300)
	big := bytes.Repeat([]byte{2}, 3<<20) // over ValueThreshold: goes in the value log

	// A DB made with badger's defaults (2 GB value log, 64 MB memtable)
	// has data in its memtable WAL that was never flushed.
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(txn *badger.Txn) error { return txn.Set(key, val) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// It opens with the small options, keeps its data, and stores values
	// that go in the value log.
	db, err = badger.Open(DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("reopen with DefaultOptions: %v", err)
	}
	if err := db.Update(func(txn *badger.Txn) error { return txn.Set([]byte("big"), big) }); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		err := db.View(func(txn *badger.Txn) error {
			for k, want := range map[string][]byte{string(key): val, "big": big} {
				item, err := txn.Get([]byte(k))
				if err != nil {
					return err
				}
				got, err := item.ValueCopy(nil)
				if err != nil {
					return err
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("wrong value for %q", k)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check()

	// No newly created file is anywhere near badger's default sizes.
	const maxFile = 32 << 20
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawNew bool
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".vlog") && !strings.HasSuffix(e.Name(), ".mem") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() <= maxFile {
			sawNew = true
		} else if strings.HasSuffix(e.Name(), ".mem") {
			t.Fatalf("memtable file %s is %d bytes", e.Name(), fi.Size())
		}
	}
	if !sawNew {
		t.Fatalf("no small value log or memtable file was created")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = badger.Open(DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	check()
}
