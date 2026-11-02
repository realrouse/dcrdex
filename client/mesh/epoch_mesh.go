// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S2: trading in epoch mode. Orders go to the epoch pool as well as the
// market's book topic (for display). Matches come only from finalized
// epochs: when an epoch finalizes, the taker (owner of the incoming order)
// proposes each of its matches to the maker with the record attached, using
// a match ID derived from the epoch and the match index. The maker checks
// the record's validator signatures (VerifyMatchProposal) before reserving
// anything; a proposal without a finalized record, or with a record signed
// by too few validators, is rejected.

import (
	"context"
	"fmt"
	"time"
)

// EnableEpochs turns on epoch mode with the release rel. It must be called
// before any orders are posted. The engine runs until ctx ends.
func (ob *OrderBooks) EnableEpochs(ctx context.Context, rel *Release, cfg EpochConfig) (*EpochEngine, error) {
	e, err := NewEpochEngine(rel, cfg, ob.ps, ob.priv, func() time.Time { return ob.now() }, ob.log.SubLogger("EPOCH"))
	if err != nil {
		return nil, err
	}
	ob.attachEpochs(ctx, e)
	go func() {
		if err := e.Run(ctx); err != nil {
			ob.log.Errorf("Epoch engine stopped: %v", err)
		}
	}()
	return e, nil
}

func (ob *OrderBooks) attachEpochs(ctx context.Context, e *EpochEngine) {
	e.SetFinalHandler(func(f *EpochFinal) { ob.onEpochFinal(ctx, f) })
	ob.mtx.Lock()
	ob.epochs = e
	ob.mtx.Unlock()
}

// Epochs is the epoch engine, nil in legacy mode.
func (ob *OrderBooks) Epochs() *EpochEngine { return ob.epochs }

// SetNowOffset shifts this client's clock (simnet clock-skew tests).
func (ob *OrderBooks) SetNowOffset(d time.Duration) {
	ob.now = func() time.Time { return time.Now().Add(d) }
}

