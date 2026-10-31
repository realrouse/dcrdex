// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mc "github.com/bisoncraft/mesh/client"
)

type eventLog struct {
	mtx sync.Mutex
	evs []*Event
}

func (l *eventLog) sink(e *Event) {
	l.mtx.Lock()
	l.evs = append(l.evs, e)
	l.mtx.Unlock()
}

func (l *eventLog) count(kind string) int {
	l.mtx.Lock()
	defer l.mtx.Unlock()
	n := 0
	for _, e := range l.evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestPresenceSignValidate(t *testing.T) {
	priv := newPriv(t)
	now := time.Now()
	node := pid(t, newPriv(t)).String()
	p := &Presence{Proto: ProtocolVersion, MinProto: MinProtocolVersion, App: strings.Repeat("v", maxPresenceApp),
		Name: strings.Repeat("n", MaxPeerNameLen), Node: node, Stamp: now.UnixMilli(), Policies: map[string][2]int64{}}
	for i := 0; i < maxPresencePolicies; i++ {
		p.Policies[strings.Repeat(string(rune('a'+i)), 3)+"_"+"xyz"] = [2]int64{172800, 86400}
	}
	if err := p.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(now); err != nil {
		t.Fatalf("valid presence rejected: %v", err)
	}
	// Biggest presence must fit the mesh's 1 KiB message cap with room to spare.
	b, _ := json.Marshal(p)
	if n := len(b) + len(PresenceTopic); n > 1024-200 {
		t.Fatalf("max presence is %d bytes", n)
	}
	// Tampering breaks the signature.
	q := *p
	q.Name = "mallory"
	if err := q.Validate(now); err == nil {
		t.Fatal("tampered name accepted")
	}
	q = *p
	q.Policies = map[string][2]int64{"dcr_btc": {1, 1}}
	if err := q.Validate(now); err == nil {
		t.Fatal("tampered policies accepted")
	}
	// Stale or future presence.
	if err := p.Validate(now.Add(PresenceTimeout + time.Second)); err == nil {
		t.Fatal("stale presence accepted")
	}
	if err := p.Validate(now.Add(-MaxClockSkew - time.Second)); err == nil {
		t.Fatal("future presence accepted")
	}
	// Over-long name.
	r := &Presence{Proto: 1, MinProto: 1, Name: strings.Repeat("x", MaxPeerNameLen+1), Stamp: now.UnixMilli()}
	_ = r.Sign(priv)
	if err := r.Validate(now); err == nil {
		t.Fatal("long name accepted")
	}
	// Bad node ID.
	r = &Presence{Proto: 1, MinProto: 1, Node: "not-a-peer", Stamp: now.UnixMilli()}
	_ = r.Sign(priv)
	if err := r.Validate(now); err == nil {
		t.Fatal("bad node accepted")
	}
}

func TestProtocolCompatible(t *testing.T) {
	for _, tc := range []struct {
		proto, min uint32
		ok         bool
	}{
		{ProtocolVersion, MinProtocolVersion, true},
		{ProtocolVersion + 1, MinProtocolVersion, true},         // newer peer that still speaks our version
		{ProtocolVersion + 1, ProtocolVersion + 1, false},       // newer peer that needs newer
		{MinProtocolVersion - 1, MinProtocolVersion - 1, false}, // too old
	} {
		ok, reason := ProtocolCompatible(tc.proto, tc.min)
		if ok != tc.ok {
			t.Fatalf("proto %d min %d: got %v (%s), want %v", tc.proto, tc.min, ok, reason, tc.ok)
		}
		if !ok && reason == "" {
			t.Fatal("no reason")
		}
	}
}

func TestSanitizeName(t *testing.T) {
	if got := SanitizeName("  <b>Bob\x00</b> "); got != "bBob/b" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("ø", 40) // 2 bytes each
	got := SanitizeName(long)
	if len(got) > MaxPeerNameLen || !strings.HasPrefix(long, got) {
		t.Fatalf("bad truncation %q", got)
	}
}

func TestParseBootstrapList(t *testing.T) {
	id := pid(t, newPriv(t)).String()
	a1 := "/ip4/127.0.0.1/tcp/15701/p2p/" + id
	a2 := "/dns4/mesh.example.org/tcp/12345/p2p/" + id
	got, err := ParseBootstrapList(" " + a1 + " ,," + a2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != a1 || got[1] != a2 {
		t.Fatalf("got %v", got)
	}
	if _, err := ParseBootstrapList("/ip4/127.0.0.1/tcp/1"); err == nil {
		t.Fatal("address without /p2p accepted")
	}
	if _, err := ParseBootstrapList("garbage"); err == nil {
		t.Fatal("garbage accepted")
	}
	if got, _ := ParseBootstrapList(""); len(got) != 0 {
		t.Fatal("empty list not empty")
	}
}

func TestPresenceExchange(t *testing.T) {
	m := newFakeMesh()
	a, b := newTestBooks(t, m, nil), newTestBooks(t, m, nil)
	var evA, evB eventLog
	a.SetEventSink(evA.sink)
	b.SetEventSink(evB.sink)
	nodeID := pid(t, newPriv(t)).String()
	a.SetIdentity("ThinkBox <A>", "1.2.3", func() string { return nodeID })
	b.SetIdentity("B", "1.2.3", nil)
	for _, ob := range []*OrderBooks{a, b} {
		if err := ob.SubscribePresence(tCtx); err != nil {
			t.Fatal(err)
		}
		if err := ob.SubscribeMarket(tCtx, dcrBTC); err != nil {
			t.Fatal(err)
		}
	}
	a.AnnouncePresence(tCtx)
	pi, ok := b.Peer(a.self.String())
	if !ok {
		t.Fatal("b doesn't know a")
	}
	if pi.Name != "ThinkBox A" || pi.Proto != ProtocolVersion || !pi.Compatible || !pi.Online || pi.Node != nodeID || pi.App != "1.2.3" {
		t.Fatalf("bad peer info %+v", pi)
	}
	if pol, ok := pi.Policies["dcr_btc"]; !ok || pol[0] != int64(DefaultLockPolicies["dcr_btc"].Maker/time.Second) {
		t.Fatalf("bad policies %v", pi.Policies)
	}
	if evB.count(EventPeer) == 0 {
		t.Fatal("no peer event")
	}
	// A presence relayed under another sender is dropped.
	p, _ := a.buildPresence()
	raw, _ := json.Marshal(p)
	c := newTestBooks(t, m, nil)
	b.handlePresenceEvent(tCtx, mc.TopicEvent{Type: mc.TopicEventData, Data: raw, Peer: c.self})
	if _, ok := b.Peer(c.self.String()); ok {
		t.Fatal("spoofed presence accepted")
	}
	// Orders posted by a show up with a's name, and events fire on both.
	if _, err := a.PostOrder(tCtx, dcrBTC, true, 2*lot, lot, 2e5, time.Minute); err != nil {
		t.Fatal(err)
	}
	bv := b.BookView(dcrBTC)
	if len(bv.Orders) != 1 || bv.Orders[0].OwnerName != "ThinkBox A" || bv.Orders[0].Own || bv.Orders[0].Warning != "" {
		t.Fatalf("bad book view %+v", bv.Orders)
	}
	if evA.count(EventOrder) != 1 || evB.count(EventOrder) != 1 {
		t.Fatalf("order events: a %d b %d", evA.count(EventOrder), evB.count(EventOrder))
	}
	if av := a.BookView(dcrBTC); len(av.Orders) != 1 || !av.Orders[0].Own {
		t.Fatal("own order not marked")
	}
}

func TestIncompatiblePeerOrdersHidden(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	// No presence subscriptions: presence messages are fed in by hand.
	ord, err := maker.PostOrder(tCtx, dcrBTC, true, 2*lot, lot, 2e5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Without presence the peer is unknown but its orders are shown.
	if bv := taker.BookView(dcrBTC); len(bv.Orders) != 1 || len(bv.Hidden) != 0 {
		t.Fatalf("unknown-version order hidden: %+v", bv)
	}
	if pi, _ := taker.Peer(maker.self.String()); pi == nil || pi.Proto != 0 || !pi.Compatible {
		t.Fatalf("bad unknown peer %+v", pi)
	}
	// The maker announces a protocol we can't speak.
	p := &Presence{Proto: ProtocolVersion + 5, MinProto: ProtocolVersion + 5, Name: "future", Stamp: time.Now().UnixMilli()}
	if err := p.Sign(maker.priv); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	taker.handlePresenceEvent(tCtx, mc.TopicEvent{Type: mc.TopicEventData, Data: raw, Peer: maker.self})
	bv := taker.BookView(dcrBTC)
	if len(bv.Orders) != 0 || len(bv.Hidden) != 1 || !strings.Contains(bv.Hidden[0].Hidden, "incompatible") {
		t.Fatalf("incompatible order not hidden with a reason: %+v", bv)
	}
	if ords, _ := taker.Book(dcrBTC); len(ords) != 0 {
		t.Fatal("Book returned an incompatible order")
	}
	if _, err := taker.TakeOrder(tCtx, dcrBTC, ord.ID(), lot); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("take of incompatible order: %v", err)
	}
	if res, _ := taker.Take(tCtx, dcrBTC, false, lot, 2e5, 0, false, time.Minute); len(res.Proposed) != 0 {
		t.Fatal("Take matched an incompatible order")
	}
	// Back to a compatible version: shown again.
	p = &Presence{Proto: ProtocolVersion, MinProto: MinProtocolVersion, Name: "now", Stamp: time.Now().UnixMilli() + 1}
	_ = p.Sign(maker.priv)
	raw, _ = json.Marshal(p)
	taker.handlePresenceEvent(tCtx, mc.TopicEvent{Type: mc.TopicEventData, Data: raw, Peer: maker.self})
	if bv := taker.BookView(dcrBTC); len(bv.Orders) != 1 {
		t.Fatal("compatible order still hidden")
	}
}

func TestPolicyWarningAndTakeOrder(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	for _, ob := range obs {
		if err := ob.SubscribePresence(tCtx); err != nil {
			t.Fatal(err)
		}
	}
	if err := maker.SetLockPolicy(dcrBTC, LockPolicy{Maker: 20 * time.Minute, Taker: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	maker.AnnouncePresence(tCtx)
	ord, err := maker.PostOrder(tCtx, dcrBTC, true, 3*lot, lot, 2e5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	bv := taker.BookView(dcrBTC)
	if len(bv.Orders) != 1 || !strings.Contains(bv.Orders[0].Warning, "lock policy differs") {
		t.Fatalf("no policy warning: %+v", bv.Orders)
	}
	// Same policy: no warning, and a click-take goes through.
	if err := taker.SetLockPolicy(dcrBTC, LockPolicy{Maker: 20 * time.Minute, Taker: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if bv := taker.BookView(dcrBTC); bv.Orders[0].Warning != "" {
		t.Fatalf("unexpected warning %q", bv.Orders[0].Warning)
	}
	if _, err := taker.TakeOrder(tCtx, dcrBTC, ord.ID(), lot/2); err == nil {
		t.Fatal("partial lot accepted")
	}
	if _, err := taker.TakeOrder(tCtx, dcrBTC, ord.ID(), 4*lot); err == nil {
		t.Fatal("more than remaining accepted")
	}
	if _, err := maker.TakeOrder(tCtx, dcrBTC, ord.ID(), lot); err == nil {
		t.Fatal("own order take accepted")
	}
	var evT eventLog
	taker.SetEventSink(evT.sink)
	mid, err := taker.TakeOrder(tCtx, dcrBTC, ord.ID(), 2*lot)
	if err != nil {
		t.Fatal(err)
	}
	r := waitStatus(t, taker, mid, MatchAgreed)
	if r.Match.Qty != 2*lot {
		t.Fatalf("qty %d", r.Match.Qty)
	}
	if evT.count(EventMatch) == 0 {
		t.Fatal("no match events")
	}
	if pi, ok := maker.Peer(taker.self.String()); !ok || !contains(pi.Seen, "inbox") {
		t.Fatalf("maker didn't record the taker from its inbox: %+v", pi)
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func TestPeerPrune(t *testing.T) {
	// The clock is read from presence goroutines, so keep it atomic.
	var clk atomic.Int64
	clk.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clk.Load()) }
	m := newFakeMesh()
	a, b := newTestBooks(t, m, nil), newTestBooks(t, m, nil)
	a.now, b.now = now, now
	advance := func(d time.Duration) { clk.Add(int64(d)) }
	var ev eventLog
	b.SetEventSink(ev.sink)
	// Only b listens, so no join-triggered announcements race the clock.
	_ = b.SubscribePresence(tCtx)
	a.AnnouncePresence(tCtx)
	if pi, ok := b.Peer(a.self.String()); !ok || !pi.Online {
		t.Fatal("not online")
	}
	advance(PresenceTimeout + time.Second)
	b.prunePeers()
	if pi, ok := b.Peer(a.self.String()); !ok || pi.Online {
		t.Fatal("not marked offline")
	}
	if ev.count(EventPeerLeft) != 1 {
		t.Fatal("no peer_left event")
	}
	advance(peerForget)
	b.prunePeers()
	if _, ok := b.Peer(a.self.String()); ok {
		t.Fatal("not forgotten")
	}
	// Unsubscribing from presence marks a peer offline at once.
	a.AnnouncePresence(tCtx)
	b.handlePresenceEvent(tCtx, mc.TopicEvent{Type: mc.TopicEventPeerUnsubscribed, Peer: a.self})
	if pi, _ := b.Peer(a.self.String()); pi == nil || pi.Online {
		t.Fatal("unsubscribe didn't mark offline")
	}
}

func TestFakeProtocol(t *testing.T) {
	m := newFakeMesh()
	a, b := newTestBooks(t, m, nil), newTestBooks(t, m, nil)
	_ = b.SubscribePresence(tCtx)
	a.SetFakeProtocol(tCtx, 9, 9)
	if pi, _ := b.Peer(a.self.String()); pi == nil || pi.Proto != 9 || pi.Compatible {
		t.Fatalf("fake protocol not seen as incompatible: %+v", pi)
	}
	a.SetFakeProtocol(tCtx, 0, 0)
	if pi, _ := b.Peer(a.self.String()); pi == nil || pi.Proto != ProtocolVersion || !pi.Compatible {
		t.Fatalf("real protocol not restored: %+v", pi)
	}
}
