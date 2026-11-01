// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"net"
	"strconv"
	"testing"
)

// TestMainnetFilterPeers checks that the default mainnet SPV peer list is
// well formed, has no duplicates and still includes the foundation peer.
func TestMainnetFilterPeers(t *testing.T) {
	if len(mainnetFilterPeers) < 2 {
		t.Fatalf("expected several default mainnet peers, got %d", len(mainnetFilterPeers))
	}
	seen := make(map[string]bool, len(mainnetFilterPeers))
	var haveFoundation bool
	for _, addr := range mainnetFilterPeers {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("bad peer address %q: %v", addr, err)
		}
		if host == "" {
			t.Fatalf("peer %q has no host", addr)
		}
		if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
			t.Fatalf("peer %q has bad port %q", addr, port)
		}
		if seen[addr] {
			t.Fatalf("duplicate peer %q", addr)
		}
		seen[addr] = true
		if addr == foundationFilterPeer {
			haveFoundation = true
		}
	}
	if !haveFoundation {
		t.Fatalf("foundation peer %s missing from mainnet defaults", foundationFilterPeer)
	}
}
