// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddressFromTxHistoryDBName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"txhistorydb-lbc1ql494304ygkws7rgen3pl9g8jjxqv3duylpj42t_v4",
		"txhistorydb-bc1qaddr",
		"txhistorydb-",
		"other",
	} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "txhistorydb-file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"txhistorydb-lbc1ql494304ygkws7rgen3pl9g8jjxqv3duylpj42t_v4": "lbc1ql494304ygkws7rgen3pl9g8jjxqv3duylpj42t",
		"txhistorydb-bc1qaddr": "bc1qaddr",
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		addr, ok := addressFromTxHistoryDBName(fi)
		if w, found := want[e.Name()]; found {
			if !ok || addr != w {
				t.Fatalf("%s: got %q, %v, want %q", e.Name(), addr, ok, w)
			}
		} else if ok {
			t.Fatalf("%s: unexpected match %q", e.Name(), addr)
		}
	}
}
