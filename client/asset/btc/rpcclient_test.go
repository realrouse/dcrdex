// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"decred.org/dcrdex/dex"
	"github.com/decred/dcrd/dcrjson/v4"
)

type countingRequester struct {
	calls int
	err   error
}

func (c *countingRequester) RawRequest(_ context.Context, _ string, _ []json.RawMessage) (json.RawMessage, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return json.RawMessage(`{}`), nil
}

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
	if got, want := fmt.Sprint(legacyOmit.changeAddressArgs()), "[default]"; got != want {
		t.Fatalf("omit change args: got %s, want %s", got, want)
	}
}

func TestListUnspentSpendableOmitted(t *testing.T) {
	var u ListUnspentResult
	if err := json.Unmarshal([]byte(`{"txid":"aa","vout":0,"amount":1.5}`), &u); err != nil {
		t.Fatal(err)
	}
	if !u.Spendable {
		t.Fatal("omitted spendable should default to true")
	}
	if err := json.Unmarshal([]byte(`{"txid":"aa","vout":0,"amount":1.5,"spendable":false}`), &u); err != nil {
		t.Fatal(err)
	}
	if u.Spendable {
		t.Fatal("explicit spendable=false must stay false")
	}
}

func TestIsMethodNotFoundErr(t *testing.T) {
	unimplemented := &dcrjson.RPCError{Code: -1, Message: "Method unimplemented"}
	wrapped := fmt.Errorf("rawrequest (getwalletinfo) error: %w", unimplemented)
	if !isMethodNotFoundErr(unimplemented) {
		t.Fatal("expected lbcwallet unimplemented error to match")
	}
	if !isMethodNotFoundErr(wrapped) {
		t.Fatal("expected wrapped unimplemented error to match")
	}
	other := &dcrjson.RPCError{Code: -1, Message: "something else"}
	if isMethodNotFoundErr(other) {
		t.Fatal("generic -1 should not match")
	}
}

func TestLockedSkipsMissingGetWalletInfo(t *testing.T) {
	unimplemented := &dcrjson.RPCError{Code: -1, Message: "Method unimplemented"}
	stub := &countingRequester{err: unimplemented}
	wc := newRPCClient(&rpcCore{
		optionalWalletInfo: true,
		cloneParams:        &BTCCloneCFG{OptionalWalletInfo: true},
		log:                dex.StdOutLogger("T", dex.LevelError),
	})
	wc.requesterV.Store(stub)
	if wc.Locked() {
		t.Fatal("optionalWalletInfo should treat the wallet as unlocked")
	}
	if stub.calls != 0 {
		t.Fatalf("getwalletinfo should not be called when optionalWalletInfo is set, got %d", stub.calls)
	}

	// Without the flag, probe once then cache.
	stub2 := &countingRequester{err: unimplemented}
	wc2 := newRPCClient(&rpcCore{
		log: dex.StdOutLogger("T", dex.LevelError),
	})
	wc2.requesterV.Store(stub2)
	if wc2.Locked() {
		t.Fatal("unimplemented getwalletinfo should treat the wallet as unlocked")
	}
	if stub2.calls != 1 {
		t.Fatalf("expected 1 getwalletinfo probe, got %d", stub2.calls)
	}
	_ = wc2.Locked()
	if stub2.calls != 1 {
		t.Fatalf("getwalletinfo should be cached after unimplemented, got %d calls", stub2.calls)
	}
}
