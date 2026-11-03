// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import "context"

// S8 re-gossip. Under message loss a proposal or a signature that misses a
// validator costs the epoch (it rolls over) and, for signatures, the
// signer's credit (S3: too few credits drop it from the active set, which
// makes the next epochs harder to finalize). A validator therefore re-sends
// its own epoch messages once:
//
//   - its proposal of epoch n, half a fallback interval after it was due
//     (before the next-ranked proposer would step in), and its signature of
//     n halfway through the signing window, if n is not finalized on its
//     chain yet;
//   - its signature of epoch n right after n finalized on its chain (if not
//     re-sent already), so the next proposer can credit it.
//
// A proposal that skips an epoch j we signed (its Prev is our tip, j is
// after it: j did not finalize for us) makes us re-send our signature of j
// once, and we wait half a fallback interval before signing it. If j had a
// quorum somewhere (an observer may have collected it), the signers' re-sent
// signatures let every validator finalize j, and the skipping proposal is
// then on a stale tip; otherwise the hold only delays the signature.
//
// Re-sent messages are identical (same signature and stamp); re-sent
// signatures are marked Resent, so receivers do not take their stamp as a
// clock sample. Nothing
// new is signed, so a re-send cannot equivocate.

type ownEpochMsgs struct {
	prop       *EpochProposal
	att        *EpochAttestation
	propResent bool
	attResent  bool
}

func (e *EpochEngine) ownLocked(n uint64) *ownEpochMsgs {
	o := e.own[n]
	if o == nil {
		o = &ownEpochMsgs{}
		e.own[n] = o
	}
	return o
}

func (e *EpochEngine) noteOwnProp(p *EpochProposal) {
	e.mtx.Lock()
	e.ownLocked(p.Epoch).prop = p
	e.mtx.Unlock()
}

func (e *EpochEngine) noteOwnAtt(a *EpochAttestation) {
	e.mtx.Lock()
	e.ownLocked(a.Epoch).att = a
	e.mtx.Unlock()
}

// regossip re-sends own proposals and signatures (see above). Called from
// tick.
func (e *EpochEngine) regossip(ctx context.Context, cur uint64, nowMs int64) {
	if e.cfg.NoRegossip {
		return
	}
	var out []*epochMsg
	e.mtx.Lock()
	last, haveLast := e.lastEpochLocked()
	for n := range e.holdRetry {
		if n+10 < cur {
			delete(e.holdRetry, n)
		}
	}
	for n := range e.skipSent {
		if n+10 < cur {
			delete(e.skipSent, n)
		}
	}
	for n, o := range e.own {
		if n+10 < cur {
			delete(e.own, n)
			continue
		}
		final := haveLast && n <= last
		end := e.p.EpochEnd(n)
		// The proposal again before the next-ranked proposer would step in
		// (a validator that missed it would propose its own and split the
		// vote). Byte-identical to the first send, so fragments of the two
		// sends combine at the receiver.
		if !final && o.prop != nil && !o.propResent && nowMs >= end+e.p.ProposeDelayMs+int64(o.prop.Rank)*e.p.FallbackMs+e.p.FallbackMs/2 &&
			nowMs <= end+e.p.SignWindowMs {
			o.propResent = true
			out = append(out, &epochMsg{Type: epMsgProp, Prop: o.prop})
		}
		if !final && nowMs >= end+e.p.SignWindowMs/2 && nowMs <= end+e.p.SignWindowMs {
			if o.att != nil && !o.attResent {
				o.attResent = true
				out = append(out, &epochMsg{Type: epMsgAtt, Att: o.att, Resent: true})
			}
		}
		if final && n == last && o.att != nil && !o.attResent && o.att.Fingerprint == e.lastFP {
			o.attResent = true
			out = append(out, &epochMsg{Type: epMsgAtt, Att: o.att, Resent: true})
		}
	}
	e.resent += len(out)
	e.mtx.Unlock()
	for _, m := range out {
		e.send(ctx, m)
	}
}

// skippedSignedLocked: an epoch j < n we signed that is after our last
// record (so not finalized for us), if any.
func (e *EpochEngine) skippedSignedLocked(n uint64) (uint64, bool) {
	last, ok := e.lastEpochLocked()
	for j := range e.signed {
		if j < n && (!ok || j > last) {
			return j, true
		}
	}
	return 0, false
}

// skipResendLocked returns our signature of an epoch the proposal p skips,
// to re-send once (see above).
func (e *EpochEngine) skipResendLocked(p *EpochProposal) *EpochAttestation {
	if !e.signer || e.cfg.NoRegossip || p.Prev != e.lastFP {
		return nil
	}
	j, ok := e.skippedSignedLocked(p.Epoch)
	if !ok || e.skipSent[j] {
		return nil
	}
	o := e.own[j]
	if o == nil || o.att == nil {
		return nil
	}
	e.skipSent[j] = true
	e.resent++
	return o.att
}
