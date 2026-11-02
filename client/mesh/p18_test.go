// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"testing"
	"time"

	_ "decred.org/dcrdex/client/asset/polygon" // registers usdc.polygon
)

func TestSameAddr(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"0xc021829367C6bA59C7d0d50e90Fe8C08f194910c", "0xc021829367c6ba59c7d0d50e90fe8c08f194910c", true},
		{"0xc021829367C6bA59C7d0d50e90Fe8C08f194910c", "0xc021829367C6bA59C7d0d50e90Fe8C08f194910d", false},
		{"Ssr9JYQ8XiM1df2P3sbNyW7gxmisGCHVGQe", "ssr9jyq8xim1df2p3sbnyw7gxmisgchvgqe", false}, // base58 is case sensitive
		{"bcrt1qabc", "bcrt1qabc", true},
	} {
		if got := sameAddr(tt.a, tt.b); got != tt.want {
			t.Errorf("sameAddr(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestUSDCPolygonMarkets(t *testing.T) {
	for _, name := range []string{"dcr_usdc.polygon", "lbc_usdc.polygon"} {
		m, err := ParseMarket(name)
		if err != nil {
			t.Fatalf("ParseMarket(%s): %v", name, err)
		}
		if n, _ := m.Name(); n != name {
			t.Fatalf("round trip %s -> %s", name, n)
		}
		if _, ok := MarketDefault(name); !ok {
			t.Fatalf("no defaults for %s", name)
		}
		if err := DefaultLockPolicies[name].Validate(); err != nil {
			t.Fatalf("%s lock policy: %v", name, err)
		}
	}
	// Tokens without their own entry fall back to the 10 minute floor.
	if d := MakerContractTimeout(966001); d != 10*time.Minute {
		t.Fatalf("usdc.polygon contract timeout %s", d)
	}
}
