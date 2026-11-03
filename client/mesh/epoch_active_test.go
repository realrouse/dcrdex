// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"strings"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
)

// s3Params are short S3 windows so the simulations stay small: 40-epoch
// window ending 5 epochs back, 80 %, at least 10 counted records.
func s3Params() EpochParams {
	p := DefaultEpochParams
	p.ActiveWindowEpochs, p.ActiveLagEpochs, p.ActivePct, p.ActiveMinEpochs = 40, 5, 80, 10
	return p
}

// TestActiveSetCompute checks the window, the lag, the 80 % rule, the
// uncounted last record and the fallback on a synthetic chain.
func TestActiveSetCompute(t *testing.T) {
	var ids []peer.ID
	for i := 0; i < 4; i++ {
		ids = append(ids, pid(t, newPriv(t)))
	}
	p := DefaultEpochParams
	p.ActiveWindowEpochs, p.ActiveLagEpochs, p.ActivePct, p.ActiveMinEpochs = 10, 2, 80, 5
	rel := makeTestRelease(t, ids, p)
	v := rel.ValidatorIDs()
	// Records for epochs 100..129; credits of record k are in record k+1.
	// v[3] signs nothing from 110 on, v[2] misses every 4th epoch.
	var chain []*EpochRecord
	for e := uint64(100); e < 130; e++ {
		r := &EpochRecord{Epoch: e, Proposal: &EpochProposal{Epoch: e}}
		if len(chain) > 0 {
			prev := e - 1
			for i, id := range v {
				if (i == 3 && prev >= 110) || (i == 2 && prev%4 == 0) {
					continue
				}
				r.Proposal.Credits = append(r.Proposal.Credits, &EpochCredit{Validator: id})
			}
		}
		chain = append(chain, r)
	}
	// Epoch 112: window 101..110, all ten counted; v[3] 9/10, v[2] 8/10
	// (exactly 80 % is enough).
	as := computeActiveSet(rel, chain, 112)
	if as.WindowFrom != 101 || as.WindowTo != 110 || as.Sample != 10 || as.Fallback {
		t.Fatalf("window %+v", as)
	}
	if as.Credits[v[3]] != 9 || as.Credits[v[2]] != 8 || len(as.Active) != 4 {
		t.Fatalf("epoch 112: credits %v active %v", as.Credits, as.Active)
	}
	// Epoch 115: window 104..113, v[3] 6/10 and v[2] 7/10 -> out.
	as = computeActiveSet(rel, chain, 115)
	if !equalStrings(as.Active, []string{v[0], v[1]}) {
		t.Fatalf("epoch 115: credits %v active %v", as.Credits, as.Active)
	}
	// The last record (129) has no credits yet: for epoch 131 the window is
	// 120..129 but only 120..128 count.
	as = computeActiveSet(rel, chain, 131)
	if as.Sample != 9 {
		t.Fatalf("sample %d", as.Sample)
	}
	// Too few records in the window (new chain or halt): everyone.
	for _, n := range []uint64{103, 145} {
		as = computeActiveSet(rel, chain, n)
		if !as.Fallback || len(as.Active) != 4 {
			t.Fatalf("epoch %d: no fallback: %+v", n, as)
		}
	}
	// The percentage is a release parameter: at 70 % v[2] (7/10) stays.
	p2 := p
	p2.ActivePct = 70
	rel2 := makeTestRelease(t, ids, p2)
	if as := computeActiveSet(rel2, chain, 115); !equalStrings(as.Active, []string{v[0], v[1], v[2]}) {
		t.Fatalf("70%%: %v", as.Credits)
	}
}

func activeNames(s *epochSim, n *simNode) []string {
	var out []string
	for _, v := range n.e.Status().Validators {
		if v.Active {
			out = append(out, v.Name)
		}
	}
	return out
}

func isActive(n *simNode, id peer.ID) bool {
	for _, v := range n.e.Status().Validators {
		if v.PeerID == id.String() {
			return v.Active
		}
	}
	return false
}

func (s *epochSim) checkActiveReplay(nodes []*simNode) {
	s.t.Helper()
	for _, n := range nodes {
		checked, bad := n.e.ActiveCheck(0)
		if len(bad) > 0 || checked == 0 {
			s.t.Fatalf("%s: active set replay: %d checked, mismatches %v", n.name, checked, bad)
		}
	}
}

