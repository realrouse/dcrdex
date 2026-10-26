// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// Match negotiation over the encrypted inbox, after the legacy Tatanka
// negotiate flow, made robust for lossy pub/sub with a 3-way handshake:
//
//	taker                               maker
//	match_propose  ------------------>  validate, reserve qty (maybe partial)
//	               <------------------  match_accept (or match_reject)
//	agreed;
//	match_confirm  ------------------>  agreed; fill order, broadcast update
//
// Unanswered proposes and accepts are re-sent every RetryInterval. The taker
// gives up after ProposalTimeout (and tells the maker); the maker releases its
// reservation after ReservationTimeout without a confirm. Every handler is
// idempotent, so duplicates just re-send the current answer. If a maker
// rejects after the taker agreed (expired reservation), the taker downgrades
// to rejected. That is safe because the maker initiates the swap in P3, so the
// taker has nothing at stake until the maker's contract appears.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/encode"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	sideMaker = "maker"
	sideTaker = "taker"
)

type matchState struct {
	side    string
	status  string
	reason  string
	match   Match
	prop    *MatchProposal
	peer    peer.ID
	peerEnc *secp256k1.PublicKey
	lotSize uint64
	updated int64
	done    chan struct{}
}

func (st *matchState) final() bool {
	switch st.status {
	case MatchAgreed, MatchRejected, MatchTimedOut:
		return true
	}
	return false
}

// MatchRecord is a snapshot of a match negotiation.
type MatchRecord struct {
	Side          string `json:"side"`
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	Updated       int64  `json:"updated"`
	AgreementHash string `json:"agreementHash,omitempty"`
	Match         Match  `json:"match"`
}

func (st *matchState) record() *MatchRecord {
	r := &MatchRecord{Side: st.side, Status: st.status, Reason: st.reason, Updated: st.updated, Match: st.match}
	if st.status == MatchAgreed {
		r.AgreementHash = st.match.AgreementHash().String()
	}
	return r
}

// setStatus must be called with ob.mtx held.
func (ob *OrderBooks) setStatus(st *matchState, status, reason string) {
	wasFinal := st.final()
	st.status, st.reason, st.updated = status, reason, ob.now().UnixMilli()
	if st.final() && !wasFinal {
		close(st.done)
		if status == MatchAgreed {
			ob.startSwapLocked(st)
		}
	}
	ob.log.Infof("Match %s (%s): %s %s", st.match.ID, st.side, status, reason)
}

// Matches returns snapshots of all known matches, oldest first.
func (ob *OrderBooks) Matches() []*MatchRecord {
	ob.mtx.RLock()
	recs := make([]*MatchRecord, 0, len(ob.matches))
	for _, st := range ob.matches {
		recs = append(recs, st.record())
	}
	ob.mtx.RUnlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].Match.Stamp < recs[j].Match.Stamp })
	return recs
}

// MatchRecord returns one match snapshot.
func (ob *OrderBooks) MatchRecord(id MatchID) (*MatchRecord, bool) {
	ob.mtx.RLock()
	defer ob.mtx.RUnlock()
	st, found := ob.matches[id]
	if !found {
		return nil, false
	}
	return st.record(), true
}


func (ob *OrderBooks) receiveAddr(assetID uint32) string {
	ob.mtx.RLock()
	f := ob.addr
	ob.mtx.RUnlock()
	addr, err := f(assetID)
	if err != nil {
		ob.log.Warnf("No receiving address for asset %d (recorded empty; needed for P3 swaps): %v", assetID, err)
		return ""
	}
	return addr
}

// TakeResult is the outcome of starting a take.
type TakeResult struct {
	Proposed []MatchID `json:"proposed"`
	// Remain is the quantity that couldn't be matched against the book.
	Remain uint64 `json:"remain"`
}

