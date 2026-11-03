// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"testing"
	"time"
)

func bookSuspects(n *simNode) map[string]*BookPeerStatus {
	out := make(map[string]*BookPeerStatus)
	for _, p := range n.e.BookCheck().Peers {
		if p.Suspect {
			out[p.Name] = p
		}
	}
	return out
}

// TestBookCrossCheckHidden: a validator that hides an owner's resting
// orders from its book digests (and proposals) becomes a suspect on every
// other peer; honest validators never do, and the orders still trade.
func TestBookCrossCheckHidden(t *testing.T) {
	s := newEpochSim(t, 4, 3, DefaultEpochParams)
	s.epochs(2)
	for i := 0; i < 10; i++ {
		s.randomOrders(3)
		s.epochs(1)
	}
	s.epochs(4 * bookDigestEvery)
	for _, n := range s.all() {
		if sus := bookSuspects(n); len(sus) > 0 {
			t.Fatalf("%s: honest validators suspected: %v", n.name, sus)
		}
		for _, p := range n.e.BookCheck().Peers {
			if p.Compared == 0 || p.BadDigests > 0 {
				t.Fatalf("%s: %+v", n.name, p)
			}
		}
	}
	bad := s.vals[1]
	owner := s.traders[0]
	bad.e.SetHideOwner(owner.id.String())
	var oids []OrderID
	for i := 0; i < 5; i++ { // resting asks far from the bids
		oids = append(oids, s.order(owner, true, 1, uint64(500_000+i*1_000)).ID())
		s.epochs(1)
	}
	s.epochs(4 * bookDigestEvery)
	for _, n := range s.all() {
		if n == bad {
			continue
		}
		sus := bookSuspects(n)
		if len(sus) != 1 || sus["v1"] == nil || sus["v1"].HiddenNow < 5 {
			for _, p := range sus {
				t.Logf("%+v", p)
			}
			t.Fatalf("%s: suspects %+v", n.name, sus)
		}
	}
	for _, id := range oids {
		if st := s.vals[0].e.OrderStatus(id); st != "resting" {
			t.Fatalf("hidden order %s: %s", id, st)
		}
	}
	bad.e.SetHideOwner("")
	s.epochs(2 * bookDigestEvery)
	for _, n := range s.all() {
		if sus := bookSuspects(n); len(sus) > 0 {
			t.Fatalf("%s: still suspected after the fix: %v", n.name, sus)
		}
	}
}

// TestBookCrossCheckWithheld: with finalization stalled, pending orders sit
// in the pools. A validator that withholds them is reported after a repair
// failed; an honest validator that lost an order gets it again (repair) and
// is never a suspect.
func TestBookCrossCheckWithheld(t *testing.T) {
	s := newEpochSim(t, 4, 2, DefaultEpochParams)
	s.epochs(3)
	s.net.setDown(s.vals[2].netKey(), true)
	s.net.setDown(s.vals[3].netKey(), true)
	s.epochs(2)
	bad, honest := s.vals[1], s.vals[0]
	owner := s.traders[0]
	bad.e.SetHideOwner(owner.id.String())
	for i := 0; i < 3; i++ {
		s.order(owner, true, 1, uint64(500_000+i*1_000))
	}
	// An order honest never received: admitted directly on the others.
	lost := func() *MeshOrder {
		now := s.traders[1].e.now()
		o := &MeshOrder{Market: dcrBTC, Sell: false, Qty: 1e8, LotSize: 1e8, Rate: 50_000, Stamp: now.UnixMilli(),
			Expiry: now.Add(10 * time.Minute).UnixMilli(), Nonce: 77, EncPubKey: tEncPub}
		if err := o.Sign(s.traders[1].priv); err != nil {
			t.Fatal(err)
		}
		return o
	}()
	for _, n := range []*simNode{bad, s.traders[0], s.traders[1]} {
		if err := n.e.admitOrder(lost); err != nil {
			t.Fatal(err)
		}
	}
	if st := honest.e.OrderStatus(lost.ID()); st != "unknown" {
		t.Fatalf("lost order on honest: %s", st)
	}
	s.epochs(6 * bookDigestEvery)
	if st := honest.e.OrderStatus(lost.ID()); st != "pool" {
		for _, n := range s.all() {
			t.Logf("%s: %+v", n.name, n.e.BookCheck())
			for _, p := range n.e.BookCheck().Peers {
				t.Logf("  %+v", p)
			}
		}
		t.Fatalf("lost order not repaired on honest: %s", st)
	}
	for _, n := range []*simNode{honest, s.traders[0], s.traders[1]} {
		sus := bookSuspects(n)
		if len(sus) != 1 || sus["v1"] == nil || sus["v1"].WithheldNow != 3 {
			t.Fatalf("%s: suspects %+v", n.name, sus)
		}
		for _, p := range n.e.BookCheck().Peers {
			if p.Name == "v0" && p.BadDigests > 0 {
				t.Fatalf("%s: honest v0 had bad digests: %+v", n.name, p)
			}
		}
	}
}

// TestReplayedStampsIgnored (S8 fake relay): replayed heartbeats with old
// stamps must not move the clock estimate past the limit.
func TestReplayedStampsIgnored(t *testing.T) {
	s := newEpochSim(t, 4, 1, DefaultEpochParams)
	s.epochs(5)
	v := s.vals[0]
	if _, _, ok := v.e.ClockSkew(); !ok {
		t.Fatal("clock not ok before")
	}
	now := v.e.nowMs()
	for i := 0; i < 50; i++ {
		for _, o := range s.vals[1:] {
			v.e.heardFrom(o.id.String(), now-20_000-int64(i)) // a relay replaying 20 s old messages
			v.e.heardFrom(o.id.String(), now-90_000)
		}
	}
	if skew, _, ok := v.e.ClockSkew(); !ok {
		t.Fatalf("replays moved the clock estimate to %d ms", skew)
	}
	if v.e.Status().StaleStamps < 100 {
		t.Fatalf("stale stamps %d", v.e.Status().StaleStamps)
	}
	s.epochs(2)
	if !v.e.Status().Signing {
		t.Fatal("not signing")
	}
}
