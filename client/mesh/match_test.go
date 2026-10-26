// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"decred.org/dcrdex/dex/calc"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Ported from the legacy tatanka/client/trade/compat_test.go.
func TestMinimumLotSize(t *testing.T) {
	p := &FeeParameters{MaxFeeExposure: 0.01, BaseFeesPerMatch: 5, QuoteFeesPerMatch: 5}
	msgRate := uint64(calc.RateEncodingFactor)
	if l := MinimumLotSize(msgRate, p); l != 1024 {
		t.Fatal(p, l)
	}
	p.QuoteFeesPerMatch *= 2
	p.BaseFeesPerMatch *= 2
	if l := MinimumLotSize(msgRate, p); l != 2048 {
		t.Fatal(p, l)
	}
	p.QuoteFeesPerMatch *= 2
	msgRate *= 2
	if l := MinimumLotSize(msgRate, p); l != 2048 {
		t.Fatal(p, l)
	}
}

func TestOrderIsMatchable(t *testing.T) {
	p := &FeeParameters{MaxFeeExposure: 0.01, BaseFeesPerMatch: 5, QuoteFeesPerMatch: 5}
	var desiredQty, lotSize uint64 = 1024, 1024
	theirOrd := &BookOrder{MeshOrder: &MeshOrder{LotSize: lotSize, Rate: calc.RateEncodingFactor, Qty: lotSize * 2}, Remaining: lotSize * 2}
	if ok, reason := OrderIsMatchable(desiredQty, theirOrd, p); !ok {
		t.Fatal(reason)
	}
	p.MaxFeeExposure /= 4
	if ok, reason := OrderIsMatchable(desiredQty, theirOrd, p); ok || reason != ReasonTheirQtyTooSmall {
		t.Fatal(ok, reason)
	}
	p.MaxFeeExposure *= 4
	if ok, reason := OrderIsMatchable(desiredQty/2, theirOrd, p); ok || reason != ReasonOurQtyTooSmall {
		t.Fatal(ok, reason)
	}
	// Remaining (after partial fills), not the original qty, counts.
	theirOrd.Remaining = lotSize / 2
	if ok, _ := OrderIsMatchable(desiredQty, theirOrd, p); ok {
		t.Fatal("partially filled order should be too small")
	}
}