// Take matches a desired trade against the book and sends proposals. If
// standing is true, whatever is not agreed once all proposals resolve is
// posted as a standing order with lotSize (zero: a single lot).
func (ob *OrderBooks) Take(ctx context.Context, m Market, sell bool, qty, rate, lotSize uint64, standing bool, lifetime time.Duration) (*TakeResult, error) {
	if qty == 0 || rate == 0 {
		return nil, errors.New("zero qty or rate")
	}
	if err := ob.SubscribeMarket(ctx, m); err != nil {
		return nil, err
	}
	book, err := ob.Book(m)
	if err != nil {
		return nil, err
	}
	cands, remain := MatchBook(&DesiredTrade{Qty: qty, Rate: rate, Sell: sell}, &ob.cfg.Fees, book, ob.self.String())
	res := &TakeResult{Remain: remain}
	dones := make([]chan struct{}, 0, len(cands))
	for _, c := range cands {
		id, done, err := ob.Propose(ctx, c.Order, c.Qty, c.Order.Rate)
		if err != nil {
			ob.log.Errorf("Error proposing match against %s: %v", c.Order.ID(), err)
			continue
		}
		res.Proposed = append(res.Proposed, id)
		dones = append(dones, done)
	}
	if standing {
		go func() {
			for _, d := range dones {
				select {
				case <-d:
				case <-ctx.Done():
					return
				}
			}
			var agreed uint64
			for _, id := range res.Proposed {
				if r, ok := ob.MatchRecord(id); ok && r.Status == MatchAgreed {
					agreed += r.Match.Qty
				}
			}
			left := qty - agreed
			lot := lotSize
			if lot == 0 {
				lot = left
			}
			if left = (left / max(lot, 1)) * lot; left == 0 {
				return
			}
			ord, err := ob.PostOrder(ctx, m, sell, left, lot, rate, lifetime)
			if err != nil {
				ob.log.Errorf("Error posting standing remainder: %v", err)
				return
			}
			ob.log.Infof("Posted standing remainder %s for %d", ord.ID(), left)
		}()
	}
	return res, nil
}

