// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
)

func signedAtt(t *testing.T, n *simNode, epoch uint64, fp EpochHash, stamp int64) *EpochAttestation {
	t.Helper()
	a := &EpochAttestation{Epoch: epoch, Fingerprint: fp, Stamp: stamp}
	if err := a.sign(n.priv); err != nil {
		t.Fatal(err)
	}
	return a
}

// TestEquivocationProof checks the proof rules: two valid signatures of
// different fingerprints for one epoch by one key, canonical order.
func TestEquivocationProof(t *testing.T) {
	s := newEpochSim(t, 2, 0, DefaultEpochParams)
	v, w := s.vals[0], s.vals[1]
	fp1, fp2 := sha256.Sum256([]byte("one")), sha256.Sum256([]byte("two"))
	a1, a2 := signedAtt(t, v, 7, fp1, 1), signedAtt(t, v, 7, fp2, 2)
	p, err := NewEquivocationProof(a2, a1)
	if err != nil {
		t.Fatalf("valid proof: %v", err)
	}
	if p2, _ := NewEquivocationProof(a1, a2); p2.ID() != p.ID() {
		t.Fatal("proof not canonical")
	}
	b, _ := json.Marshal(p)
	var back EquivocationProof
	if err := json.Unmarshal(b, &back); err != nil || back.Verify() != nil || back.ID() != p.ID() {
		t.Fatalf("json round trip: %v %v", err, back.Verify())
	}
	if len(b) > 700 {
		t.Fatalf("proof is %d bytes", len(b))
	}
	// Same fingerprint, different stamps: not a conflict.
	if _, err := NewEquivocationProof(a1, signedAtt(t, v, 7, fp1, 3)); err == nil {
		t.Fatal("same result accepted as equivocation")
	}
	// Different epochs.
	if _, err := NewEquivocationProof(a1, signedAtt(t, v, 8, fp2, 2)); err == nil {
		t.Fatal("different epochs accepted")
	}
	bad := func(name string, f func(p *EquivocationProof)) {
		q := *p
		qa, qb := *p.A, *p.B
		q.A, q.B = &qa, &qb
		f(&q)
		if err := q.Verify(); err == nil {
			t.Fatalf("%s: forged proof accepted", name)
		} else {
			t.Logf("%s: rejected: %v", name, err)
		}
	}
	// A real signature reused for another fingerprint (the forgery an
	// attacker can make against an honest validator).
	bad("reused signature", func(q *EquivocationProof) {
		other := sha256.Sum256([]byte("zzz"))
		q.A, q.B = &SignedResult{Fingerprint: fp1, Stamp: 1, Sig: a1.Sig}, &SignedResult{Fingerprint: other, Stamp: 1, Sig: a1.Sig}
		if string(q.A.Fingerprint[:]) > string(q.B.Fingerprint[:]) {
			q.A, q.B = q.B, q.A
		}
	})
	// Signed by another key in the accused's name.
	bad("other key", func(q *EquivocationProof) {
		x := signedAtt(t, w, 7, q.B.Fingerprint, q.B.Stamp)
		q.B.Sig = x.Sig
	})
	bad("other epoch", func(q *EquivocationProof) { q.Epoch = 8 })
	bad("other validator", func(q *EquivocationProof) { q.Validator = w.id.String() })
	bad("stamp changed", func(q *EquivocationProof) { q.A.Stamp++ })
	bad("not canonical", func(q *EquivocationProof) { q.A, q.B = q.B, q.A })
	bad("same twice", func(q *EquivocationProof) { q.B = q.A })
	bad("missing", func(q *EquivocationProof) { q.B = nil })
}

