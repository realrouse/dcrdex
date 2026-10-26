// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// P6 adversarial and edge-case tests.

// newNegotiatorsClock is newNegotiators with a per-peer adjustable clock.
func newNegotiatorsClock(t *testing.T, n int) (*fakeMesh, []*OrderBooks, []*atomic.Int64) {
	t.Helper()
	m := newFakeMesh()
	obs := make([]*OrderBooks, n)
	offs := make([]*atomic.Int64, n)
	for i := range obs {
		ob := newTestBooks(t, m, nil)
		off := new(atomic.Int64)
		ob.now = func() time.Time { return time.Now().Add(time.Duration(off.Load())) }
		ob.cfg = fastCfg
		addr := ob.self.String()[len(ob.self.String())-6:]
		ob.SetAddressFunc(func(assetID uint32) (string, error) { return addr + "-asset" + string(rune('0'+assetID%10)), nil })
		if err := ob.SubscribeInbox(tCtx); err != nil {
			t.Fatal(err)
		}
		if err := ob.SubscribeMarket(tCtx, dcrBTC); err != nil {
			t.Fatal(err)
		}
		obs[i], offs[i] = ob, off
	}
	return m, obs, offs
}

func waitReason(t *testing.T, ob *OrderBooks, id MatchID, sub string) *SwapRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _ := ob.SwapRecord(id)
		if r != nil && strings.Contains(r.Reason, sub) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("swap %s: no reason %q: %+v", id, sub, r)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lastEvent(r *SwapRecord) string {
	if len(r.Events) == 0 {
		return ""
	}
	return r.Events[len(r.Events)-1].Msg
}

func zeroBalances(t *testing.T, chain *fakeChain, obs ...*OrderBooks) {
	t.Helper()
	for _, ob := range obs {
		for _, a := range []uint32{0, 42} {
			if b := chain.balance(a, walletOwner(ob)); b != 0 {
				t.Fatalf("%s asset %d net balance %d, want 0", walletOwner(ob), a, b)
			}
		}
	}
}

// A malicious maker announces the agreed terms but locks a wrong contract.
// The taker must never participate, must abandon before its lock time, and
// the maker gets only its own (refunded) coins back.
func TestP6WrongMakerContract(t *testing.T) {
	for _, tc := range []struct{ mode, reason string }{
		{TamperValue, "maker contract rejected: value"},
		{TamperSecretHash, "maker contract rejected: secret hash"},
		{TamperLockTime, "maker contract rejected: lock time"},
		{TamperRecipient, "maker contract rejected: recipient"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			_, obs, offs := newNegotiatorsClock(t, 2)
			maker, taker := obs[0], obs[1]
			chain := newFakeChain()
			enableFakeSwaps(t, maker, chain, nil)
			enableFakeSwaps(t, taker, chain, nil)
			if err := maker.SetSwapTamper(tc.mode); err != nil {
				t.Fatal(err)
			}
			id := agree(t, maker, taker)
			r := waitReason(t, taker, id, tc.reason)
			if r.TakerSwap != nil || chain.contractsBy(walletOwner(taker)) != 0 {
				t.Fatalf("taker acted on a bad contract: %+v", r)
			}
			waitSwap(t, maker, id, SwapInitSent, 5*time.Second)
			// Just before the taker lock time (less the safety margin) the
			// taker gives up without having risked anything.
			offs[1].Store(int64(time.Hour))
			ar := waitSwap(t, taker, id, SwapAbandoned, 5*time.Second)
			if ev := lastEvent(ar); !strings.Contains(ev, "no valid maker contract") {
				t.Fatalf("abandon event %q", ev)
			}
			chain.advance(2*time.Hour + time.Minute)
			waitSwap(t, maker, id, SwapRefunded, 5*time.Second)
			zeroBalances(t, chain, maker, taker)
		})
	}
}

// A malicious taker participates with a wrong contract. The maker must not
// redeem (which would reveal the secret), and both sides refund.
func TestP6WrongTakerContract(t *testing.T) {
	for _, tc := range []struct{ mode, reason string }{
		{TamperValue, "taker contract rejected: value"},
		{TamperSecretHash, "taker contract rejected: secret hash"},
		{TamperLockTime, "taker contract rejected: lock time"},
		{TamperRecipient, "taker contract rejected: recipient"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			_, obs := newNegotiators(t, 2)
			maker, taker := obs[0], obs[1]
			chain := newFakeChain()
			enableFakeSwaps(t, maker, chain, nil)
			enableFakeSwaps(t, taker, chain, nil)
			if err := taker.SetSwapTamper(tc.mode); err != nil {
				t.Fatal(err)
			}
			id := agree(t, maker, taker)
			waitSwap(t, taker, id, SwapPartSent, 5*time.Second)
			r := waitReason(t, maker, id, tc.reason)
			if r.Status != SwapInitSent || r.MakerRedeem != nil {
				t.Fatalf("maker acted on a bad contract: %+v", r)
			}
			chain.advance(time.Hour + time.Minute)
			waitSwap(t, taker, id, SwapRefunded, 5*time.Second)
			chain.advance(time.Hour)
			waitSwap(t, maker, id, SwapRefunded, 5*time.Second)
			zeroBalances(t, chain, maker, taker)
		})
	}
}

