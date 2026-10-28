// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"fmt"
	"testing"
)

func TestAccountFirstAddrRPCArgs(t *testing.T) {
	lbc := &rpcClient{rpcCore: &rpcCore{segwit: true, accountFirstAddrRPC: true, omitAddressType: true}}
	if got, want := fmt.Sprint(lbc.changeAddressArgs()), "[default bech32]"; got != want {
		t.Fatalf("lbc change args: got %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(lbc.newAddressArgs("bech32")), "[default bech32]"; got != want {
		t.Fatalf("lbc new args: got %s, want %s", got, want)
	}
	bitcoind := &rpcClient{rpcCore: &rpcCore{segwit: true}}
	if got, want := fmt.Sprint(bitcoind.changeAddressArgs()), "[bech32]"; got != want {
		t.Fatalf("bitcoind change args: got %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(bitcoind.newAddressArgs("bech32")), "[ bech32]"; got != want {
		t.Fatalf("bitcoind new args: got %s, want %s", got, want)
	}
	legacyOmit := &rpcClient{rpcCore: &rpcCore{omitAddressType: true}}
	if got := legacyOmit.changeAddressArgs(); got != nil {
		t.Fatalf("omit change args: got %v, want nil", got)
	}
}