// Ported from the legacy tatanka/client/trade/match_test.go, plus owner
// skipping and partial remaining.
func TestMatchBook(t *testing.T) {
	weSell := false
	var weWantLots uint64 = 1
	var lotSize, msgRate, baseQty uint64
	var desire *DesiredTrade
	var ords []*BookOrder
	var p *FeeParameters
	mk := func(rate, qty uint64, owner string, stamp int64) *BookOrder {
		return &BookOrder{MeshOrder: &MeshOrder{Rate: rate, Qty: qty, LotSize: lotSize, Sell: !weSell, Owner: owner, Stamp: stamp}, Remaining: qty}
	}
	reset := func() {
		lotSize = 1024
		msgRate = calc.RateEncodingFactor
		baseQty = weWantLots * lotSize
		p = &FeeParameters{MaxFeeExposure: 0.01, BaseFeesPerMatch: 5, QuoteFeesPerMatch: 5}
		desire = &DesiredTrade{Qty: baseQty, Rate: msgRate, Sell: weSell}
		ords = []*BookOrder{mk(msgRate, baseQty, "maker1", 1)}
	}
	test := func(expRemain uint64, expQtys []uint64) {
		t.Helper()
		sortBookOrders(ords)
		matches, remain := MatchBook(desire, p, ords, "me")
		if remain != expRemain {
			t.Fatal("wrong remain", remain, expRemain)
		}
		if len(matches) != len(expQtys) {
			t.Fatal("wrong number of matches", len(matches), len(expQtys))
		}
		for i, m := range matches {
			if m.Qty != expQtys[i] {
				t.Fatal("wrong qty", i, m.Qty, expQtys[i])
			}
		}
	}

	reset()
	test(0, []uint64{baseQty})
	ords[0].Qty *= 2
	ords[0].Remaining *= 2
	test(0, []uint64{baseQty})
	desire.Qty *= 3
	test(baseQty, []uint64{2 * baseQty})
	ords = append(ords, mk(msgRate, baseQty, "maker2", 2))
	test(0, []uint64{2 * baseQty, baseQty})
	ords[1].Rate *= 2
	test(baseQty, []uint64{2 * baseQty})

	weWantLots = 4
	reset()
	test(0, []uint64{baseQty})

	weSell = true
	reset()
	test(0, []uint64{baseQty})
	desire.Qty *= 2
	test(baseQty, []uint64{baseQty})
	ords = append(ords, mk(msgRate, baseQty, "maker2", 2))
	test(0, []uint64{baseQty, baseQty})
	ords[1].Rate -= 1
	test(baseQty, []uint64{baseQty})

	weSell, weWantLots = false, 1
	reset()
	test(0, []uint64{baseQty})
	p.MaxFeeExposure /= 2
	test(baseQty, nil)

	// Own orders are skipped.
	reset()
	ords[0].Owner = "me"
	test(baseQty, nil)

	// Better price first, then time priority.
	reset()
	desire.Qty = 2 * baseQty
	desire.Rate = 2 * msgRate
	ords = []*BookOrder{mk(2*msgRate, baseQty, "late", 5), mk(msgRate, baseQty, "cheap", 9), mk(2*msgRate, baseQty, "early", 1)}
	sortBookOrders(ords)
	matches, _ := MatchBook(desire, p, ords, "me")
	if len(matches) != 2 || matches[0].Order.Owner != "cheap" || matches[1].Order.Owner != "early" {
		t.Fatalf("wrong priority")
	}

	// A partially filled order only offers whole lots of its remainder.
	reset()
	desire.Qty = 4 * lotSize
	ords[0].Qty, ords[0].Remaining = 4*lotSize, 2*lotSize+10
	test(2*lotSize, []uint64{2 * lotSize})
}

// Mesh publishes are limited to codec.MaxMessageSize (1 KiB) including the
// topic and protobuf framing, and pushes add the sender ID. Keep our largest
// messages well under that.
func TestMessageSizes(t *testing.T) {
	const budget = 1024 - 200
	m := newFakeMesh()
	a, b := newTestBooks(t, m, nil), newTestBooks(t, m, nil)
	longAddr := strings.Repeat("x", 90)
	var id OrderID
	p := &MatchProposal{MatchID: id, MakerOrderID: id, Taker: a.self.String(),
		TakerEncPubKey: a.encPriv.PubKey().SerializeCompressed(), Qty: 1 << 62, Rate: 1 << 62,
		Stamp: 1 << 62, Nonce: 1 << 63, TakerAddr: longAddr, MakerLockTime: 1 << 40, TakerLockTime: 1 << 40}
	for name, payload := range map[string]any{
		msgMatchPropose: p,
		msgMatchAccept:  &MatchAccept{MatchID: id, Qty: 1 << 62, MakerAddr: longAddr},
		msgMatchReject:  &MatchReject{MatchID: id, Reason: strings.Repeat("r", 100)},
	} {
		sealed, err := a.sealInbox(b.self, b.encPriv.PubKey(), name, payload)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(sealed) + len(InboxTopic(b.self)); n > budget {
			t.Fatalf("%s is %d bytes, budget %d", name, n, budget)
		}
	}
	ord := &MeshOrder{Market: dcrBTC, Qty: 1 << 62, LotSize: 1 << 62, Rate: 1 << 62, Stamp: 1 << 62,
		Expiry: 1 << 62, Nonce: 1 << 63, EncPubKey: tEncPub}
	_ = ord.Sign(a.priv)
	upd := &MeshOrderUpdate{OrderID: id, Remaining: 1 << 62, Stamp: 1 << 62}
	_ = upd.Sign(a.priv)
	for _, msg := range []*bookMsg{{Type: msgTypeOrder, Order: ord}, {Type: msgTypeUpdate, Update: upd}} {
		enc, _ := encodeBookMsg(msg)
		if len(enc)+len("orderbook.dcr_btc") > budget {
			t.Fatalf("%s is %d bytes", msg.Type, len(enc))
		}
	}
}