// A maker that never initiates: the taker abandons with nothing at stake.
func TestP6MakerNeverInitiates(t *testing.T) {
	_, obs, offs := newNegotiatorsClock(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	_ = maker.SetSwapHalt(HaltInit)
	id := agree(t, maker, taker)
	waitSwap(t, taker, id, SwapNew, 5*time.Second)
	offs[1].Store(int64(time.Hour))
	r := waitSwap(t, taker, id, SwapAbandoned, 5*time.Second)
	if ev := lastEvent(r); !strings.Contains(ev, "maker never initiated") {
		t.Fatalf("abandon event %q", ev)
	}
	zeroBalances(t, chain, maker, taker)
}

// Insufficient funds or a locked wallet: Initiate fails. Nothing is lost and
// both sides end terminal once the swap can no longer start safely.
func TestP6InitiateFails(t *testing.T) {
	_, obs, offs := newNegotiatorsClock(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	chain.mtx.Lock()
	if chain.dead == nil {
		chain.dead = map[string]bool{}
	}
	chain.dead[walletOwner(maker)] = true // every Initiate fails
	chain.mtx.Unlock()
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	id := agree(t, maker, taker)
	time.Sleep(200 * time.Millisecond)
	if r, _ := maker.SwapRecord(id); r.MakerSwap != nil {
		t.Fatalf("maker has a contract: %+v", r)
	}
	offs[0].Store(int64(time.Hour))
	offs[1].Store(int64(time.Hour))
	waitSwap(t, maker, id, SwapAbandoned, 5*time.Second)
	waitSwap(t, taker, id, SwapAbandoned, 5*time.Second)
	zeroBalances(t, chain, maker, taker)
}

func sealTo(t *testing.T, from, to *OrderBooks, typ string, payload any) []byte {
	t.Helper()
	b, err := from.sealInbox(to.self, to.encPriv.PubKey(), typ, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestP6InboxReplayTamperSpoof(t *testing.T) {
	m := newFakeMesh()
	clock := time.Now()
	a, b, c := newTestBooks(t, m, &clock), newTestBooks(t, m, &clock), newTestBooks(t, m, &clock)
	msg := sealTo(t, a, b, "t", map[string]int{"x": 1})
	if _, err := b.receiveInbox(msg, a.self); err != nil {
		t.Fatal(err)
	}
	// Replay inside the window.
	if _, err := b.receiveInbox(msg, a.self); !errors.Is(err, errReplayed) {
		t.Fatalf("replay: %v", err)
	}
	// Every single-byte tamper fails to open.
	for i := 0; i < len(msg); i++ {
		bad := append([]byte(nil), msg...)
		bad[i] ^= 0x01
		if _, err := b.openInbox(bad, a.self); err == nil {
			t.Fatalf("tampered byte %d accepted", i)
		}
	}
	// Re-attributed to another sender (a relay lying about the origin).
	msg2 := sealTo(t, a, b, "t", 2)
	if _, err := b.receiveInbox(msg2, c.self); err == nil {
		t.Fatal("spoofed sender accepted")
	}
	// Meant for someone else.
	msg3 := sealTo(t, a, c, "t", 3)
	if _, err := b.receiveInbox(msg3, a.self); err == nil {
		t.Fatal("message for c opened by b")
	}
	// Truncated and garbage input never panics.
	for i := 0; i < len(msg2); i++ {
		_, _ = b.openInbox(msg2[:i], a.self)
	}
	for i := 0; i < 2000; i++ {
		g := make([]byte, i%1200)
		_, _ = rand.Read(g)
		if i%2 == 0 && len(g) > 0 {
			g[0] = inboxVersion
		}
		if _, err := b.openInbox(g, a.self); err == nil {
			t.Fatal("garbage opened")
		}
	}
}

func TestP6InboxClockSkew(t *testing.T) {
	m := newFakeMesh()
	var sendClock, recvClock time.Time
	a, b := newTestBooks(t, m, &sendClock), newTestBooks(t, m, &recvClock)
	base := time.Now()
	for _, tc := range []struct {
		skew time.Duration // sender clock relative to receiver
		ok   bool
	}{
		{0, true},
		{-MaxInboxMsgAge + time.Second, true},
		{-MaxInboxMsgAge - time.Second, false},
		{MaxClockSkew - time.Second, true},
		{MaxClockSkew + time.Second, false},
	} {
		recvClock, sendClock = base, base.Add(tc.skew)
		_, err := b.receiveInbox(sealTo(t, a, b, "t", tc.skew.String()), a.self)
		if (err == nil) != tc.ok {
			t.Fatalf("skew %v: err %v, want ok=%v", tc.skew, err, tc.ok)
		}
	}
	// A message accepted near the edge can't be replayed later: by the time
	// it leaves the replay cache it is outside the stamp window.
	recvClock, sendClock = base, base
	msg := sealTo(t, a, b, "t", "late")
	if _, err := b.receiveInbox(msg, a.self); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{time.Minute, MaxInboxMsgAge - time.Second, MaxInboxMsgAge + MaxClockSkew + time.Second} {
		recvClock = base.Add(d)
		if _, err := b.receiveInbox(msg, a.self); err == nil {
			t.Fatalf("replay accepted after %v", d)
		}
	}
}

func TestP6InboxFlood(t *testing.T) {
	m := newFakeMesh()
	clock := time.Now()
	a, b, c := newTestBooks(t, m, &clock), newTestBooks(t, m, &clock), newTestBooks(t, m, &clock)
	ok := 0
	for i := 0; i < 1000; i++ {
		if _, err := b.receiveInbox(sealTo(t, a, b, "t", i), a.self); err == nil {
			ok++
		} else if !errors.Is(err, errRateLimited) {
			t.Fatal(err)
		}
	}
	if ok != InboxBurst {
		t.Fatalf("accepted %d of a 1000 burst, want %v", ok, InboxBurst)
	}
	if b.InboxDropped() < 900 {
		t.Fatalf("dropped %d", b.InboxDropped())
	}
	// Another peer is not starved by a flooder.
	if _, err := b.receiveInbox(sealTo(t, c, b, "t", 0), c.self); err != nil {
		t.Fatalf("honest peer blocked: %v", err)
	}
	// The flooder recovers at the sustained rate.
	clock = clock.Add(time.Second)
	ok = 0
	for i := 0; i < 100; i++ {
		if _, err := b.receiveInbox(sealTo(t, a, b, "t", 1000+i), a.self); err == nil {
			ok++
		}
	}
	if ok < InboxRate-1 || ok > InboxRate+1 {
		t.Fatalf("after 1s accepted %d, want ~%v", ok, InboxRate)
	}
	// Garbage floods are rate limited before any crypto work.
	for i := 0; i < 500; i++ {
		_, _ = b.receiveInbox([]byte("junk"), c.self)
	}
	if _, err := b.receiveInbox([]byte("junk"), c.self); !errors.Is(err, errRateLimited) {
		t.Fatalf("junk flood not limited: %v", err)
	}
}

func TestP6OrderFloodsAndCaps(t *testing.T) {
	alice := newPriv(t)
	aliceID := pid(t, alice)
	book := NewBook(dcrBTC)
	for i := 0; i < MaxOrdersPerOwner+5; i++ {
		o := signedOrder(t, alice, tNow)
		o.Nonce = uint64(i)
		_ = o.Sign(alice)
		_, err := book.AddOrder(o, aliceID, tNow)
		if (err == nil) != (i < MaxOrdersPerOwner) {
			t.Fatalf("order %d: %v", i, err)
		}
	}
	// Our own orders (sender "") are not capped by the remote limit.
	bob := newPriv(t)
	for i := 0; i < MaxOrdersPerOwner+1; i++ {
		o := signedOrder(t, bob, tNow)
		o.Nonce = uint64(i)
		_ = o.Sign(bob)
		if _, err := book.AddOrder(o, "", tNow); err != nil {
			t.Fatal(err)
		}
	}
	// Cancel spam for unknown orders is bounded.
	var lastErr error
	for i := 0; i < maxTombstones+10; i++ {
		var id OrderID
		copy(id[:], fmt.Sprintf("%032d", i))
		_, lastErr = book.Cancel(signedCancel(t, alice, id, tNow), aliceID, tNow)
	}
	if lastErr == nil {
		t.Fatal("tombstones not capped")
	}
	book.mtx.Lock()
	n := len(book.cancelled)
	book.mtx.Unlock()
	if n > maxTombstones {
		t.Fatalf("%d tombstones", n)
	}
	// Expired tombstones make room again.
	later := tNow.Add(MaxOrderLifetime + time.Minute)
	var id OrderID
	copy(id[:], "fresh-after-expiry-xxxxxxxxxxxxx")
	if _, err := book.Cancel(signedCancel(t, alice, id, later), aliceID, later); err != nil {
		t.Fatalf("cancel after prune: %v", err)
	}
}

func TestP6DustAndLotBoundaries(t *testing.T) {
	alice := newPriv(t)
	for _, tc := range []struct {
		lot, rate uint64
		ok        bool
	}{
		{MinSwapValue, 1e8, true},      // base lot at the floor, quote = lot
		{MinSwapValue - 1, 1e8, false}, // base lot below the floor
		{1e8, 1e4, true},               // quote per lot exactly 10000
		{1e8, 1e4 - 1, false},          // quote per lot 9999
		{1, 1e16, false},               // tiny base lot, large rate
		{1e8, 2e5, true},               // ordinary
	} {
		o := &MeshOrder{Market: dcrBTC, Sell: true, Qty: 3 * tc.lot, LotSize: tc.lot, Rate: tc.rate, EncPubKey: tEncPub,
			Stamp: tNow.UnixMilli(), Expiry: tNow.Add(lifetime).UnixMilli()}
		_ = o.Sign(alice)
		err := o.Validate(tNow)
		if (err == nil) != tc.ok {
			t.Fatalf("lot %d rate %d: %v", tc.lot, tc.rate, err)
		}
	}
	// PostOrder refuses dust too, and a non-multiple qty.
	_, obs := newNegotiators(t, 1)
	if _, err := obs[0].PostOrder(tCtx, dcrBTC, true, 1000, 1000, 2e5, time.Minute); err == nil {
		t.Fatal("dust order posted")
	}
	if _, err := obs[0].PostOrder(tCtx, dcrBTC, true, 3*lot/2, lot, 2e5, time.Minute); err == nil {
		t.Fatal("non-multiple qty posted")
	}
}

func TestP6SelfMatch(t *testing.T) {
	_, obs := newNegotiators(t, 1)
	ob := obs[0]
	ord, err := ob.PostOrder(tCtx, dcrBTC, true, 2*lot, lot, 2e5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ob.Take(tCtx, dcrBTC, false, 2*lot, 2e5, lot, false, time.Minute)
	if err == nil && len(res.Proposed) != 0 {
		t.Fatalf("took own order: %+v", res)
	}
	// A crafted proposal claiming to come from ourselves is dropped.
	stamp := time.Now().UnixMilli()
	p := &MatchProposal{MakerOrderID: ord.ID(), Taker: ob.self.String(), Qty: lot, Nonce: 1, Stamp: stamp,
		MatchID: computeMatchID(ord.ID(), ob.self.String(), 1, stamp)}
	ob.handlePropose(tCtx, ob.self, p)
	time.Sleep(50 * time.Millisecond)
	if len(ob.Matches()) != 0 {
		t.Fatalf("self match recorded: %+v", ob.Matches())
	}
}

// Many takers race for the same order. The maker never commits more than the
// order quantity, however the proposals interleave.
func TestP6DuplicateTakes(t *testing.T) {
	_, obs := newNegotiators(t, 6)
	maker := obs[0]
	if _, err := maker.PostOrder(tCtx, dcrBTC, true, 3*lot, lot, 2e5, time.Minute); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, tk := range obs[1:] {
		wg.Add(1)
		go func(tk *OrderBooks) {
			defer wg.Done()
			for i := 0; i < 3; i++ { // each taker also retries the same take
				_, _ = tk.Take(tCtx, dcrBTC, false, 2*lot, 2e5, lot, false, time.Minute)
			}
		}(tk)
	}
	wg.Wait()
	time.Sleep(time.Second) // let proposals resolve or time out
	var agreed uint64
	for _, r := range maker.Matches() {
		if r.Status == MatchAgreed {
			agreed += r.Match.Qty
		}
	}
	if agreed > 3*lot || agreed == 0 {
		t.Fatalf("maker agreed %d atoms of a %v order", agreed, 3*lot)
	}
	// Takers can't believe in more agreed matches than the maker has.
	var takerAgreed uint64
	ids := map[MatchID]bool{}
	for _, tk := range obs[1:] {
		for _, r := range tk.Matches() {
			if r.Status == MatchAgreed {
				takerAgreed += r.Match.Qty
				ids[r.Match.ID] = true
			}
		}
	}
	for _, r := range maker.Matches() {
		if ids[r.Match.ID] && r.Status != MatchAgreed {
			t.Fatalf("taker thinks %s agreed, maker has %s", r.Match.ID, r.Status)
		}
	}
	if takerAgreed > agreed {
		t.Fatalf("takers agreed %d > maker %d", takerAgreed, agreed)
	}
}

// Forged order traffic on the market topic never reaches the book.
func TestP6ForgedOrders(t *testing.T) {
	m := newFakeMesh()
	clock := time.Now()
	a, b := newTestBooks(t, m, &clock), newTestBooks(t, m, &clock)
	if err := b.SubscribeMarket(tCtx, dcrBTC); err != nil {
		t.Fatal(err)
	}
	mallory := newPriv(t)
	o := signedOrder(t, mallory, clock)
	o.Owner = a.self.String() // claim to be a
	if err := o.Validate(clock); err == nil {
		t.Fatal("spoofed owner validated")
	}
	o2 := signedOrder(t, mallory, clock)
	o2.Rate++ // tampered after signing
	if err := o2.Validate(clock); err == nil {
		t.Fatal("tampered order validated")
	}
	o3 := signedOrder(t, mallory, clock.Add(MaxClockSkew+time.Second))
	if err := o3.Validate(clock); err == nil {
		t.Fatal("future order validated")
	}
	_ = peer.ID("")
	if n := bookLen(t, b); n != 0 {
		t.Fatalf("book has %d", n)
	}
	_ = context.Background
}

// Replays and junk injected through the mesh are dropped by the receiver.
func TestP6FloodInjection(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	a, b := obs[0], obs[1]
	a.EnableAdversary()
	if _, err := a.Flood(tCtx, b.self, "replay", 10, 0); err == nil {
		t.Fatal("replay with nothing captured")
	}
	a.sendInbox(tCtx, b.self, b.encPriv.PubKey(), "ping", 1)
	time.Sleep(50 * time.Millisecond)
	before := b.InboxDropped()
	res, err := a.Flood(tCtx, b.self, "replay", 50, 0)
	if err != nil || res.Sent != 50 {
		t.Fatalf("flood: %+v %v", res, err)
	}
	if _, err := a.Flood(tCtx, b.self, "junk", 200, 300); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if d := b.InboxDropped() - before; d < 50 {
		t.Fatalf("only %d replays dropped", d)
	}
}

// P6 campaign bug: a contract broadcast failed because another tx had spent
// its funding coins. The retry saw "funding spent" and wrongly assumed our
// saved contract had been broadcast, so the swap sat in init_sent forever
// with "refund error: coin not found". Now it stays pending, sends nothing,
// and ends abandoned once its own lock time has passed.
func TestP6FundingSpentElsewhere(t *testing.T) {
	_, obs, offs := newNegotiatorsClock(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	chain.mtx.Lock()
	if chain.crash == nil {
		chain.crash = map[string]string{}
	}
	chain.crash[walletOwner(maker)] = fundingStolen
	chain.mtx.Unlock()
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	id := agree(t, maker, taker)
	waitReason(t, maker, id, "unknown to the wallet")
	if r, _ := maker.SwapRecord(id); r.Status != SwapInitPending {
		t.Fatalf("maker claims a contract it never broadcast: %s", r.Status)
	}
	if r, _ := taker.SwapRecord(id); r.Status != SwapNew {
		t.Fatalf("taker %s", r.Status)
	}
	// Past the taker lock time: the taker abandons; past the maker lock time
	// the maker gives up on the missing contract.
	offs[0].Store(int64(time.Hour + time.Minute))
	offs[1].Store(int64(time.Hour + time.Minute))
	waitSwap(t, taker, id, SwapAbandoned, 5*time.Second)
	chain.advance(2*time.Hour + time.Minute)
	r := waitSwap(t, maker, id, SwapAbandoned, 5*time.Second)
	if ev := lastEvent(r); !strings.Contains(ev, "never found on chain") {
		t.Fatalf("event %q", ev)
	}
	zeroBalances(t, chain, maker, taker)
}

// A record already in init_sent whose contract doesn't exist (written by the
// old code) is abandoned after the lock time instead of retrying forever.
func TestP6MissingContractAfterLock(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	_ = taker.SetSwapHalt(HaltParticipate)
	id := agree(t, maker, taker)
	r := waitSwap(t, maker, id, SwapInitSent, 5*time.Second)
	chain.mtx.Lock()
	delete(chain.contracts, string(r.MakerSwap.CoinID)) // the tx vanished
	chain.mtx.Unlock()
	chain.advance(2*time.Hour + time.Minute)
	waitReason(t, maker, id, "not found by the wallet")
	waitSwap(t, maker, id, SwapAbandoned, 5*time.Second)
}