// TestEpochActiveSetDropRejoin: an offline validator drops out of the
// active set, the quorum is then 2/3 of the smaller set, and the validator
// rejoins after signing again for long enough. All peers agree on the set.
func TestEpochActiveSetDropRejoin(t *testing.T) {
	s := newEpochSim(t, 4, 1, s3Params())
	s.epochs(60)
	v0 := s.vals[0]
	st := v0.e.Status()
	if st.ActiveSet.Fallback || st.ActivePower != 4 {
		t.Fatalf("warm-up: %+v power %d", st.ActiveSet, st.ActivePower)
	}
	off := s.vals[3]
	s.net.setDown(off.id, true)
	dropped := 0
	for i := 1; i <= 60 && dropped == 0; i++ {
		s.randomOrders(1)
		s.epochs(1)
		if !isActive(v0, off.id) {
			dropped = i
		}
	}
	if dropped == 0 {
		t.Fatal("offline validator never dropped out")
	}
	up := []*simNode{s.vals[0], s.vals[1], s.vals[2], s.traders[0]}
	s.epochs(2)
	s.checkAgreement(up)
	for _, n := range up {
		if got := activeNames(s, n); len(got) != 3 {
			t.Fatalf("%s active %v", n.name, got)
		}
	}
	last := v0.e.LastRecord()
	if len(last.Active) != 3 {
		t.Fatalf("record active %d", len(last.Active))
	}
	t.Logf("v3 offline: dropped out after %d epochs; active %v on every peer", dropped, activeNames(s, v0))

	// Quorum over the active set: 2 of the 3 active (2 of 4 bonded) finalize.
	s.net.setDown(s.vals[2].id, true)
	f0 := lastEpoch(v0.e)
	s.epochs(6)
	f1 := lastEpoch(v0.e)
	if f1 < f0+5 {
		t.Fatalf("2 of 3 active did not finalize: %d -> %d", f0, f1)
	}
	for _, r := range v0.e.Records(f0, 100) {
		if len(r.Sigs) != 2 || len(r.Active) != 3 {
			t.Fatalf("epoch %d: %d sigs, %d active", r.Epoch, len(r.Sigs), len(r.Active))
		}
	}
	t.Logf("2 of 3 active validators finalized %d epochs (quorum counted over the active set)", f1-f0)
	s.net.setDown(s.vals[2].id, false)
	s.epochs(2)

	// v3 comes back: it syncs, signs (credited, not counted) and rejoins.
	s.net.setDown(off.id, false)
	back := lastEpoch(v0.e)
	rejoined := 0
	for i := 1; i <= 80 && rejoined == 0; i++ {
		s.randomOrders(1)
		s.epochs(1)
		if isActive(v0, off.id) {
			rejoined = i
		}
	}
	if rejoined == 0 {
		t.Fatalf("v3 never rejoined; credit %+v", v0.e.Status().Validators[3])
	}
	credited := 0
	for _, r := range v0.e.Records(back, 1000) {
		for _, c := range r.Proposal.Credits {
			if c.Validator == off.id.String() {
				credited++
			}
		}
	}
	if credited == 0 {
		t.Fatal("v3 signatures never credited")
	}
	s.epochs(2)
	s.checkAgreement(s.all())
	s.checkActiveReplay(s.all())
	for _, n := range s.all() {
		if got := activeNames(s, n); len(got) != 4 {
			t.Fatalf("%s active %v after rejoin", n.name, got)
		}
	}
	t.Logf("v3 back: credited in %d records, rejoined after %d epochs; all peers agree", credited, rejoined)
}

