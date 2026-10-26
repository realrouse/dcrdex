// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

func newTestStore(t *testing.T) *SwapStore {
	t.Helper()
	store, err := NewSwapStore(t.TempDir(), testStoreKey)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// restartBooks simulates a bisonw restart: a new OrderBooks with the same
// keys that loads the swap store and resumes.
func restartBooks(t *testing.T, m *fakeMesh, old *OrderBooks, chain *fakeChain, store *SwapStore) *OrderBooks {
	t.Helper()
	nb := NewOrderBooks(m.client(old.self), old.priv, old.encPriv, tLogger)
	nb.cfg = fastCfg
	if err := nb.SubscribeInbox(tCtx); err != nil {
		t.Fatal(err)
	}
	owner := walletOwner(old)
	nb.EnableSwaps(fastSwapCfg, func(assetID uint32) (SwapWallet, error) {
		return &fakeSwapWallet{c: chain, asset: assetID, owner: owner}, nil
	}, store)
	if _, err := nb.LoadSwaps(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(tCtx)
	t.Cleanup(cancel)
	go nb.RunSwaps(ctx, 20*time.Millisecond)
	return nb
}

// kill stops a swap engine and drops all inbox traffic to and from it.
func kill(m *fakeMesh, stop context.CancelFunc, ob *OrderBooks) {
	stop()
	dead := ob.self
	m.setDrop(func(topic string, from peer.ID) bool { return topic == InboxTopic(dead) || from == dead })
	time.Sleep(60 * time.Millisecond)
}

func waitSwapCond(t *testing.T, ob *OrderBooks, id MatchID, what string, f func(*SwapRecord) bool) *SwapRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, ok := ob.SwapRecord(id)
		if ok && f(r) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("swap %s: %s not reached: %+v", id, what, r)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSwapStoreEncryption(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSwapStore(dir, testStoreKey)
	secret := bytes.Repeat([]byte{0xab}, 32)
	var id MatchID
	id[0] = 1
	rec := &SwapRecord{Match: Match{ID: id}, Side: sideMaker, Status: SwapInitSent, Secret: secret, SecretHash: sha256Sum(secret)}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, id.String()+".json"))
	for _, enc := range []string{hex.EncodeToString(secret), base64.StdEncoding.EncodeToString(secret)} {
		if strings.Contains(string(b), enc) {
			t.Fatal("secret on disk in the clear")
		}
	}
	if !strings.Contains(string(b), "encSecret") || rec.EncSecret != nil {
		t.Fatal("encSecret missing or record mutated")
	}
	recs, err := store.Load()
	if err != nil || len(recs) != 1 || !bytes.Equal(recs[0].Secret, secret) {
		t.Fatalf("load: %v %+v", err, recs)
	}
	wrong, _ := NewSwapStore(dir, bytes.Repeat([]byte{8}, 32))
	if _, err := wrong.Load(); err == nil {
		t.Fatal("wrong key decrypted the secret")
	}
	if _, err := NewSwapStore(dir, []byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

// Maker saves its signed contract, then "crashes" before broadcasting. After a
// restart it must re-initiate with the same funding coins, exactly once.
func TestSwapCrashBeforeBroadcast(t *testing.T) {
	t.Run("rpc", func(t *testing.T) { testSwapCrashBeforeBroadcast(t, false) })
	t.Run("spv", func(t *testing.T) { testSwapCrashBeforeBroadcast(t, true) })
}

func testSwapCrashBeforeBroadcast(t *testing.T, spv bool) {
	m, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	store := newTestStore(t)
	stopMaker := enableFakeSwaps(t, maker, chain, store)
	enableFakeSwaps(t, taker, chain, nil)
	_ = maker.SetSwapHalt(HaltPreBroadcast)
	id := agree(t, maker, taker)
	pending := waitSwap(t, maker, id, SwapInitPending, 5*time.Second)
	if pending.MakerSwap == nil || len(pending.Funding) != 1 {
		t.Fatalf("pending record incomplete: %+v", pending)
	}
	if chain.contractsBy(walletOwner(maker)) != 0 {
		t.Fatal("contract broadcast despite halt")
	}
	kill(m, stopMaker, maker)
	chain.setSPV(spv) // from here on, unmined and missing look the same
	m.setDrop(nil)
	maker2 := restartBooks(t, m, maker, chain, store)
	if spv {
		waitSwapCond(t, maker2, id, "re-initiated", func(r *SwapRecord) bool { return r.Status != SwapInitPending })
		chain.setSPV(false) // let the contracts confirm
	}
	mr := waitSwap(t, maker2, id, SwapComplete, 5*time.Second)
	waitSwap(t, taker, id, SwapComplete, 5*time.Second)
	if n := chain.contractsBy(walletOwner(maker)); n != 1 {
		t.Fatalf("%d maker contracts", n)
	}
	if !bytes.Equal(mr.Funding[0], pending.Funding[0]) {
		t.Fatal("retry did not reuse the funding coin")
	}
}

// Taker broadcasts, then crashes before recording it. After restart the saved
// contract is found on chain and not re-created.
func TestSwapCrashAfterBroadcast(t *testing.T) {
	t.Run("rpc", func(t *testing.T) { testSwapCrashAfterBroadcast(t, false, "found on chain (recovered)") })
	// An SPV wallet can't tell a mempool tx from a missing one; the spent
	// funding coins prove the broadcast.
	t.Run("spv", func(t *testing.T) { testSwapCrashAfterBroadcast(t, true, "funding coins spent by saved contract") })
}

func testSwapCrashAfterBroadcast(t *testing.T, spv bool, wantEvent string) {
	m, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	store := newTestStore(t)
	enableFakeSwaps(t, maker, chain, nil)
	stopTaker := enableFakeSwaps(t, taker, chain, store)
	chain.mtx.Lock()
	chain.crash[walletOwner(taker)] = crashAfterBroadcast
	crashed := make(chan struct{})
	dead := taker.self
	chain.onCrash = func() { // the process dies right after the broadcast
		stopTaker()
		chain.spv = spv // the new contract is only in the mempool (chain.mtx is held)
		chain.dead[walletOwner(taker)] = true
		m.setDrop(func(topic string, from peer.ID) bool { return topic == InboxTopic(dead) || from == dead })
		close(crashed)
	}
	chain.mtx.Unlock()
	_ = maker.SetSwapHalt(HaltRedeem) // keep the maker from finishing while the taker is down
	id := agree(t, maker, taker)
	select {
	case <-crashed:
	case <-time.After(5 * time.Second):
		t.Fatal("no crash")
	}
	time.Sleep(100 * time.Millisecond)
	recs, _ := store.Load()
	if len(recs) != 1 || recs[0].Status != SwapPartPending || recs[0].TakerSwap == nil {
		t.Fatalf("persisted %d records: %s", len(recs), recSummary(recs))
	}
	m.setDrop(nil)
	_ = maker.SetSwapHalt(HaltNone)
	chain.mtx.Lock()
	delete(chain.dead, walletOwner(taker))
	chain.mtx.Unlock()
	taker2 := restartBooks(t, m, taker, chain, store)
	if spv {
		waitSwapCond(t, taker2, id, "recovered", func(r *SwapRecord) bool { return r.Status != SwapPartPending })
		chain.setSPV(false)
	}
	tr := waitSwap(t, taker2, id, SwapComplete, 5*time.Second)
	waitSwap(t, maker, id, SwapComplete, 5*time.Second)
	if n := chain.contractsBy(walletOwner(taker)); n != 1 {
		t.Fatalf("%d taker contracts", n)
	}
	found := false
	for _, e := range tr.Events {
		found = found || strings.Contains(e.Msg, wantEvent)
	}
	if !found {
		t.Fatalf("no recovery event: %+v", tr.Events)
	}
}

func TestSwapTakerRestartAfterParticipate(t *testing.T) {
	m, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	store := newTestStore(t)
	enableFakeSwaps(t, maker, chain, nil)
	stopTaker := enableFakeSwaps(t, taker, chain, store)
	_ = taker.SetSwapHalt(HaltTakerRedeem)
	id := agree(t, maker, taker)
	waitSwap(t, taker, id, SwapPartSent, 5*time.Second)
	waitSwapCond(t, maker, id, "taker contract audited", func(r *SwapRecord) bool { return r.TakerSwap != nil })
	kill(m, stopTaker, taker)
	// The maker still redeems; its secret messages are lost.
	waitSwap(t, maker, id, SwapRedeemed, 5*time.Second)
	m.setDrop(nil)
	taker2 := restartBooks(t, m, taker, chain, store)
	waitSwap(t, taker2, id, SwapComplete, 5*time.Second)
	waitSwap(t, maker, id, SwapComplete, 5*time.Second)
}

func TestSwapMakerRestartAfterRedeem(t *testing.T) {
	m, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	store := newTestStore(t)
	stopMaker := enableFakeSwaps(t, maker, chain, store)
	enableFakeSwaps(t, taker, chain, nil)
	_ = taker.SetSwapHalt(HaltTakerRedeem)
	id := agree(t, maker, taker)
	waitSwap(t, maker, id, SwapRedeemed, 5*time.Second)
	kill(m, stopMaker, maker)
	// Taker redeems while the maker is down; the done ack is lost.
	_ = taker.SetSwapHalt(HaltNone)
	waitSwap(t, taker, id, SwapComplete, 5*time.Second)
	m.setDrop(nil)
	maker2 := restartBooks(t, m, maker, chain, store)
	// The restarted maker re-sends the secret; the finished taker re-acks.
	mr := waitSwap(t, maker2, id, SwapComplete, 5*time.Second)
	if mr.TakerRedeem == nil {
		t.Fatal("no taker redeem recorded")
	}
}

// Order cancels and expiry don't touch agreed matches or their swaps.
func TestSwapOrderCancelAndExpiry(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	go maker.RunMaintenance(tCtx, 50*time.Millisecond)
	go taker.RunMaintenance(tCtx, 50*time.Millisecond)

	// Cancel the rest of a partly matched order while its swap runs.
	_ = taker.SetSwapHalt(HaltParticipate)
	ord, err := maker.PostOrder(tCtx, dcrBTC, true, 4*lot, lot, 2e5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	res, err := taker.Take(tCtx, dcrBTC, false, 2*lot, 2e5, lot, false, time.Minute)
	if err != nil || len(res.Proposed) != 1 {
		t.Fatalf("take: %+v %v", res, err)
	}
	id1 := res.Proposed[0]
	waitStatus(t, maker, id1, MatchAgreed)
	waitSwap(t, taker, id1, SwapInitAudited, 5*time.Second)
	if err := maker.CancelOrder(tCtx, ord.ID()); err != nil {
		t.Fatal(err)
	}
	for _, ob := range obs {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, ok := remainingOn(t, ob, ord.ID()); !ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("cancelled order still on book")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// An order that expires mid-swap.
	ord2, err := maker.PostOrder(tCtx, dcrBTC, true, 2*lot, lot, 2e5, 700*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res, err = taker.Take(tCtx, dcrBTC, false, lot, 2e5, lot, false, time.Minute)
	if err != nil || len(res.Proposed) != 1 {
		t.Fatalf("take 2: %+v %v", res, err)
	}
	id2 := res.Proposed[0]
	waitSwap(t, taker, id2, SwapInitAudited, 5*time.Second)
	time.Sleep(900 * time.Millisecond)
	if len(maker.OwnOrders()) != 0 {
		t.Fatal("expired own order kept")
	}
	if _, ok := remainingOn(t, taker, ord2.ID()); ok {
		t.Fatal("expired order still on taker book")
	}
	_ = taker.SetSwapHalt(HaltNone)
	for _, id := range []MatchID{id1, id2} {
		waitSwap(t, taker, id, SwapComplete, 5*time.Second)
		waitSwap(t, maker, id, SwapComplete, 5*time.Second)
	}
}

func TestLockPolicy(t *testing.T) {
	for _, bad := range []LockPolicy{
		{Maker: time.Minute, Taker: 10 * time.Second},      // taker too short
		{Maker: 90 * time.Second, Taker: 80 * time.Second}, // gap too small
		{Maker: time.Hour, Taker: 50 * time.Minute},        // < 1.5x
		{Maker: 72 * time.Hour, Taker: time.Hour},          // too long
	} {
		if bad.Validate() == nil {
			t.Fatalf("%+v validated", bad)
		}
	}
	for name, p := range DefaultLockPolicies {
		if err := p.Validate(); err != nil {
			t.Fatalf("default %s: %v", name, err)
		}
	}
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	if p := maker.LockPolicy(dcrBTC); p != DefaultLockPolicies["dcr_btc"] {
		t.Fatalf("policy %+v", p)
	}
	// Policies differ: the maker rejects the proposal.
	short := LockPolicy{Maker: 10 * time.Minute, Taker: 5 * time.Minute}
	if err := maker.SetLockPolicy(dcrBTC, short); err != nil {
		t.Fatal(err)
	}
	if _, err := maker.PostOrder(tCtx, dcrBTC, true, 2*lot, lot, 2e5, time.Minute); err != nil {
		t.Fatal(err)
	}
	res, _ := taker.Take(tCtx, dcrBTC, false, lot, 2e5, lot, false, time.Minute)
	r := waitStatus(t, taker, res.Proposed[0], MatchRejected)
	if !strings.Contains(r.Reason, "lock policy") {
		t.Fatalf("reason %q", r.Reason)
	}
	// Same policy on both sides: agreed with the policy's lock times.
	_ = taker.SetLockPolicy(dcrBTC, short)
	res, _ = taker.Take(tCtx, dcrBTC, false, lot, 2e5, lot, false, time.Minute)
	mr := waitStatus(t, maker, res.Proposed[0], MatchAgreed).Match
	if mr.MakerLockTime-mr.Stamp/1000 != 600 || mr.TakerLockTime-mr.Stamp/1000 != 300 {
		t.Fatalf("lock times %d %d stamp %d", mr.MakerLockTime, mr.TakerLockTime, mr.Stamp)
	}
	if err := taker.SetLockPolicy(dcrBTC, LockPolicy{Maker: time.Minute, Taker: time.Minute}); err == nil {
		t.Fatal("invalid override accepted")
	}
}

func recSummary(recs []*SwapRecord) string {
	var b strings.Builder
	for _, r := range recs {
		fmt.Fprintf(&b, "[%s %s takerSwap=%v] ", r.Side, r.Status, r.TakerSwap != nil)
	}
	return b.String()
}
