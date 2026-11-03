// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S1: the deterministic epoch state machine. Every node applies the same
// finalized proposals in the same order to the same state and gets the same
// matches and state hash. Nothing here reads a clock, a map in iteration
// order, or anything else that differs between nodes.
//
// Rules for epoch N with end time E (unix ms):
//  1. Resting orders with Expiry <= E are removed.
//  2. Cancels (sorted by order ID) remove the owner's resting order.
//  3. New orders (sorted by stamp, then ID) that are valid at E, were never
//     in the state before and have Stamp < E + MaxClockSkewMs are matched in
//     turn against the opposite side of their market (same lot size only,
//     never against the same owner) at the resting (maker) rate, best price
//     first, then oldest stamp, then lowest ID. Quantities are whole lots.
//     What is left rests.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"time"
)

// EpochMatch is a match produced by an epoch. The maker is the resting
// order's owner, the taker the incoming (aggressor) order's owner.
type EpochMatch struct {
	Market       Market  `json:"market"`
	MakerOrderID OrderID `json:"makerOrderID"`
	TakerOrderID OrderID `json:"takerOrderID"`
	Maker        string  `json:"maker"`
	Taker        string  `json:"taker"`
	MakerSell    bool    `json:"makerSell"`
	Qty          uint64  `json:"qty"`
	Rate         uint64  `json:"rate"`
}

func (m *EpochMatch) write(b *bytes.Buffer) {
	_ = binary.Write(b, binary.BigEndian, m.Market.Base)
	_ = binary.Write(b, binary.BigEndian, m.Market.Quote)
	b.Write(m.MakerOrderID[:])
	b.Write(m.TakerOrderID[:])
	writeStr(b, m.Maker)
	writeStr(b, m.Taker)
	if m.MakerSell {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	_ = binary.Write(b, binary.BigEndian, m.Qty)
	_ = binary.Write(b, binary.BigEndian, m.Rate)
}

type restingOrder struct {
	ord       *MeshOrder
	id        OrderID
	remaining uint64
}

// epochState is the deterministic book: resting orders, and every order ID
// ever admitted (until its expiry) so an order can't be added twice.
type epochState struct {
	resting map[OrderID]*restingOrder
	seen    map[OrderID]int64 // order ID -> expiry
}

func newEpochState() *epochState {
	return &epochState{resting: make(map[OrderID]*restingOrder), seen: make(map[OrderID]int64)}
}

func (s *epochState) clone() *epochState {
	c := &epochState{resting: make(map[OrderID]*restingOrder, len(s.resting)), seen: make(map[OrderID]int64, len(s.seen))}
	for id, r := range s.resting {
		rc := *r
		c.resting[id] = &rc
	}
	for id, e := range s.seen {
		c.seen[id] = e
	}
	return c
}

func (s *epochState) sortedResting() []*restingOrder {
	out := make([]*restingOrder, 0, len(s.resting))
	for _, r := range s.resting {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].id[:], out[j].id[:]) < 0 })
	return out
}

// hash commits to the resting orders and their remaining quantities.
func (s *epochState) hash() [32]byte {
	b := new(bytes.Buffer)
	b.WriteString("tatanka-epoch-state-v1")
	for _, r := range s.sortedResting() {
		b.Write(r.id[:])
		_ = binary.Write(b, binary.BigEndian, r.remaining)
	}
	return sha256.Sum256(b.Bytes())
}

// better is true if a is matched before b for an aggressor of side sell
// (sell aggressor: highest buy first; buy aggressor: lowest sell first).
func better(a, b *restingOrder, aggressorSell bool) bool {
	if a.ord.Rate != b.ord.Rate {
		if aggressorSell {
			return a.ord.Rate > b.ord.Rate
		}
		return a.ord.Rate < b.ord.Rate
	}
	if a.ord.Stamp != b.ord.Stamp {
		return a.ord.Stamp < b.ord.Stamp
	}
	return bytes.Compare(a.id[:], b.id[:]) < 0
}