// epochTakeLifetime is the lifetime of a non-standing epoch-mode take: a few
// epochs to be included and matched.
func (ob *OrderBooks) epochTakeLifetime() time.Duration {
	d := 4 * ob.epochs.rel.EpochDuration()
	if d < 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// takeEpoch is Take in epoch mode: a crossing order is posted and the
// matching happens in the epoch it is finalized in.
func (ob *OrderBooks) takeEpoch(ctx context.Context, m Market, sell bool, qty, rate, lotSize uint64, standing bool, lifetime time.Duration) (*TakeResult, error) {
	if !standing || lifetime <= 0 {
		lifetime = ob.epochTakeLifetime()
	}
	if lotSize == 0 {
		lotSize = qty
	}
	ord, err := ob.PostOrder(ctx, m, sell, qty, lotSize, rate, lifetime)
	if err != nil {
		return nil, err
	}
	ob.log.Infof("Epoch mode take: posted order %s (%d @ %d); matches come from the next finalized epoch", ord.ID(), qty, rate)
	return &TakeResult{Remain: qty}, nil
}

func (ob *OrderBooks) onEpochFinal(ctx context.Context, f *EpochFinal) {
	rec := f.Record
	end := ob.epochs.p.EpochEnd(rec.Epoch)
	self := ob.self.String()
	for i, m := range rec.Matches {
		if m.Taker != self {
			if m.Maker == self {
				ob.log.Infof("Epoch %d match %d: our order %s matched %d @ %d by %s; waiting for its proposal", rec.Epoch, i, m.MakerOrderID, m.Qty, m.Rate, m.Taker)
			}
			continue
		}
		// Old records (catch-up) are past the proposal window.
		if ob.now().UnixMilli()-end > (MaxInboxMsgAge - time.Minute).Milliseconds() {
			continue
		}
		mo := f.Makers[m.MakerOrderID]
		if mo == nil {
			ob.log.Errorf("Epoch %d match %d: maker order %s unknown", rec.Epoch, i, m.MakerOrderID)
			continue
		}
		ob.fillOwn(ctx, m.TakerOrderID, m.Qty, false)
		ref := &EpochRef{Epoch: rec.Epoch, Fingerprint: rec.Fingerprint, Index: i}
		bo := &BookOrder{MeshOrder: mo, Remaining: m.Qty}
		id, _, err := ob.proposeWith(ctx, bo, m.Qty, m.Rate, end, epochMatchNonce(rec.Epoch, i), ref)
		if err != nil {
			ob.log.Errorf("Error proposing epoch %d match %d: %v", rec.Epoch, i, err)
			continue
		}
		ob.log.Infof("Epoch %d match %d: proposing %s (%d @ %d) to maker %s", rec.Epoch, i, id, m.Qty, m.Rate, m.Maker)
	}
}

func stripProposal(r *EpochRecord) *EpochRecord {
	c := *r
	c.Proposal = nil
	return &c
}

// VerifyMatchProposal returns a rejection reason for a proposal that does
// not come from a finalized epoch, or "".
func (e *EpochEngine) VerifyMatchProposal(p *MatchProposal) string {
	ref := p.Epoch
	if ref == nil {
		return "epoch mode: match is not from a finalized epoch"
	}
	rec := e.Record(ref.Epoch)
	if rec == nil {
		return fmt.Sprintf("epoch %d is not finalized in our chain", ref.Epoch)
	}
	if rec.Fingerprint != ref.Fingerprint {
		return fmt.Sprintf("epoch %d reference is not the finalized record (not finalized)", ref.Epoch)
	}
	if ref.Index < 0 || ref.Index >= len(rec.Matches) {
		return "epoch match index out of range"
	}
	m := rec.Matches[ref.Index]
	switch {
	case m.MakerOrderID != p.MakerOrderID:
		return "epoch match is for a different maker order"
	case m.Taker != p.Taker:
		return "epoch match is for a different taker"
	case m.Qty != p.Qty || m.Rate != p.Rate:
		return "proposal qty or rate differs from the epoch match"
	case p.Stamp != e.p.EpochEnd(rec.Epoch) || p.Nonce != epochMatchNonce(rec.Epoch, ref.Index):
		return "proposal stamp or nonce is not the epoch match's"
	}
	return ""
}

// AwaitRecord waits up to timeout for epoch n to be finalized locally (the
// taker may see the record a moment before we do).
func (e *EpochEngine) AwaitRecord(ctx context.Context, n uint64, timeout time.Duration) bool {
	end := time.Now().Add(timeout)
	synced := false
	for {
		if e.Record(n) != nil {
			return true
		}
		if le, ok := e.lastEpoch(); ok && le >= n {
			return false // skipped
		}
		if time.Now().After(end) {
			return false
		}
		if !synced && time.Until(end) < timeout/2 {
			synced = true
			go e.requestSync(ctx, true)
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return false
		}
	}
}

// ForgeEpochMatch is an ADVERSARY action for simnet tests: it proposes a
// match against bo backed by a fake epoch record signed only by us. An
// honest maker must reject it.
func (ob *OrderBooks) ForgeEpochMatch(ctx context.Context, bo *BookOrder, qty uint64) (MatchID, chan struct{}, error) {
	e := ob.epochs
	if e == nil {
		return MatchID{}, nil, fmt.Errorf("not in epoch mode")
	}
	n := e.p.EpochOf(ob.now().UnixMilli()) - 1
	ids := e.rel.ValidatorIDs()
	proposer := ids[0]
	if e.rel.IsValidator(ob.self.String()) {
		proposer = ob.self.String()
	}
	e.mtx.Lock()
	prev := e.lastFP
	e.mtx.Unlock()
	rec := &EpochRecord{Epoch: n, Prev: prev, Proposer: proposer, Active: ids,
		Matches: []*EpochMatch{{Market: bo.Market, MakerOrderID: bo.ID(), Maker: bo.Owner, Taker: ob.self.String(),
			MakerSell: bo.Sell, Qty: qty, Rate: bo.Rate}}}
	copy(rec.ProposalHash[:], []byte("forged proposal ................"))
	rec.Fingerprint = epochFingerprint(rec.Epoch, rec.Prev, rec.ProposalHash, rec.Active, rec.Matches, rec.StateHash)
	a := &EpochAttestation{Epoch: n, Fingerprint: rec.Fingerprint, Stamp: ob.now().UnixMilli()}
	if err := a.sign(ob.priv); err != nil {
		return MatchID{}, nil, err
	}
	rec.Sigs = []*EpochAttestation{a}
	ob.log.Warnf("ADVERSARY: proposing forged epoch %d match against %s signed only by us", n, bo.ID())
	return ob.proposeWith(ctx, bo, qty, bo.Rate, e.p.EpochEnd(n), epochMatchNonce(n, 0), &EpochRef{Epoch: n, Fingerprint: rec.Fingerprint})
}