// TestEpochEquivocationBan: a validator signs two results; every peer bans
// it at the same record, it leaves the set, finalization continues, the
// penalty hook sees it, and the ban survives a restart from the store.
func TestEpochEquivocationBan(t *testing.T) {
	s := newEpochSim(t, 5, 2, s3Params())
	dir := t.TempDir()
	stores := make(map[string]string)
	for _, n := range s.all() {
		stores[n.name] = filepath.Join(dir, n.name+".jsonl")
		s.startNode(n, n.name[0] == 'v', stores[n.name])
	}
	var hmtx sync.Mutex
	hooked := make(map[string]*BanInfo)
	for _, n := range s.all() {
		nn := n
		n.e.SetPenaltyHook(func(b *BanInfo) { hmtx.Lock(); hooked[nn.name] = b; hmtx.Unlock() })
	}
	s.epochs(15)
	s.randomOrders(20)
	s.epochs(3)
	bad := s.vals[4]
	before := lastEpoch(s.vals[0].e)
	bad.e.SetEquivocate(1)
	s.epochs(8)
	var banRec uint64
	for _, n := range s.all() {
		bs := n.e.BansStatus()
		if len(bs.Banned) != 1 || bs.Banned[0].Validator != bad.id.String() {
			t.Fatalf("%s: banned %+v held %+v flags %v", n.name, bs.Banned, bs.Held, n.e.Status().Flags)
		}
		if banRec == 0 {
			banRec = bs.Banned[0].BanRecord
		} else if bs.Banned[0].BanRecord != banRec {
			t.Fatalf("%s: ban record %d, others %d", n.name, bs.Banned[0].BanRecord, banRec)
		}
		n.e.mtx.Lock()
		member := n.e.valsLocked(banRec + 1).IsValidator(bad.id.String())
		n.e.mtx.Unlock()
		if member {
			t.Fatalf("%s: banned validator still in the set", n.name)
		}
	}
	t.Logf("equivocation after epoch %d, banned by record %d (%d epochs later)", before, banRec, banRec-before)
	if banRec-before > 4 {
		t.Fatalf("ban took %d epochs", banRec-before)
	}
	if st := bad.e.Status(); st.Role != "banned" || st.Signing {
		t.Fatalf("banned node status role %s signing %v", st.Role, st.Signing)
	}
	s.randomOrders(20)
	last := lastEpoch(s.vals[0].e)
	s.epochs(10)
	if got := lastEpoch(s.vals[0].e); got < last+8 {
		t.Fatalf("finalization after ban: %d -> %d", last, got)
	}
	for _, r := range s.vals[0].e.Records(banRec+1, 1000) {
		for _, a := range r.Active {
			if a == bad.id.String() {
				t.Fatalf("record %d has the banned validator active", r.Epoch)
			}
		}
		for _, a := range r.Sigs {
			if a.Validator == bad.id.String() {
				t.Fatalf("record %d carries the banned validator's signature", r.Epoch)
			}
		}
	}
	s.checkAgreement(s.all())
	s.checkActiveReplay(s.all())
	hmtx.Lock()
	if len(hooked) != len(s.all()) {
		t.Fatalf("penalty hook ran on %d of %d nodes", len(hooked), len(s.all()))
	}
	for name, b := range hooked {
		if b.Validator != bad.id.String() || b.BanRecord != banRec || !b.Seeded || b.Power != 1 {
			t.Fatalf("%s hook: %+v", name, b)
		}
	}
	hmtx.Unlock()
	// Restart every node from its store: the ban is still there.
	for _, n := range s.all() {
		n.e.store.Close()
		s.startNode(n, n.name[0] == 'v', stores[n.name])
		if bs := n.e.BansStatus(); len(bs.Banned) != 1 || bs.Banned[0].BanRecord != banRec {
			t.Fatalf("%s after restart: %+v", n.name, bs.Banned)
		}
	}
	last = lastEpoch(s.vals[0].e)
	s.epochs(10)
	if got := lastEpoch(s.vals[0].e); got < last+6 {
		t.Fatalf("finalization after restart: %d -> %d", last, got)
	}
	if lastEpoch(bad.e) < last+6 {
		t.Fatal("banned node does not follow the chain")
	}
	if g := bad.e.BansStatus().GuardEpoch; g == 0 || g > banRec {
		t.Fatalf("banned node signed after its ban (guard %d, ban record %d)", g, banRec)
	}
	s.checkAgreement(s.all())
}

// TestEpochForgedProof: a forged proof against an honest validator is
// rejected in gossip and in a proposal; nobody is banned and finalization
// continues (the next proposer takes the forging proposer's epochs).
func TestEpochForgedProof(t *testing.T) {
	s := newEpochSim(t, 4, 1, s3Params())
	s.epochs(15)
	honest, forger, client := s.vals[0], s.vals[1], s.traders[0]
	if err := client.e.SetForgeProof(honest.id.String(), "gossip"); err != nil {
		t.Fatal(err)
	}
	s.epochs(4)
	for _, n := range s.vals {
		if n.e.BansStatus().BadProofs == 0 {
			t.Fatalf("%s did not see the forged gossip", n.name)
		}
	}
	_ = client.e.SetForgeProof("", "off")
	_ = forger.e.SetForgeProof(honest.id.String(), "propose")
	last := lastEpoch(honest.e)
	s.epochs(12)
	_ = forger.e.SetForgeProof("", "off")
	got := lastEpoch(honest.e)
	t.Logf("forging proposer: finalized %d of 12 epochs", got-last)
	if got < last+11 {
		t.Fatalf("finalization with a forging proposer: %d -> %d", last, got)
	}
	var byForger int
	for _, r := range honest.e.Records(last+1, 100) {
		if r.Proposer == forger.id.String() {
			byForger++
		}
		if len(r.Proposal.Proofs) > 0 {
			t.Fatalf("record %d carries proofs", r.Epoch)
		}
	}
	if byForger > 0 {
		t.Fatalf("%d records proposed by the forger finalized", byForger)
	}
	for _, n := range s.all() {
		bs := n.e.BansStatus()
		if len(bs.Banned) != 0 || len(bs.Held) != 0 {
			t.Fatalf("%s: banned %v held %v", n.name, bs.Banned, bs.Held)
		}
	}
	var flagged int
	for _, f := range honest.e.Status().Flags {
		if f.Kind == "bad-proof" && strings.Contains(f.Detail, "equivocation proof") {
			flagged++
		}
	}
	if flagged == 0 {
		t.Fatal("no bad-proof flag")
	}
	s.checkAgreement(s.all())
}