func sortOrdersForEpoch(ords []*MeshOrder) []*MeshOrder {
	type oi struct {
		o  *MeshOrder
		id OrderID
	}
	l := make([]oi, len(ords))
	for i, o := range ords {
		l[i] = oi{o, o.ID()}
	}
	sort.Slice(l, func(i, j int) bool {
		if l[i].o.Stamp != l[j].o.Stamp {
			return l[i].o.Stamp < l[j].o.Stamp
		}
		return bytes.Compare(l[i].id[:], l[j].id[:]) < 0
	})
	out := make([]*MeshOrder, len(l))
	for i := range l {
		out[i] = l[i].o
	}
	return out
}

// applyEpoch applies a proposal's orders and cancels to s (mutated) and
// returns the matches. Invalid entries are skipped deterministically.
//
// S6: with a trading gate g (nil: no trading bonds), resting orders of
// owners without a counted trading bond are removed after expiry (step 1b),
// and new orders the gate refuses (no bond, too large for the tier, too many
// resting orders) are ignored: not matched, not rested and not marked seen.
func applyEpoch(s *epochState, prop *EpochProposal, params *EpochParams, gs ...*userGate) []*EpochMatch {
	var g *userGate
	if len(gs) > 0 {
		g = gs[0]
	}
	end := params.EpochEnd(prop.Epoch)
	endT := time.UnixMilli(end)
	// 1. Expiry. Seen IDs are kept until their expiry passes.
	for id, r := range s.resting {
		if r.ord.Expiry <= end {
			delete(s.resting, id)
		}
	}
	for id, exp := range s.seen {
		if exp <= end {
			delete(s.seen, id)
		}
	}
	// 1b. S6: orders of owners whose trading bond no longer counts.
	restingBy := make(map[string]uint64)
	if g != nil {
		for id, r := range s.resting {
			if g.tier(r.ord.Owner) == 0 {
				delete(s.resting, id)
				continue
			}
			restingBy[r.ord.Owner]++
		}
	}
	// 2. Cancels.
	cancels := append([]*MeshCancel(nil), prop.Cancels...)
	sort.Slice(cancels, func(i, j int) bool {
		return bytes.Compare(cancels[i].OrderID[:], cancels[j].OrderID[:]) < 0
	})
	for _, c := range cancels {
		r := s.resting[c.OrderID]
		if r == nil || r.ord.Owner != c.Owner || c.Stamp > end+params.MaxClockSkewMs || c.Validate(endT) != nil {
			continue
		}
		delete(s.resting, c.OrderID)
		if g != nil {
			restingBy[c.Owner]--
		}
	}
	// 3. New orders.
	var matches []*EpochMatch
	for _, o := range sortOrdersForEpoch(prop.Orders) {
		id := o.ID()
		if _, seen := s.seen[id]; seen {
			continue
		}
		if o.Stamp >= end+params.MaxClockSkewMs || o.Validate(endT) != nil {
			continue
		}
		if g.orderReason(o, restingBy[o.Owner]) != "" {
			continue
		}
		s.seen[id] = o.Expiry
		remain := o.Qty
		for remain > 0 {
			var best *restingOrder
			for _, r := range s.resting {
				if r.ord.Market != o.Market || r.ord.Sell == o.Sell || r.ord.LotSize != o.LotSize || r.ord.Owner == o.Owner {
					continue
				}
				if o.Sell && r.ord.Rate < o.Rate || !o.Sell && r.ord.Rate > o.Rate {
					continue
				}
				if best == nil || better(r, best, o.Sell) {
					best = r
				}
			}
			if best == nil {
				break
			}
			q := min(remain, best.remaining)
			matches = append(matches, &EpochMatch{
				Market: o.Market, MakerOrderID: best.id, TakerOrderID: id, Maker: best.ord.Owner, Taker: o.Owner,
				MakerSell: best.ord.Sell, Qty: q, Rate: best.ord.Rate,
			})
			remain -= q
			if best.remaining -= q; best.remaining == 0 {
				delete(s.resting, best.id)
				if g != nil {
					restingBy[best.ord.Owner]--
				}
			}
		}
		if remain > 0 {
			s.resting[id] = &restingOrder{ord: o, id: id, remaining: remain}
			if g != nil {
				restingBy[o.Owner]++
			}
		}
	}
	return matches
}
