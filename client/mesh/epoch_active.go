// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S3: the active validator set. A validator is active for epoch N if it
// signed at least ActivePct % of the finalized epochs in a window of
// ActiveWindowEpochs epochs ending ActiveLagEpochs before N (PLAN: 80 %,
// ~24 h ending 100 epochs back; ~600 epochs on simnet). Every peer computes
// the set from the finalized chain alone, so all peers get the same set:
//
//   - The signers of record k are its credits: the attestations of k that
//     the proposal of the next finalized record carries (EpochProposal.
//     Credits). They are inside that record's fingerprint, so once it is
//     final they are the same for everyone, including late signatures that
//     arrived after k finalized. The last record's credits are not known
//     yet, so it is not counted.
//   - With fewer than ActiveMinEpochs counted records in the window (a new
//     chain, or after a long halt) every release validator is active. That
//     is the strictest quorum (2/3 of everyone), so the fallback never makes
//     finalization easier, and it lets a halted network recover.
//
// The quorum (2/3) and the floor (active power >= 1/2 of bonded power) are
// counted over this set (thresholdMet). A validator that is not active
// still signs: its signatures don't count for the quorum, but they are
// credited, so after it has signed again for long enough it rejoins.

import (
	"errors"
	"fmt"
	"sort"

	"github.com/libp2p/go-libp2p/core/peer"
)

// ActiveSet is the active validator set of an epoch and how it was derived.
type ActiveSet struct {
	Epoch uint64 `json:"epoch"`
	// AsOf is the last record of the chain the set was computed from.
	AsOf uint64 `json:"asOf"`
	// WindowFrom..WindowTo (inclusive) is the epoch window.
	WindowFrom uint64 `json:"windowFrom"`
	WindowTo   uint64 `json:"windowTo"`
	// Sample is the number of finalized records counted in the window.
	Sample int `json:"sample"`
	// Fallback: too few records in the window, all validators active.
	Fallback bool `json:"fallback"`
	// Active is the sorted active set.
	Active []string `json:"active"`
	// Credits is each validator's credited signatures in the window.
	Credits map[string]int `json:"credits"`
	Power   uint64         `json:"power"`
	// S4: Samples is the counted records of bonded members that joined
	// inside the window (since they joined); Bonded is the epoch's bonded
	// power (seeded + bonded members).
	Samples map[string]int `json:"samples,omitempty"`
	Bonded  uint64         `json:"bonded"`
}

// computeActiveSet derives the active set for epoch n from chain, the
// finalized records of the current chain in epoch order (last = newest).
//
// S4: the candidates are the epoch's validator set vs (seeded + bonded
// members). A bonded member is judged only on the records since it became a
// member: it is active once it has at least ActiveMinEpochs of them in the
// window and signed ActivePct % of those. Seeded validators are members from
// the start, so for them this is the S3 rule unchanged.
func computeActiveSet(vs *valSet, chain []*EpochRecord, n uint64) *ActiveSet {
	p := &vs.rel.Params
	as := &ActiveSet{Epoch: n, Credits: make(map[string]int)}
	if len(chain) > 0 {
		as.AsOf = chain[len(chain)-1].Epoch
	}
	hi := int64(n) - p.ActiveLagEpochs
	lo := hi - p.ActiveWindowEpochs + 1
	if lo < 0 {
		lo = 0
	}
	if hi >= 0 {
		as.WindowFrom, as.WindowTo = uint64(lo), uint64(hi)
		// The last record's credits are in a future proposal: not counted.
		counted := len(chain) - 1
		i := sort.Search(counted, func(i int) bool { return int64(chain[i].Epoch) >= lo })
		var newer []string // bonded members that joined inside the window
		for _, v := range vs.ids {
			if vs.since(v) > uint64(lo) {
				newer = append(newer, v)
			}
		}
		as.Samples = make(map[string]int, len(newer))
		for ; i < counted && int64(chain[i].Epoch) <= hi; i++ {
			as.Sample++
			for _, v := range newer {
				if chain[i].Epoch >= vs.since(v) {
					as.Samples[v]++
				}
			}
			for _, c := range chain[i+1].Proposal.Credits {
				as.Credits[c.Validator]++
			}
		}
	}
	if int64(as.Sample) < p.ActiveMinEpochs {
		as.Fallback = true
		as.Active = vs.IDs()
	} else {
		for _, v := range vs.ids {
			sample, isNew := as.Samples[v]
			if !isNew {
				sample = as.Sample
			} else if int64(sample) < p.ActiveMinEpochs {
				continue // a new bonded member is judged once it has enough records
			}
			if credits := min(as.Credits[v], sample); int64(credits)*100 >= p.ActivePct*int64(sample) {
				as.Active = append(as.Active, v)
			}
		}
	}
	sort.Strings(as.Active)
	for _, v := range as.Active {
		as.Power += vs.Power(v)
	}
	as.Bonded = vs.BondedPower()
	return as
}