func TestInboxEncryption(t *testing.T) {
	m := newFakeMesh()
	a, b, c := newTestBooks(t, m, nil), newTestBooks(t, m, nil), newTestBooks(t, m, nil)
	payload := &MatchReject{Reason: "hello"}
	sealed, err := a.sealInbox(b.self, b.encPriv.PubKey(), msgMatchReject, payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "hello") {
		t.Fatal("plaintext visible")
	}
	msg, err := b.openInbox(sealed, a.self)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	var got MatchReject
	if err := json.Unmarshal(msg.Payload, &got); err != nil || got.Reason != "hello" || msg.Type != msgMatchReject {
		t.Fatalf("wrong payload %+v %v", got, err)
	}
	// Wrong recipient, wrong publisher, tampering, header swap.
	if _, err := c.openInbox(sealed, a.self); err == nil {
		t.Fatal("third party opened message")
	}
	if _, err := b.openInbox(sealed, c.self); err == nil {
		t.Fatal("accepted message published by someone else")
	}
	tampered := append([]byte{}, sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := b.openInbox(tampered, a.self); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	tampered = append([]byte{}, sealed...)
	tampered[5] ^= 1 // ephemeral key
	if _, err := b.openInbox(tampered, a.self); err == nil {
		t.Fatal("tampered ephemeral key opened")
	}
	// c encrypting to b but claiming to be a (signature check).
	c2 := NewOrderBooks(nil, c.priv, c.encPriv, tLogger)
	c2.self = a.self
	forged, _ := c2.sealInbox(b.self, b.encPriv.PubKey(), msgMatchReject, payload)
	if _, err := b.openInbox(forged, a.self); err == nil {
		t.Fatal("forged signature accepted")
	}
	// Stale message.
	old := time.Now().Add(-2 * MaxInboxMsgAge)
	a.now = func() time.Time { return old }
	stale, _ := a.sealInbox(b.self, b.encPriv.PubKey(), msgMatchReject, payload)
	if _, err := b.openInbox(stale, a.self); err == nil {
		t.Fatal("stale message accepted")
	}
}

var fastCfg = NegotiationConfig{
	RetryInterval:      30 * time.Millisecond,
	ProposalTimeout:    400 * time.Millisecond,
	ReservationTimeout: 400 * time.Millisecond,
	MakerLockTime:      2 * time.Hour,
	TakerLockTime:      time.Hour,
	Fees:               DefaultNegotiationConfig.Fees,
}

func newNegotiators(t *testing.T, n int) (*fakeMesh, []*OrderBooks) {
	t.Helper()
	m := newFakeMesh()
	obs := make([]*OrderBooks, n)
	for i := range obs {
		ob := newTestBooks(t, m, nil)
		ob.cfg = fastCfg
		addr := ob.self.String()[len(ob.self.String())-6:]
		ob.SetAddressFunc(func(assetID uint32) (string, error) { return addr + "-asset" + string(rune('0'+assetID%10)), nil })
		if err := ob.SubscribeInbox(tCtx); err != nil {
			t.Fatal(err)
		}
		if err := ob.SubscribeMarket(tCtx, dcrBTC); err != nil {
			t.Fatal(err)
		}
		obs[i] = ob
	}
	return m, obs
}

func waitStatus(t *testing.T, ob *OrderBooks, id MatchID, status string) *MatchRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, ok := ob.MatchRecord(id)
		if ok && r.Status == status {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("match %s: want status %s, have %+v", id, status, r)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func remainingOn(t *testing.T, ob *OrderBooks, id OrderID) (uint64, bool) {
	t.Helper()
	ords, _ := ob.Book(dcrBTC)
	for _, o := range ords {
		if o.ID() == id {
			return o.Remaining, true
		}
	}
	return 0, false
}

const lot = 1e8

func TestNegotiateFullAndPartialFills(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	ord, err := maker.PostOrder(tCtx, dcrBTC, true, 4*lot, lot, 2e5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Take 3 of 4 lots.
	res, err := taker.Take(tCtx, dcrBTC, false, 3*lot, 2e5, lot, false, time.Minute)
	if err != nil || len(res.Proposed) != 1 || res.Remain != 0 {
		t.Fatalf("take: %+v %v", res, err)
	}
	id := res.Proposed[0]
	tr := waitStatus(t, taker, id, MatchAgreed)
	mr := waitStatus(t, maker, id, MatchAgreed)
	if tr.AgreementHash != mr.AgreementHash || tr.AgreementHash == "" {
		t.Fatalf("agreement differs:\n%+v\n%+v", tr.Match, mr.Match)
	}
	if tr.Match != mr.Match {
		t.Fatalf("match records differ:\n%+v\n%+v", tr.Match, mr.Match)
	}
	m := tr.Match
	if m.Qty != 3*lot || m.Rate != 2e5 || m.QuoteQty != calc.BaseToQuote(2e5, 3*lot) || !m.MakerSell ||
		m.Maker != maker.self.String() || m.Taker != taker.self.String() || m.MakerOrderID != ord.ID() ||
		m.Initiator != "maker" || m.MakerAddr == "" || m.TakerAddr == "" || m.MakerAddr == m.TakerAddr ||
		m.MakerLockTime-m.TakerLockTime != 3600 {
		t.Fatalf("bad match %+v", m)
	}
	if tr.Side != "taker" || mr.Side != "maker" {
		t.Fatal("wrong sides")
	}
	// Both books show 1 lot remaining.
	for _, ob := range obs {
		if rem, ok := remainingOn(t, ob, ord.ID()); !ok || rem != lot {
			t.Fatalf("remaining %d %v", rem, ok)
		}
	}
	// Take 2 more lots: only 1 is available, the rest is the remainder.
	res, err = taker.Take(tCtx, dcrBTC, false, 2*lot, 2e5, lot, false, time.Minute)
	if err != nil || len(res.Proposed) != 1 || res.Remain != lot {
		t.Fatalf("take 2: %+v %v", res, err)
	}
	waitStatus(t, taker, res.Proposed[0], MatchAgreed)
	waitStatus(t, maker, res.Proposed[0], MatchAgreed)
	for _, ob := range obs {
		if _, ok := remainingOn(t, ob, ord.ID()); ok {
			t.Fatal("filled order still on book")
		}
	}
	if len(maker.OwnOrders()) != 0 {
		t.Fatal("filled own order kept")
	}
	// Nothing left to take.
	res, _ = taker.Take(tCtx, dcrBTC, false, lot, 2e5, lot, false, time.Minute)
	if len(res.Proposed) != 0 || res.Remain != lot {
		t.Fatalf("take from empty book: %+v", res)
	}
}

func TestNegotiatePartialAccept(t *testing.T) {
	m, obs := newNegotiators(t, 3)
	maker, t1, t2 := obs[0], obs[1], obs[2]
	ord, _ := maker.PostOrder(tCtx, dcrBTC, true, 4*lot, lot, 2e5, time.Minute)
	// t2 doesn't see the maker's fill updates, so its book is stale.
	m.setDrop(func(topic string, from peer.ID) bool {
		return from == maker.self && strings.HasPrefix(topic, OrderBookTopicPrefix)
	})
	res, _ := t1.Take(tCtx, dcrBTC, false, 3*lot, 2e5, lot, false, time.Minute)
	waitStatus(t, maker, res.Proposed[0], MatchAgreed)
	if rem, _ := remainingOn(t, t2, ord.ID()); rem != 4*lot {
		t.Fatalf("t2 book not stale: %d", rem)
	}
	res, _ = t2.Take(tCtx, dcrBTC, false, 4*lot, 2e5, lot, false, time.Minute)
	if len(res.Proposed) != 1 {
		t.Fatalf("no proposal: %+v", res)
	}
	tr := waitStatus(t, t2, res.Proposed[0], MatchAgreed)
	mr := waitStatus(t, maker, res.Proposed[0], MatchAgreed)
	if tr.Match.Qty != lot || tr.AgreementHash != mr.AgreementHash || !strings.Contains(tr.Reason, "partial") {
		t.Fatalf("partial accept: %+v / %+v", tr, mr)
	}
}

func TestNegotiateRejects(t *testing.T) {
	m, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	ord, _ := maker.PostOrder(tCtx, dcrBTC, true, 4*lot, lot, 2e5, time.Minute)
	bo, ok := func() (*BookOrder, bool) {
		taker.mtx.RLock()
		defer taker.mtx.RUnlock()
		return taker.books[dcrBTC].Order(ord.ID(), time.Now())
	}()
	if !ok {
		t.Fatal("taker doesn't see order")
	}
	for _, tc := range []struct {
		qty, rate uint64
		reason    string
	}{
		{lot / 2, 2e5, "multiple of the lot size"},
		{lot, 1e5, "rate does not match"},
		{5 * lot, 2e5, ""}, // more than available: accepted partially, not rejected
	} {
		id, _, err := taker.Propose(tCtx, bo, tc.qty, tc.rate)
		if err != nil {
			t.Fatal(err)
		}
		if tc.reason == "" {
			r := waitStatus(t, taker, id, MatchAgreed)
			if r.Match.Qty != 4*lot {
				t.Fatalf("over-sized proposal agreed at %d", r.Match.Qty)
			}
			continue
		}
		r := waitStatus(t, taker, id, MatchRejected)
		if !strings.Contains(r.Reason, tc.reason) {
			t.Fatalf("reason %q, want %q", r.Reason, tc.reason)
		}
		waitStatus(t, maker, id, MatchRejected)
	}
	// Order fully filled now; another proposal against the stale view is
	// rejected as unknown.
	id, _, _ := taker.Propose(tCtx, bo, lot, 2e5)
	if r := waitStatus(t, taker, id, MatchRejected); !strings.Contains(r.Reason, "unknown") {
		t.Fatal(r.Reason)
	}
	// Cancelled order whose cancel the taker didn't see.
	m.setDrop(func(topic string, from peer.ID) bool {
		return from == maker.self && strings.HasPrefix(topic, OrderBookTopicPrefix)
	})
	ord2, _ := maker.PostOrder(tCtx, dcrBTC, true, lot, lot, 2e5, time.Minute)
	m.setDrop(nil)
	maker.broadcastOwn(tCtx, nil) // taker learns of ord2
	m.setDrop(func(topic string, from peer.ID) bool {
		return from == maker.self && strings.HasPrefix(topic, OrderBookTopicPrefix)
	})
	if err := maker.CancelOrder(tCtx, ord2.ID()); err != nil {
		t.Fatal(err)
	}
	res, _ := taker.Take(tCtx, dcrBTC, false, lot, 2e5, lot, false, time.Minute)
	if len(res.Proposed) != 1 {
		t.Fatalf("taker didn't propose against stale order: %+v", res)
	}
	waitStatus(t, taker, res.Proposed[0], MatchRejected)
	// Can't propose against own order.
	ownBo := &BookOrder{MeshOrder: ord2, Remaining: lot}
	if _, _, err := maker.Propose(tCtx, ownBo, lot, 2e5); err == nil {
		t.Fatal("proposed against own order")
	}
}

func TestNegotiateTimeoutsAndRetries(t *testing.T) {
	m, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	ord, _ := maker.PostOrder(tCtx, dcrBTC, true, 2*lot, lot, 2e5, time.Minute)

	// 1. Maker offline (all inbox traffic to it lost): taker times out.
	m.setDrop(func(topic string, _ peer.ID) bool { return topic == InboxTopic(maker.self) })
	res, _ := taker.Take(tCtx, dcrBTC, false, lot, 2e5, lot, false, time.Minute)
	r := waitStatus(t, taker, res.Proposed[0], MatchTimedOut)
	if !strings.Contains(r.Reason, "no answer") {
		t.Fatal(r.Reason)
	}
	if _, found := maker.MatchRecord(res.Proposed[0]); found {
		t.Fatal("maker saw the proposal")
	}

	// 2. First proposal and first accept lost: retries recover.
	var lostProp, lostAcc bool
	m.setDrop(func(topic string, from peer.ID) bool {
		if topic == InboxTopic(maker.self) && !lostProp {
			lostProp = true
			return true
		}
		if topic == InboxTopic(taker.self) && !lostAcc {
			lostAcc = true
			return true
		}
		return false
	})
	res, _ = taker.Take(tCtx, dcrBTC, false, lot, 2e5, lot, false, time.Minute)
	tr := waitStatus(t, taker, res.Proposed[0], MatchAgreed)
	mr := waitStatus(t, maker, res.Proposed[0], MatchAgreed)
	if tr.AgreementHash != mr.AgreementHash {
		t.Fatal("retried agreement differs")
	}

	// Let straggling retransmissions from case 2 drain.
	time.Sleep(5 * fastCfg.RetryInterval)

	// 3. All confirms lost: the maker's reservation expires and the taker
	// is told, so both end up not agreed and the quantity is available again.
	var propSeen bool
	m.setDrop(func(topic string, from peer.ID) bool {
		if topic == InboxTopic(maker.self) && from == taker.self {
			if !propSeen {
				propSeen = true
				return false // the proposal
			}
			return true // confirms (and proposal retries)
		}
		return false
	})
	res, _ = taker.Take(tCtx, dcrBTC, false, lot, 2e5, lot, false, time.Minute)
	waitStatus(t, maker, res.Proposed[0], MatchTimedOut)
	waitStatus(t, taker, res.Proposed[0], MatchRejected)
	maker.mtx.RLock()
	oo := maker.own[ord.ID()]
	avail := oo.available()
	maker.mtx.RUnlock()
	if avail != lot {
		t.Fatalf("reservation not released: %d", avail)
	}
	m.setDrop(nil)
	// And the lot can still be taken.
	res, _ = taker.Take(tCtx, dcrBTC, false, lot, 2e5, lot, false, time.Minute)
	waitStatus(t, maker, res.Proposed[0], MatchAgreed)
}

func TestTakeStandingRemainder(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	_, _ = maker.PostOrder(tCtx, dcrBTC, true, lot, lot, 2e5, time.Minute)
	res, err := taker.Take(tCtx, dcrBTC, false, 3*lot, 2e5, lot, true, time.Minute)
	if err != nil || res.Remain != 2*lot {
		t.Fatalf("%+v %v", res, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		own := taker.OwnOrders()
		if len(own) == 1 {
			if own[0].Qty != 2*lot || own[0].Sell || own[0].Rate != 2e5 {
				t.Fatalf("bad standing order %+v", own[0].MeshOrder)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no standing remainder posted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The maker sees it as a buy order on its book.
	ords, _ := maker.Book(dcrBTC)
	if len(ords) != 1 || ords[0].Owner != taker.self.String() {
		t.Fatalf("maker book %+v", ords)
	}
}
