// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import "testing"

func TestAliasRPCPassword(t *testing.T) {
	s := map[string]string{"rpcuser": "u", "rpcpass": "secret", "rpclisten": "10.8.0.2:9244"}
	AliasRPCPassword(s)
	if s["rpcpassword"] != "secret" {
		t.Fatalf("rpcpass not copied: %#v", s)
	}
	if s["rpcbind"] != "10.8.0.2:9244" {
		t.Fatalf("rpclisten not copied: %#v", s)
	}
	s2 := map[string]string{"rpcpassword": "keep", "rpcpass": "other"}
	AliasRPCPassword(s2)
	if s2["rpcpassword"] != "keep" {
		t.Fatalf("existing rpcpassword overwritten: %#v", s2)
	}
	AliasRPCPassword(nil)
}