// TestEpochBanProofRequired: a validator holding a proof (here loaded from
// its proof file, as after a chain restart) does not sign a proposal that
// leaves the proof out, and signs once a proposal carries it.
func TestEpochBanProofRequired(t *testing.T) {
	s := newEpochSim(t, 4, 0, s3Params())
	s.epochs(5)
	v, bad := s.vals[0], s.vals[3]
	fp1, fp2 := sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b"))
	p, err := NewEquivocationProof(signedAtt(t, bad, 3, fp1, 1), signedAtt(t, bad, 3, fp2, 2))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := filepath.Join(dir, "v.jsonl")
	s.startNode(v, true, store)
	v.e.mtx.Lock()
	v.e.proofs[p.Validator] = &proofEntry{proof: p}
	v.e.writeProofsLocked()
	v.e.mtx.Unlock()
	s.startNode(v, true, store) // reload: the proof comes from the file
	if h := v.e.BansStatus().Held; len(h) != 1 || h[0].Validator != bad.id.String() {
		t.Fatalf("held after reload: %+v", h)
	}
	n := lastEpoch(s.vals[1].e) + 1
	without := &EpochProposal{Epoch: n, Proposer: s.vals[1].id.String()}
	with := &EpochProposal{Epoch: n, Proposer: s.vals[1].id.String(), Proofs: []*EquivocationProof{p}}
	v.e.mtx.Lock()
	okWithout, okWith := v.e.bansOKLocked(without), v.e.bansOKLocked(with)
	v.e.mtx.Unlock()
	if okWithout || !okWith {
		t.Fatalf("bansOK without %v with %v", okWithout, okWith)
	}
	// The network bans the validator once v proposes (it carries the proof).
	s.epochs(8)
	for _, n := range s.all() {
		if bs := n.e.BansStatus(); len(bs.Banned) != 1 {
			t.Fatalf("%s: banned %+v", n.name, bs.Banned)
		}
	}
	s.checkAgreement(s.all())
}

// TestSignGuard: one result per epoch, also across restarts.
func TestSignGuard(t *testing.T) {
	s := newEpochSim(t, 1, 0, DefaultEpochParams)
	v := s.vals[0]
	store := filepath.Join(t.TempDir(), "v.jsonl")
	s.startNode(v, true, store)
	fpA, fpB := sha256.Sum256([]byte("A")), sha256.Sum256([]byte("B"))
	v.e.mtx.Lock()
	if err := v.e.guardSignLocked(100, fpA); err != nil {
		t.Fatal(err)
	}
	v.e.mtx.Unlock()
	s.startNode(v, true, store)
	v.e.mtx.Lock()
	defer v.e.mtx.Unlock()
	if err := v.e.guardSignLocked(100, fpB); err == nil {
		t.Fatal("signed a second result for epoch 100 after restart")
	}
	if err := v.e.guardSignLocked(99, fpB); err == nil {
		t.Fatal("signed an older epoch")
	}
	if err := v.e.guardSignLocked(100, fpA); err != nil {
		t.Fatalf("same result again: %v", err)
	}
	if err := v.e.guardSignLocked(101, fpB); err != nil {
		t.Fatalf("next epoch: %v", err)
	}
}

