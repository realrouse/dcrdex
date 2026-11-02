// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestRandomName(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]+-[a-z]+-\d{4}$`)
	host, _ := os.Hostname()
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		n := RandomName()
		if !re.MatchString(n) || len(n) > MaxPeerNameLen {
			t.Fatalf("bad name %q", n)
		}
		if host != "" && n == host {
			t.Fatalf("name is the host name")
		}
		seen[n] = true
	}
	if len(seen) < 190 {
		t.Fatalf("only %d distinct names of 200", len(seen))
	}
	for _, w := range append(append([]string{}, nameAdjectives...), nameAnimals...) {
		if len(w) > 10 {
			t.Fatalf("word too long: %s", w)
		}
	}
}

func TestLoadOrCreateName(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mesh", "name")
	a, err := LoadOrCreateName(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateName(p)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a == "" {
		t.Fatalf("name not kept: %q then %q", a, b)
	}
}