// chainLocked is the current chain's records (after any chain restart).
func (e *EpochEngine) chainLocked() []*EpochRecord {
	if e.chainStart > len(e.records) {
		e.chainStart = len(e.records)
	}
	return e.records[e.chainStart:]
}

// activeForLocked is the active set for epoch n on top of our chain.
func (e *EpochEngine) activeForLocked(n uint64) *ActiveSet {
	if c := e.activeCache; c != nil && c.Epoch == n && e.activeCacheFP == e.lastFP {
		return c
	}
	as := computeActiveSet(e.valsLocked(n), e.chainLocked(), n)
	e.activeCache, e.activeCacheFP = as, e.lastFP
	return as
}

// ActiveFor is the active set for epoch n computed from our chain up to and
// including the record of epoch asOf (0: the whole chain).
func (e *EpochEngine) ActiveFor(n, asOf uint64) (*ActiveSet, error) {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	chain := e.chainLocked()
	if asOf > 0 {
		i := sort.Search(len(chain), func(i int) bool { return chain[i].Epoch > asOf })
		if i == 0 || chain[i-1].Epoch != asOf {
			return nil, fmt.Errorf("no record for epoch %d in our chain", asOf)
		}
		chain = chain[:i]
	}
	asof := uint64(0)
	if len(chain) > 0 {
		asof = chain[len(chain)-1].Epoch
	}
	return computeActiveSet(e.ledger.members(n, asof), chain, n), nil
}

// ActiveCheck recomputes the active set of every record of our chain with
// epoch > from from the records before it and compares it with the
// record's Active list (which every peer already checked on acceptance).
func (e *EpochEngine) ActiveCheck(from uint64) (checked int, mismatches []uint64) {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	chain := e.chainLocked()
	for i, r := range chain {
		if r.Epoch <= from {
			continue
		}
		checked++
		asof := uint64(0)
		if i > 0 {
			asof = chain[i-1].Epoch
		}
		if as := computeActiveSet(e.ledger.members(r.Epoch, asof), chain[:i], r.Epoch); !equalStrings(as.Active, r.Active) {
			mismatches = append(mismatches, r.Epoch)
		}
	}
	return
}

// checkActiveSetLocked checks a proposal building on our last record: its
// Active list must be the computed active set and its credits must be valid
// signatures of our last record.
func (e *EpochEngine) checkActiveSetLocked(p *EpochProposal) error {
	if e.last == nil {
		if len(p.Credits) > 0 {
			return errors.New("credits on the first record of a chain")
		}
	} else {
		prevSet := e.valsLocked(e.last.Epoch)
		for _, c := range p.Credits {
			if !prevSet.IsValidator(c.Validator) {
				return fmt.Errorf("credit for %s, not a validator in epoch %d", e.rel.ValidatorName(c.Validator), e.last.Epoch)
			}
			if err := c.verify(e.last.Epoch, e.lastFP); err != nil {
				return fmt.Errorf("bad credit from %s: %v", e.rel.ValidatorName(c.Validator), err)
			}
		}
	}
	as := e.activeForLocked(p.Epoch)
	if !equalStrings(as.Active, p.Active) {
		return fmt.Errorf("active list %v is not the epoch's active set %v", e.names(p.Active), e.names(as.Active))
	}
	return nil
}

func (e *EpochEngine) names(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = e.rel.ValidatorName(id)
	}
	return out
}

// creditsLocked are the known signatures of our last record, sorted.
func (e *EpochEngine) creditsLocked() []*EpochCredit {
	if e.last == nil {
		return nil
	}
	out := make([]*EpochCredit, 0, len(e.lastCredits))
	for _, c := range e.lastCredits {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Validator < out[j].Validator })
	return out
}

// addCreditLocked records a valid attestation of our last record.
func (e *EpochEngine) addCreditLocked(a *EpochAttestation) {
	if e.last == nil || a.Epoch != e.last.Epoch || a.Fingerprint != e.lastFP || !e.valsLocked(e.last.Epoch).IsValidator(a.Validator) {
		return
	}
	if _, have := e.lastCredits[a.Validator]; have {
		return
	}
	if a.verify() != nil {
		return
	}
	e.lastCredits[a.Validator] = &EpochCredit{Validator: a.Validator, Stamp: a.Stamp, Sig: a.Sig}
}

// verifyByID verifies sig with the public key inside the peer ID.
func verifyByID(id string, msg, sig []byte) error {
	pid, err := peer.Decode(id)
	if err != nil {
		return err
	}
	pub, err := pid.ExtractPublicKey()
	if err != nil {
		return err
	}
	ok, err := pub.Verify(msg, sig)
	if err != nil || !ok {
		return errors.New("bad signature")
	}
	return nil
}
