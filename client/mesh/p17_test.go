// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"strings"
	"testing"
	"time"
)

// P17: an unfunded or silent maker is detected within MakerInitTimeout, not
// at the taker's lock time.

var p17SwapCfg = SwapConfig{SwapConfs: 1, SafetyMargin: time.Second, ResendInterval: 30 * time.Millisecond,
	MissingContractTimeout: 200 * time.Millisecond, MakerInitTimeout: 900 * time.Millisecond}

func enableP17Swaps(t *testing.T, ob *OrderBooks, chain *fakeChain) {
	t.Helper()
	owner := walletOwner(ob)
	ob.EnableSwaps(p17SwapCfg, func(assetID uint32) (SwapWallet, error) {
		return &fakeSwapWallet{c: chain, asset: assetID, owner: owner}, nil
	}, nil)
	ctx, cancel := context.WithCancel(tCtx)
	go ob.RunSwaps(ctx, 20*time.Millisecond)
	t.Cleanup(cancel)
}

func p17LastEvent(r *SwapRecord) string {
	if r == nil || len(r.Events) == 0 {
		return ""
	}
	return r.Events[len(r.Events)-1].Msg
}

// The maker can't fund its contract: it gives up shortly before the taker's
// deadline and tells the taker, which abandons at once.
func TestSwapMakerUnfunded(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	chain.mtx.Lock()
	chain.dead[walletOwner(maker)] = true // every Initiate fails
	chain.mtx.Unlock()
	enableP17Swaps(t, maker, chain)
	enableP17Swaps(t, taker, chain)
	start := time.Now()
	id := agree(t, maker, taker)
	mr := waitSwap(t, maker, id, SwapAbandoned, 5*time.Second)
	tr := waitSwap(t, taker, id, SwapAbandoned, 5*time.Second)
	took := time.Since(start)
	if !strings.Contains(p17LastEvent(mr), "maker could not lock its coins") {
		t.Fatalf("maker reason: %q", p17LastEvent(mr))
	}
	if !strings.Contains(p17LastEvent(tr), "the maker gave up before locking coins") {
		t.Fatalf("taker reason: %q (want the maker's abort)", p17LastEvent(tr))
	}
	if took > 3*time.Second {
		t.Fatalf("took %s", took)
	}
	if tr.TakerSwap != nil || chain.balance(0, walletOwner(taker)) != 0 {
		t.Fatalf("taker locked coins: %+v", tr)
	}
}

// The maker never answers (old version, offline, stuck): the taker gives up
// after MakerInitTimeout on its own and tells the maker.
func TestSwapMakerSilent(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	enableP17Swaps(t, maker, chain)
	enableP17Swaps(t, taker, chain)
	_ = maker.SetSwapHalt(HaltInit)
	id := agree(t, maker, taker)
	tr := waitSwap(t, taker, id, SwapAbandoned, 5*time.Second)
	if !strings.Contains(p17LastEvent(tr), "maker did not lock their coins within") {
		t.Fatalf("taker reason: %q", p17LastEvent(tr))
	}
	mr := waitSwap(t, maker, id, SwapAbandoned, 5*time.Second)
	if !strings.Contains(p17LastEvent(mr), "the taker gave up before locking coins") {
		t.Fatalf("maker reason: %q", p17LastEvent(mr))
	}
	if chain.balance(42, walletOwner(maker)) != 0 || chain.balance(0, walletOwner(taker)) != 0 {
		t.Fatalf("coins moved: %+v", chain.balances)
	}
}

// A funded maker within the timeout still completes.
func TestSwapP17Normal(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	enableP17Swaps(t, maker, chain)
	enableP17Swaps(t, taker, chain)
	id := agree(t, maker, taker)
	waitSwap(t, maker, id, SwapComplete, 5*time.Second)
	waitSwap(t, taker, id, SwapComplete, 5*time.Second)
}

func TestMakerTimeouts(t *testing.T) {
	if MakerContractTimeout(0) != 30*time.Minute || MakerContractTimeout(42) != 15*time.Minute || MakerContractTimeout(140) != 10*time.Minute {
		t.Fatalf("contract timeouts %s %s %s", MakerContractTimeout(0), MakerContractTimeout(42), MakerContractTimeout(140))
	}
	var c SwapConfig
	if c.MakerInitWait() != 3*time.Minute || c.makerGiveUp() != 150*time.Second {
		t.Fatalf("init %s give up %s", c.MakerInitWait(), c.makerGiveUp())
	}
}