// Propose sends a match proposal against a book order. Take uses it with
// MatchBook results; it is exported for tests and the debug API, so it does
// not check lot or rate compatibility itself (the maker does).
func (ob *OrderBooks) Propose(ctx context.Context, bo *BookOrder, qty, rate uint64) (MatchID, chan struct{}, error) {
	if bo.Owner == ob.self.String() {
		return MatchID{}, nil, errors.New("can't match own order")
	}
	makerID, err := peer.Decode(bo.Owner)
	if err != nil {
		return MatchID{}, nil, err
	}
	makerEnc, err := secp256k1.ParsePubKey(bo.EncPubKey)
	if err != nil {
		return MatchID{}, nil, fmt.Errorf("bad maker encryption key: %w", err)
	}
	stamp := ob.now().UnixMilli()
	nonce := binary.BigEndian.Uint64(encode.RandomBytes(8))
	policy := ob.LockPolicy(bo.Market)
	if err := policy.Validate(); err != nil {
		return MatchID{}, nil, fmt.Errorf("lock policy for market %d-%d: %w", bo.Market.Base, bo.Market.Quote, err)
	}
	mlt, tlt := policy.lockTimes(stamp)
	p := &MatchProposal{
		MatchID:        computeMatchID(bo.ID(), ob.self.String(), nonce, stamp),
		MakerOrderID:   bo.ID(),
		Taker:          ob.self.String(),
		TakerEncPubKey: ob.encPriv.PubKey().SerializeCompressed(),
		Qty:            qty,
		Rate:           rate,
		Stamp:          stamp,
		Nonce:          nonce,
		MakerLockTime:  mlt,
		TakerLockTime:  tlt,
	}
	match := Match{
		ID: p.MatchID, Market: bo.Market, MakerOrderID: p.MakerOrderID, Maker: bo.Owner, Taker: p.Taker,
		MakerSell: bo.Sell, Qty: qty, Rate: rate, QuoteQty: calc.BaseToQuote(rate, qty), Stamp: stamp,
		Initiator: Initiator, MakerLockTime: mlt, TakerLockTime: tlt,
	}
	p.TakerAddr = ob.receiveAddr(match.TakerReceives())
	match.TakerAddr = p.TakerAddr
	st := &matchState{side: sideTaker, status: MatchProposed, match: match, prop: p, peer: makerID,
		peerEnc: makerEnc, lotSize: bo.LotSize, updated: stamp, done: make(chan struct{})}
	ob.mtx.Lock()
	ob.matches[p.MatchID] = st
	ob.mtx.Unlock()
	ob.log.Infof("Proposing match %s: %d @ %d against order %s of %s", p.MatchID, qty, rate, bo.ID(), bo.Owner)
	ob.sendInbox(ctx, makerID, makerEnc, msgMatchPropose, p)

	go func() {
		retry := time.NewTicker(ob.cfg.RetryInterval)
		defer retry.Stop()
		deadline := time.NewTimer(ob.cfg.ProposalTimeout)
		defer deadline.Stop()
		for {
			select {
			case <-st.done:
				return
			case <-retry.C:
				ob.sendInbox(ctx, makerID, makerEnc, msgMatchPropose, p)
			case <-deadline.C:
				ob.mtx.Lock()
				timedOut := st.status == MatchProposed
				if timedOut {
					ob.setStatus(st, MatchTimedOut, "no answer from maker")
				}
				ob.mtx.Unlock()
				if timedOut {
					ob.sendInbox(ctx, makerID, makerEnc, msgMatchReject, &MatchReject{MatchID: p.MatchID, Reason: "taker timed out"})
				}
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return p.MatchID, st.done, nil
}

func (ob *OrderBooks) handleInboxMsg(ctx context.Context, msg *inboxMsg) {
	from := msg.From
	switch msg.Type {
	case msgMatchPropose:
		var p MatchProposal
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			ob.log.Debugf("Bad proposal from %s: %v", from, err)
			return
		}
		p.Taker = from.String() // implied by the authenticated sender
		ob.handlePropose(ctx, from, &p)
	case msgMatchAccept:
		var a MatchAccept
		if err := json.Unmarshal(msg.Payload, &a); err != nil {
			return
		}
		ob.handleAccept(ctx, from, &a)
	case msgMatchConfirm:
		var c MatchConfirm
		if err := json.Unmarshal(msg.Payload, &c); err != nil {
			return
		}
		ob.handleConfirm(ctx, from, &c)
	case msgMatchReject:
		var r MatchReject
		if err := json.Unmarshal(msg.Payload, &r); err != nil {
			return
		}
		ob.handleReject(ctx, from, &r)
	case swapMsgInit, swapMsgPart, swapMsgRedeem, swapMsgDone:
		ob.handleSwapMsg(from, msg.Type, msg.Payload)
	default:
		ob.log.Debugf("Unknown inbox message type %q from %s", msg.Type, from)
	}
}

// checkProposal returns a rejection reason or "". ob.mtx must be held.
func (ob *OrderBooks) checkProposal(p *MatchProposal, oo *ownOrder) string {
	now := ob.now()
	switch {
	case oo == nil:
		return "unknown, filled or cancelled order"
	case oo.ord.Expiry <= now.UnixMilli():
		return "order expired"
	case p.Rate != oo.ord.Rate:
		return "rate does not match order"
	case p.Qty == 0 || p.Qty%oo.ord.LotSize != 0:
		return "qty is not a multiple of the lot size"
	case p.Stamp > now.Add(MaxClockSkew).UnixMilli() || p.Stamp < now.Add(-MaxInboxMsgAge).UnixMilli():
		return "proposal stamp out of range"
	}
	if mlt, tlt := ob.policyLocked(oo.ord.Market).lockTimes(p.Stamp); p.MakerLockTime != mlt || p.TakerLockTime != tlt {
		return "lock times do not match the market lock policy"
	}
	if oo.available() < oo.ord.LotSize {
		return "insufficient remaining quantity"
	}
	return ""
}

func (ob *OrderBooks) handlePropose(ctx context.Context, from peer.ID, p *MatchProposal) {
	if p.Taker != from.String() || computeMatchID(p.MakerOrderID, p.Taker, p.Nonce, p.Stamp) != p.MatchID {
		ob.log.Debugf("Dropping inconsistent proposal from %s", from)
		return
	}
	takerEnc, err := secp256k1.ParsePubKey(p.TakerEncPubKey)
	if err != nil {
		ob.log.Debugf("Bad taker encryption key from %s", from)
		return
	}

	// Look up our receiving address before taking the lock for the rest.
	makerAddr := ""
	ob.mtx.RLock()
	oo0 := ob.own[p.MakerOrderID]
	_, dup := ob.matches[p.MatchID]
	ob.mtx.RUnlock()
	if oo0 != nil && !dup {
		receives := oo0.ord.Base
		if oo0.ord.Sell {
			receives = oo0.ord.Quote
		}
		makerAddr = ob.receiveAddr(receives)
	}

	ob.mtx.Lock()
	if st, found := ob.matches[p.MatchID]; found {
		// Duplicate (retransmission). Re-send our current answer.
		status, match, reason := st.status, st.match, st.reason
		ob.mtx.Unlock()
		if st.side != sideMaker {
			return
		}
		switch status {
		case MatchAccepted, MatchAgreed:
			ob.sendInbox(ctx, from, takerEnc, msgMatchAccept, &MatchAccept{MatchID: p.MatchID, Qty: match.Qty, MakerAddr: match.MakerAddr})
		default:
			ob.sendInbox(ctx, from, takerEnc, msgMatchReject, &MatchReject{MatchID: p.MatchID, Reason: reason})
		}
		return
	}
	oo := ob.own[p.MakerOrderID]
	st := &matchState{side: sideMaker, prop: p, peer: from, peerEnc: takerEnc, updated: ob.now().UnixMilli(), done: make(chan struct{})}
	st.match = Match{ID: p.MatchID, MakerOrderID: p.MakerOrderID, Maker: ob.self.String(), Taker: p.Taker,
		Qty: p.Qty, Rate: p.Rate, QuoteQty: calc.BaseToQuote(p.Rate, p.Qty), Stamp: p.Stamp, Initiator: Initiator,
		TakerAddr: p.TakerAddr, MakerLockTime: p.MakerLockTime, TakerLockTime: p.TakerLockTime}
	if oo != nil {
		st.match.Market, st.match.MakerSell, st.lotSize = oo.ord.Market, oo.ord.Sell, oo.ord.LotSize
	}
	ob.matches[p.MatchID] = st
	if reason := ob.checkProposal(p, oo); reason != "" {
		ob.setStatus(st, MatchRejected, reason)
		ob.mtx.Unlock()
		ob.sendInbox(ctx, from, takerEnc, msgMatchReject, &MatchReject{MatchID: p.MatchID, Reason: reason})
		return
	}
	// Partial acceptance if the order's available quantity shrank.
	qty := p.Qty
	if avail := (oo.available() / oo.ord.LotSize) * oo.ord.LotSize; avail < qty {
		qty = avail
	}
	oo.reserved += qty
	st.match.Qty, st.match.QuoteQty = qty, calc.BaseToQuote(p.Rate, qty)
	st.match.MakerAddr = makerAddr
	ob.setStatus(st, MatchAccepted, fmt.Sprintf("reserved %d of %d proposed", qty, p.Qty))
	acc := &MatchAccept{MatchID: p.MatchID, Qty: qty, MakerAddr: makerAddr}
	ob.mtx.Unlock()

	ob.sendInbox(ctx, from, takerEnc, msgMatchAccept, acc)

	go func() {
		retry := time.NewTicker(ob.cfg.RetryInterval)
		defer retry.Stop()
		deadline := time.NewTimer(ob.cfg.ReservationTimeout)
		defer deadline.Stop()
		for {
			select {
			case <-st.done:
				return
			case <-retry.C:
				ob.sendInbox(ctx, from, takerEnc, msgMatchAccept, acc)
			case <-deadline.C:
				ob.mtx.Lock()
				expired := st.status == MatchAccepted
				if expired {
					ob.releaseLocked(st)
					ob.setStatus(st, MatchTimedOut, "no confirm from taker")
				}
				ob.mtx.Unlock()
				if expired {
					// Tell the taker, in case only its confirm was lost.
					ob.sendInbox(ctx, from, takerEnc, msgMatchReject, &MatchReject{MatchID: p.MatchID, Reason: "reservation expired"})
				}
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// releaseLocked releases a maker reservation. ob.mtx must be held.
func (ob *OrderBooks) releaseLocked(st *matchState) {
	if oo := ob.own[st.match.MakerOrderID]; oo != nil {
		oo.reserved -= st.match.Qty
	}
}

func (ob *OrderBooks) handleAccept(ctx context.Context, from peer.ID, a *MatchAccept) {
	ob.mtx.Lock()
	st, found := ob.matches[a.MatchID]
	if !found || st.side != sideTaker || st.peer != from {
		ob.mtx.Unlock()
		return
	}
	switch st.status {
	case MatchProposed:
		if a.Qty == 0 || a.Qty > st.prop.Qty || a.Qty%st.lotSize != 0 {
			ob.setStatus(st, MatchRejected, "bad accepted qty")
			ob.mtx.Unlock()
			ob.sendInbox(ctx, from, st.peerEnc, msgMatchReject, &MatchReject{MatchID: a.MatchID, Reason: "bad accepted qty"})
			return
		}
		st.match.Qty, st.match.QuoteQty, st.match.MakerAddr = a.Qty, calc.BaseToQuote(st.match.Rate, a.Qty), a.MakerAddr
		reason := ""
		if a.Qty < st.prop.Qty {
			reason = fmt.Sprintf("partial: %d of %d proposed", a.Qty, st.prop.Qty)
		}
		ob.setStatus(st, MatchAgreed, reason)
		fallthrough
	case MatchAgreed:
		qty := st.match.Qty
		ob.mtx.Unlock()
		ob.sendInbox(ctx, from, st.peerEnc, msgMatchConfirm, &MatchConfirm{MatchID: a.MatchID, Qty: qty})
	default: // rejected / timed out: tell the maker to release.
		reason := st.reason
		ob.mtx.Unlock()
		ob.sendInbox(ctx, from, st.peerEnc, msgMatchReject, &MatchReject{MatchID: a.MatchID, Reason: "taker aborted: " + reason})
	}
}

func (ob *OrderBooks) handleConfirm(ctx context.Context, from peer.ID, c *MatchConfirm) {
	ob.mtx.Lock()
	st, found := ob.matches[c.MatchID]
	if !found || st.side != sideMaker || st.peer != from {
		ob.mtx.Unlock()
		return
	}
	switch st.status {
	case MatchAccepted:
		if c.Qty != st.match.Qty {
			ob.releaseLocked(st)
			ob.setStatus(st, MatchRejected, "confirm qty mismatch")
			ob.mtx.Unlock()
			ob.sendInbox(ctx, from, st.peerEnc, msgMatchReject, &MatchReject{MatchID: c.MatchID, Reason: "confirm qty mismatch"})
			return
		}
		ob.setStatus(st, MatchAgreed, st.reason)
		orderID, qty := st.match.MakerOrderID, st.match.Qty
		ob.mtx.Unlock()
		ob.fillOwn(ctx, orderID, qty, true)
	case MatchAgreed:
		ob.mtx.Unlock()
	default:
		reason := st.reason
		ob.mtx.Unlock()
		ob.sendInbox(ctx, from, st.peerEnc, msgMatchReject, &MatchReject{MatchID: c.MatchID, Reason: reason})
	}
}

func (ob *OrderBooks) handleReject(_ context.Context, from peer.ID, r *MatchReject) {
	ob.mtx.Lock()
	defer ob.mtx.Unlock()
	st, found := ob.matches[r.MatchID]
	if !found || st.peer != from {
		return
	}
	switch {
	case st.side == sideTaker && (st.status == MatchProposed || st.status == MatchAgreed):
		if st.status == MatchAgreed {
			ob.abortSwapLocked(st.match.ID, r.Reason)
		}
		ob.setStatus(st, MatchRejected, "maker: "+r.Reason)
	case st.side == sideMaker && st.status == MatchAccepted:
		ob.releaseLocked(st)
		ob.setStatus(st, MatchRejected, "taker: "+r.Reason)
	}
}