// TestDuplicateKey: messages signed with our key that we did not send hold
// signing; our own echoes and a previous run's messages do not.
func TestDuplicateKey(t *testing.T) {
	s := newEpochSim(t, 4, 0, s3Params())
	s.epochs(6)
	v := s.vals[0]
	now := v.e.nowMs()
	v.e.mtx.Lock()
	hold := v.e.signHoldLocked(now)
	v.e.mtx.Unlock()
	if hold != "" {
		t.Fatalf("hold before: %s", hold)
	}
	hb := func(stamp int64) []byte {
		h := &EpochHeartbeat{Epoch: s.rel.Params.EpochOf(stamp), Stamp: stamp}
		pid, pub, _ := ownerOf(v.priv)
		h.Validator, h.PubKey = pid.String(), pub
		h.Sig, _ = v.priv.Sign(h.msg())
		b, _ := json.Marshal(&epochMsg{Type: epMsgHB, HB: h})
		return b
	}
	v.e.mtx.Lock()
	v.e.noteSentLocked(now - 5)
	started := v.e.startedAt
	v.e.mtx.Unlock()
	v.e.ownKeyMsg(hb(now - 5))      // our own echo
	v.e.ownKeyMsg(hb(started - 10)) // previous run
	if v.e.BansStatus().DupCount != 0 {
		t.Fatal("echo or old message counted as a twin")
	}
	v.e.ownKeyMsg(hb(now - 3)) // a twin
	bs := v.e.BansStatus()
	if !bs.DupKey || !strings.Contains(bs.SignHold, "another instance") {
		t.Fatalf("twin not detected: %+v", bs)
	}
	last := lastEpoch(v.e)
	g := bs.GuardEpoch
	s.epochs(5)
	if lastEpoch(v.e) < last+3 {
		t.Fatal("network stopped")
	}
	if v.e.BansStatus().GuardEpoch != g {
		t.Fatal("signed while a twin was running")
	}
	s.epochs(DupQuietEpochs + 2)
	if v.e.BansStatus().GuardEpoch == g {
		t.Fatal("did not resume signing after the twin went quiet")
	}
}

// TestLedgerBans: bans count from the epoch after the record, by asOf, and
// remove bonded members too.
func TestLedgerBans(t *testing.T) {
	s := newEpochSim(t, 3, 0, DefaultEpochParams)
	l := newBondLedger(s.rel)
	bad := s.vals[2]
	fp1, fp2 := sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b"))
	p, _ := NewEquivocationProof(signedAtt(t, bad, 3, fp1, 1), signedAtt(t, bad, 3, fp2, 2))
	rec := &EpochRecord{Epoch: 10, Proposal: &EpochProposal{Epoch: 10, Proofs: []*EquivocationProof{p}}}
	if added := l.applyBans(rec); len(added) != 1 {
		t.Fatal("not added")
	}
	if added := l.applyBans(rec); len(added) != 0 {
		t.Fatal("added twice")
	}
	id := bad.id.String()
	for _, c := range []struct {
		n, asOf uint64
		member  bool
	}{{10, 9, true}, {10, 10, true}, {11, 9, true}, {11, 10, false}, {500, 400, false}} {
		if got := l.members(c.n, c.asOf).IsValidator(id); got != c.member {
			t.Fatalf("members(%d, %d): %v", c.n, c.asOf, got)
		}
	}
	if vs := l.members(11, 10); vs.BondedPower() != 2 || len(vs.IDs()) != 2 {
		t.Fatalf("power %d ids %d", vs.BondedPower(), len(vs.IDs()))
	}
}

// TestDuplicateKeyTwin: the same validator key started a second time (the
// mesh never delivers a peer's own messages to it) sees its key's
// signatures in peers' records and never signs; the original keeps signing
// and nobody gets an equivocation proof. Once the original stops, the twin
// signs after the quiet period.
func TestDuplicateKeyTwin(t *testing.T) {
	s := newEpochSim(t, 4, 0, s3Params())
	s.epochs(15)
	v := s.vals[1]
	twin := &simNode{name: "twin", priv: v.priv, id: v.id, key: peer.ID("twin-of-" + string(v.id))}
	s.net.mtx.Lock()
	s.net.real[twin.key] = v.id
	s.net.mtx.Unlock()
	s.startNode(twin, true, "")
	s.traders = append(s.traders, twin)
	g0 := v.e.BansStatus().GuardEpoch
	s.epochs(12)
	tb, vb := twin.e.BansStatus(), v.e.BansStatus()
	t.Logf("twin: hold %q msgs %d guard %d; original guard %d -> %d", tb.SignHold, tb.DupCount, tb.GuardEpoch, g0, vb.GuardEpoch)
	if tb.GuardEpoch != 0 || tb.DupCount == 0 || !strings.Contains(tb.SignHold, "another instance") {
		t.Fatalf("twin: %+v", tb)
	}
	if vb.GuardEpoch < g0+10 {
		t.Fatalf("original stopped signing: %d -> %d", g0, vb.GuardEpoch)
	}
	for _, n := range s.all() {
		if bs := n.e.BansStatus(); len(bs.Banned)+len(bs.Held) != 0 {
			t.Fatalf("%s: banned %v held %v", n.name, bs.Banned, bs.Held)
		}
	}
	// The original goes away: the twin takes over after the quiet period.
	s.net.setDown(v.id, true)
	s.epochs(DupQuietEpochs + 4)
	if g := twin.e.BansStatus().GuardEpoch; g == 0 {
		t.Fatal("twin never signed after the original stopped")
	}
	for _, n := range s.all() {
		if n == v {
			continue
		}
		if bs := n.e.BansStatus(); len(bs.Banned)+len(bs.Held) != 0 {
			t.Fatalf("%s after takeover: banned %v held %v", n.name, bs.Banned, bs.Held)
		}
	}
}