// TestEpochActiveSetFloor: validators drop out one by one until the active
// set holds less than half of the bonded power; then nothing finalizes even
// though every active validator signs. After the window drains every
// validator counts again (fallback) and the network resumes once enough
// come back.
func TestEpochActiveSetFloor(t *testing.T) {
	s := newEpochSim(t, 5, 1, s3Params())
	s.epochs(60)
	v0 := s.vals[0]
	dropOut := func(n *simNode) {
		t.Helper()
		s.net.setDown(n.id, true)
		for i := 0; i < 80; i++ {
			s.epochs(1)
			if !isActive(v0, n.id) {
				return
			}
		}
		t.Fatalf("%s did not drop out", n.name)
	}
	dropOut(s.vals[4])
	dropOut(s.vals[3])
	t.Logf("after 2 dropouts: active %v, power %d/%d", activeNames(s, v0), v0.e.Status().ActivePower, v0.e.Status().BondedPower)
	// v2 down: v0+v1 are 2/3 of the 3 active and keep finalizing until v2
	// drops out; then active power 2 < 5/2: the floor stops finalization.
	dropOut(s.vals[2])
	st := v0.e.Status()
	if st.ActivePower != 2 || st.CanFinalize {
		t.Fatalf("floor: active %d canFinalize %v", st.ActivePower, st.CanFinalize)
	}
	s.epochs(2)
	stalled := lastEpoch(v0.e)
	s.epochs(10)
	if got := lastEpoch(v0.e); got != stalled {
		t.Fatalf("finalized below the floor: %d -> %d", stalled, got)
	}
	t.Logf("active power 2 of 5: floor stops finalization at epoch %d", stalled)
	// The window drains: fallback to all 5 (quorum 4), still stalled.
	s.epochs(50)
	st = v0.e.Status()
	if !st.ActiveSet.Fallback || st.ActivePower != 5 || lastEpoch(v0.e) != stalled {
		t.Fatalf("drain: %+v power %d last %d", st.ActiveSet, st.ActivePower, lastEpoch(v0.e))
	}
	s.net.setDown(s.vals[2].id, false)
	s.net.setDown(s.vals[3].id, false)
	s.epochs(5)
	if got := lastEpoch(v0.e); got < stalled+3 {
		t.Fatalf("not resumed: %d", got)
	}
	s.net.setDown(s.vals[4].id, false)
	s.epochs(3)
	s.checkAgreement(s.all())
	s.checkActiveReplay(s.all())
	t.Logf("window drained -> all 5 active (fallback); 4 of 5 back -> resumed at %d", lastEpoch(v0.e))
}

// TestEpochActiveSetForged: a proposal with an active list that is not the
// computed set, or with a forged credit, is never signed and is flagged.
func TestEpochActiveSetForged(t *testing.T) {
	s := newEpochSim(t, 4, 1, s3Params())
	s.epochs(20)
	v0, v1, v2 := s.vals[0], s.vals[1], s.vals[2]
	n := lastEpoch(v0.e) + 1
	v0.e.mtx.Lock()
	prev, last := v0.e.lastFP, v0.e.last
	credits := v0.e.creditsLocked()
	v0.e.mtx.Unlock()
	mk := func(active []string, cr []*EpochCredit) *EpochProposal {
		p := &EpochProposal{Epoch: n + 1000, Prev: prev, Active: active, Credits: cr, Stamp: 1}
		p.Rank = ProposerRankIn(active, p.Epoch, v1.id.String())
		if err := p.sign(v1.priv); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ids := s.rel.ValidatorIDs()
	var small []string
	for _, id := range ids {
		if id != s.vals[3].id.String() {
			small = append(small, id)
		}
	}
	// Feed directly to v2 (outside the signing window: only the checks run).
	v2.e.handleProposal(s.ctx, mk(small, credits))
	bad := credits[0]
	forged := &EpochCredit{Validator: bad.Validator, Stamp: bad.Stamp + 1, Sig: bad.Sig}
	cr2 := append([]*EpochCredit{forged}, credits[1:]...)
	v2.e.handleProposal(s.ctx, mk(ids, cr2))
	var kinds []string
	for _, f := range v2.e.Status().Flags {
		if f.Kind == "active-set" {
			kinds = append(kinds, f.Detail)
		}
	}
	if len(kinds) != 2 || !strings.Contains(kinds[0], "not the epoch's active set") || !strings.Contains(kinds[1], "bad credit") {
		t.Fatalf("flags %v", kinds)
	}
	_ = last
	t.Logf("rejected: %v", kinds)
	s.epochs(3)
	s.checkAgreement(s.all())
}
