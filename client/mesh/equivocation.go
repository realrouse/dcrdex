// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S5: equivocation proofs and bans.
//
// A validator equivocates when it signs two different results (epoch
// fingerprints) for the same epoch. Two such signatures are a proof anyone
// can check with the validator's public key (inside its peer ID), so a proof
// is gossiped and every peer verifies it itself. A verified proof rides in a
// proposal like S4 signals and bonds; once a record carries it, the
// validator is banned from the next epoch on: it leaves the validator set
// (so also the active set, the quorum and the bonded power) on every peer,
// because every peer derives the set from the same finalized chain.
//
// What is NOT here: no bond is slashed, forfeited or burned. Whether
// validator bonds are slashable or a one-time payment is an open decision,
// so a ban only removes the validator. A penalty can be added later behind
// SetPenaltyHook, which is called with every new ban (and the banned
// validator's bond, if it was a bonded member).
//
// Honest validators must never produce a proof against themselves:
//   - the engine signs at most one fingerprint per epoch (signed map), and a
//     sign guard file keeps the highest signed epoch and fingerprint across
//     restarts, so a restart inside a signing window cannot sign twice;
//   - a started validator listens for StartListenEpochs before it signs; if
//     it hears messages signed with its own key that it did not send (the
//     same key started twice), it does not sign until that has been quiet
//     for DupQuietEpochs. The other instance sees the same and stops too.
// A validator holding a proof does not sign a proposal that leaves it out
// (for proofs it held before the epoch ended), so an equivocating proposer
// cannot keep itself in the set by omitting the proof.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
)

const (
	// MaxProofsPerEpoch is the most equivocation proofs one proposal carries.
	MaxProofsPerEpoch = 4
	// StartListenEpochs: a started validator listens this long for another
	// instance of its own key before it signs.
	StartListenEpochs = 2
	// DupQuietEpochs: after messages from another instance of our key, we
	// sign again only once it has been quiet this long.
	DupQuietEpochs = 15
	epMsgProof     = "eqv"
	proofDomain    = "s5"
)

// SignedResult is one validator signature of an epoch fingerprint (an
// EpochAttestation without the epoch and validator, which the proof holds).
type SignedResult struct {
	Fingerprint EpochHash `json:"fp"`
	Stamp       int64     `json:"t"`
	Sig         []byte    `json:"s"`
}

func (r *SignedResult) verify(validator string, epoch uint64) error {
	a := &EpochAttestation{Epoch: epoch, Fingerprint: r.Fingerprint, Stamp: r.Stamp}
	return verifyByID(validator, a.msg(), r.Sig)
}

// EquivocationProof is two signatures by one validator of different results
// for the same epoch. A sorts before B by fingerprint (canonical form).
type EquivocationProof struct {
	Validator string        `json:"v"`
	Epoch     uint64        `json:"e"`
	A         *SignedResult `json:"a"`
	B         *SignedResult `json:"b"`
}

// NewEquivocationProof makes a canonical proof from two attestations.
func NewEquivocationProof(a, b *EpochAttestation) (*EquivocationProof, error) {
	if a.Validator != b.Validator || a.Epoch != b.Epoch {
		return nil, errors.New("attestations of different validators or epochs")
	}
	ra := &SignedResult{Fingerprint: a.Fingerprint, Stamp: a.Stamp, Sig: a.Sig}
	rb := &SignedResult{Fingerprint: b.Fingerprint, Stamp: b.Stamp, Sig: b.Sig}
	if bytes.Compare(ra.Fingerprint[:], rb.Fingerprint[:]) > 0 {
		ra, rb = rb, ra
	}
	p := &EquivocationProof{Validator: a.Validator, Epoch: a.Epoch, A: ra, B: rb}
	return p, p.Verify()
}

// Verify checks the proof: two valid signatures by the validator's key of
// different fingerprints for the same epoch, in canonical order.
func (p *EquivocationProof) Verify() error {
	if p == nil || p.A == nil || p.B == nil {
		return errors.New("incomplete proof")
	}
	if c := bytes.Compare(p.A.Fingerprint[:], p.B.Fingerprint[:]); c == 0 {
		return errors.New("same fingerprint: not conflicting")
	} else if c > 0 {
		return errors.New("results not in canonical order")
	}
	if err := p.A.verify(p.Validator, p.Epoch); err != nil {
		return fmt.Errorf("first signature: %v", err)
	}
	if err := p.B.verify(p.Validator, p.Epoch); err != nil {
		return fmt.Errorf("second signature: %v", err)
	}
	return nil
}

// write adds the proof to a proposal hash.
func (p *EquivocationProof) write(b *bytes.Buffer) {
	writeStr(b, p.Validator)
	_ = binary.Write(b, binary.BigEndian, p.Epoch)
	for _, r := range []*SignedResult{p.A, p.B} {
		b.Write(r.Fingerprint[:])
		_ = binary.Write(b, binary.BigEndian, r.Stamp)
		_ = binary.Write(b, binary.BigEndian, uint16(len(r.Sig)))
		b.Write(r.Sig)
	}
}

// ID identifies a proof (hash of its canonical serialization).
func (p *EquivocationProof) ID() EpochHash {
	b := new(bytes.Buffer)
	p.write(b)
	return sha256.Sum256(b.Bytes())
}

// ---------------- ledger ----------------

// ledgerBan is a ban in the chain: the record that carried the proof.
type ledgerBan struct {
	proof *EquivocationProof
	epoch uint64 // record epoch; banned from epoch+1
}

// bannedAt: id is banned for epoch n, from records up to asOf.
func (l *bondLedger) bannedAt(id string, n, asOf uint64) bool {
	b := l.bans[id]
	return b != nil && b.epoch <= asOf && b.epoch < n
}

// applyBans adds a finalized record's proofs (S5).
func (l *bondLedger) applyBans(rec *EpochRecord) []*ledgerBan {
	if rec.Proposal == nil {
		return nil
	}
	var added []*ledgerBan
	for _, p := range rec.Proposal.Proofs {
		if l.bans[p.Validator] != nil {
			continue
		}
		b := &ledgerBan{proof: p, epoch: rec.Epoch}
		l.bans[p.Validator] = b
		added = append(added, b)
	}
	return added
}

// ---------------- penalty hook ----------------

// BanInfo describes a ban for the penalty hook and the status API.
type BanInfo struct {
	Validator  string             `json:"validator"`
	Name       string             `json:"name"`
	ProofEpoch uint64             `json:"proofEpoch"`
	BanRecord  uint64             `json:"banRecord"` // record carrying the proof
	BannedFrom uint64             `json:"bannedFrom"`
	Seeded     bool               `json:"seeded"`
	Power      uint64             `json:"power"`
	Bond       *BondClaim         `json:"bond,omitempty"` // the member's bond, if bonded
	Proof      *EquivocationProof `json:"proof"`
	ProofID    string             `json:"proofID"`
	Synced     bool               `json:"synced,omitempty"`
}

// PenaltyHook is called (in its own goroutine) for every ban this node
// applies. S5 installs none: there is no slashing or forfeiture until the
// bond policy (slashable vs one-time payment) is decided.
type PenaltyHook func(*BanInfo)

// SetPenaltyHook sets the hook called for each new ban.
func (e *EpochEngine) SetPenaltyHook(f PenaltyHook) {
	e.mtx.Lock()
	e.penalty = f
	e.mtx.Unlock()
}

// ---------------- engine: files ----------------

func (e *EpochEngine) bansPath() string {
	if e.cfg.StorePath == "" {
		return ""
	}
	return e.cfg.StorePath + ".bans.json"
}

func (e *EpochEngine) guardPath() string {
	if e.cfg.StorePath == "" {
		return ""
	}
	return e.cfg.StorePath + ".signguard"
}

type proofFile struct {
	Release string               `json:"release"`
	Proofs  []*EquivocationProof `json:"proofs"`
}

type signGuard struct {
	Epoch uint64 `json:"epoch"`
	FP    string `json:"fp"`
}

// loadS5Files loads the proofs we hold and the sign guard.
func (e *EpochEngine) loadS5Files() {
	if p := e.bansPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			var f proofFile
			h := e.rel.Hash()
			if json.Unmarshal(b, &f) == nil && f.Release == hex.EncodeToString(h[:]) {
				for _, pr := range f.Proofs {
					if pr.Verify() == nil {
						e.proofs[pr.Validator] = &proofEntry{proof: pr, recv: 0}
					}
				}
				if len(e.proofs) > 0 {
					e.log.Infof("Loaded %d equivocation proof(s) we hold", len(e.proofs))
				}
			}
		}
	}
	if p := e.guardPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			var g signGuard
			if json.Unmarshal(b, &g) == nil {
				if fp, err := hex.DecodeString(g.FP); err == nil && len(fp) == 32 {
					e.guardEpoch = g.Epoch
					copy(e.guardFP[:], fp)
					e.log.Infof("Sign guard: last signed epoch %d", g.Epoch)
				}
			}
		}
	}
}

func (e *EpochEngine) writeProofsLocked() {
	p := e.bansPath()
	if p == "" {
		return
	}
	h := e.rel.Hash()
	f := proofFile{Release: hex.EncodeToString(h[:])}
	for _, pe := range e.proofs {
		f.Proofs = append(f.Proofs, pe.proof)
	}
	sort.Slice(f.Proofs, func(i, j int) bool { return f.Proofs[i].Validator < f.Proofs[j].Validator })
	b, _ := json.MarshalIndent(&f, "", " ")
	if err := os.WriteFile(p+".tmp", b, 0o600); err == nil {
		_ = os.Rename(p+".tmp", p)
	}
}

// guardSignLocked: we may sign fp for epoch n; records it (and the file)
// before the signature leaves this node.
func (e *EpochEngine) guardSignLocked(n uint64, fp EpochHash) error {
	if n < e.guardEpoch || (n == e.guardEpoch && fp != e.guardFP) {
		return fmt.Errorf("sign guard: already signed epoch %d (%s), refusing epoch %d (%s)", e.guardEpoch, e.guardFP, n, fp)
	}
	if n == e.guardEpoch {
		return nil
	}
	e.guardEpoch, e.guardFP = n, fp
	if p := e.guardPath(); p != "" {
		b, _ := json.Marshal(&signGuard{Epoch: n, FP: fp.String()})
		f, err := os.OpenFile(p+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(b)
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return err
		}
		if err := os.Rename(p+".tmp", p); err != nil {
			return err
		}
	}
	return nil
}

// ---------------- engine: detection ----------------

type proofEntry struct {
	proof *EquivocationProof
	recv  int64 // local ms (0: loaded from disk)
}

type firstSig struct {
	att *EpochAttestation
}

// noteSigLocked records a validator's signature of an epoch result and
// returns a proof if it conflicts with one seen before.
func (e *EpochEngine) noteSigLocked(a *EpochAttestation) *EquivocationProof {
	key := fmt.Sprintf("%s|%d", a.Validator, a.Epoch)
	first := e.firstSigs[key]
	if first == nil {
		e.firstSigs[key] = &firstSig{att: a}
		return nil
	}
	if first.att.Fingerprint == a.Fingerprint {
		return nil
	}
	p, err := NewEquivocationProof(first.att, a)
	if err != nil {
		return nil
	}
	return p
}

// addProofLocked adds a verified proof; reports whether it is new.
func (e *EpochEngine) addProofLocked(p *EquivocationProof, how string) bool {
	if have := e.proofs[p.Validator]; have != nil {
		return false
	}
	e.proofs[p.Validator] = &proofEntry{proof: p, recv: e.nowMs()}
	e.writeProofsLocked()
	e.addFlagLocked(&EpochFlag{Epoch: p.Epoch, Kind: "equivocation", Peer: p.Validator,
		Detail: fmt.Sprintf("signed two results for epoch %d (%s, %s); proof %s", p.Epoch, p.A.Fingerprint, p.B.Fingerprint, how)})
	return true
}

// checkSigLocked runs detection on a validly signed attestation.
func (e *EpochEngine) checkSigLocked(a *EpochAttestation) (newProof *EquivocationProof) {
	if p := e.noteSigLocked(a); p != nil && e.addProofLocked(p, "detected") {
		return p
	}
	return nil
}

// handleProof checks a gossiped proof.
func (e *EpochEngine) handleProof(from string, p *EquivocationProof) {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	if have := e.proofs[p.Validator]; have != nil && have.proof.ID() == p.ID() {
		return
	}
	if err := p.Verify(); err != nil {
		e.badProofs++
		e.addFlagLocked(&EpochFlag{Epoch: p.Epoch, Kind: "bad-proof", Peer: from,
			Detail: fmt.Sprintf("rejected equivocation proof against %s: %v", e.nameOf(p.Validator), err)})
		return
	}
	if !e.rel.IsValidator(p.Validator) && !e.ledger.everMember(p.Validator) {
		return
	}
	e.addProofLocked(p, "received")
}

// everMember: id has (had) an admitted bond in the chain.
func (l *bondLedger) everMember(id string) bool {
	for _, b := range l.bonds {
		if b.claim.Validator == id {
			return true
		}
	}
	return false
}

// checkS5Locked is the deterministic check of a proposal's proofs.
func (e *EpochEngine) checkS5Locked(p *EpochProposal) error {
	vs := e.valsLocked(p.Epoch)
	for _, pr := range p.Proofs {
		if err := pr.Verify(); err != nil {
			e.badProofs++
			return fmt.Errorf("bad equivocation proof against %s: %v", e.nameOf(pr.Validator), err)
		}
		if pr.Epoch > p.Epoch {
			return errors.New("equivocation proof from a future epoch")
		}
		if !vs.IsValidator(pr.Validator) {
			return fmt.Errorf("equivocation proof against %s, not a validator of epoch %d (or already banned)", e.nameOf(pr.Validator), p.Epoch)
		}
	}
	return nil
}

// s5ProposalLocked adds the proofs we hold against current members.
func (e *EpochEngine) s5ProposalLocked(p *EpochProposal) {
	vs := e.valsLocked(p.Epoch)
	for _, pe := range e.proofs {
		if vs.IsValidator(pe.proof.Validator) && pe.proof.Epoch <= p.Epoch {
			p.Proofs = append(p.Proofs, pe.proof)
		}
	}
	sort.Slice(p.Proofs, func(i, j int) bool { return p.Proofs[i].Validator < p.Proofs[j].Validator })
	if len(p.Proofs) > MaxProofsPerEpoch {
		p.Proofs = p.Proofs[:MaxProofsPerEpoch]
	}
	if e.forgeMode == "propose" && e.forgeAgainst != "" {
		if f := e.forgedProofLocked(e.forgeAgainst); f != nil {
			p.Proofs = append(p.Proofs, f)
			sort.Slice(p.Proofs, func(i, j int) bool { return p.Proofs[i].Validator < p.Proofs[j].Validator })
			e.log.Warnf("ADVERSARY: proposal for epoch %d carries a forged proof against %s", p.Epoch, e.nameOf(e.forgeAgainst))
		}
	}
}

// bansOKLocked: we only sign a proposal that carries every proof we held
// before its epoch ended against a validator still in its set.
func (e *EpochEngine) bansOKLocked(p *EpochProposal) bool {
	if len(p.Proofs) >= MaxProofsPerEpoch {
		return true
	}
	in := make(map[string]bool, len(p.Proofs))
	for _, pr := range p.Proofs {
		in[pr.Validator] = true
	}
	vs := e.valsLocked(p.Epoch)
	end := e.p.EpochEnd(p.Epoch)
	for v, pe := range e.proofs {
		if pe.recv < end && vs.IsValidator(v) && !in[v] && pe.proof.Epoch <= p.Epoch {
			e.log.Infof("Not signing epoch %d: the proposal from %s leaves out our equivocation proof against %s",
				p.Epoch, e.nameOf(p.Proposer), e.nameOf(v))
			return false
		}
	}
	return true
}

// afterBansLocked runs for bans a record added.
func (e *EpochEngine) afterBansLocked(added []*ledgerBan, synced bool) {
	for _, b := range added {
		pr := b.proof
		vs := e.ledger.members(b.epoch, b.epoch-1) // the set the proof was checked against
		info := &BanInfo{Validator: pr.Validator, Name: e.nameOf(pr.Validator), ProofEpoch: pr.Epoch, BanRecord: b.epoch,
			BannedFrom: b.epoch + 1, Proof: pr, ProofID: pr.ID().String(), Synced: synced}
		if m := vs.members[pr.Validator]; m != nil {
			info.Seeded, info.Power = m.Seeded, m.Power
			if m.Bond != nil {
				info.Bond = m.Bond.claim
			}
		}
		if _, have := e.proofs[pr.Validator]; !have {
			e.proofs[pr.Validator] = &proofEntry{proof: pr}
			e.writeProofsLocked()
		}
		if !synced {
			e.log.Warnf("VALIDATOR BANNED: %s equivocated in epoch %d; the proof is in record %d, banned from epoch %d on (no bond penalty in this release)",
				info.Name, pr.Epoch, b.epoch, b.epoch+1)
		} else {
			e.log.Infof("Validator %s is banned since epoch %d (equivocation in epoch %d)", info.Name, b.epoch+1, pr.Epoch)
		}
		if pr.Validator == e.self {
			e.log.Errorf("OUR VALIDATOR KEY IS BANNED for signing two results in epoch %d. We no longer sign.", pr.Epoch)
		}
		if e.penalty != nil {
			go e.penalty(info)
		}
	}
	if len(added) > 0 {
		e.valsCache, e.activeCache = nil, nil
	}
}

// ---------------- engine: duplicate key ----------------

// ownKeyMsg checks a message from our own peer ID that this instance did not
// send: another instance runs with our key.
func (e *EpochEngine) ownKeyMsg(b []byte) {
	var m epochMsg
	if json.Unmarshal(b, &m) != nil {
		return
	}
	var stamp int64
	var what string
	switch {
	case m.Type == epMsgHB && m.HB != nil && m.HB.Validator == e.self:
		if verifySig(m.HB.Validator, m.HB.PubKey, m.HB.msg(), m.HB.Sig) != nil {
			return
		}
		stamp, what = m.HB.Stamp, "heartbeat"
	case m.Type == epMsgAtt && m.Att != nil && m.Att.Validator == e.self:
		if m.Att.verify() != nil {
			return
		}
		stamp, what = m.Att.Stamp, fmt.Sprintf("signature of epoch %d", m.Att.Epoch)
	case m.Type == epMsgProp && m.Prop != nil && m.Prop.Proposer == e.self:
		if h := m.Prop.Hash(); verifySig(m.Prop.Proposer, m.Prop.PubKey, h[:], m.Prop.Sig) != nil {
			return
		}
		stamp, what = m.Prop.Stamp, fmt.Sprintf("proposal for epoch %d", m.Prop.Epoch)
	default:
		return
	}
	e.mtx.Lock()
	defer e.mtx.Unlock()
	if e.sentStamps[stamp] || stamp < e.startedAt {
		return // ours (echo), or from before we started (a previous run)
	}
	nowMs := e.nowMs()
	first := e.dupAt == 0
	e.dupAt = nowMs
	e.dupCount++
	if first || nowMs-e.dupFlagAt > 10*e.p.EpochMs {
		e.dupFlagAt = nowMs
		e.addFlagLocked(&EpochFlag{Epoch: e.p.EpochOf(nowMs), Kind: "duplicate-key", Peer: e.self,
			Detail: fmt.Sprintf("another instance is running with our validator key (its %s); not signing until it has been quiet for %d epochs", what, DupQuietEpochs)})
	}
}

// signHoldLocked is why we may not sign now ("" if we may).
func (e *EpochEngine) signHoldLocked(nowMs int64) string {
	if nowMs < e.startedAt+StartListenEpochs*e.p.EpochMs {
		return "listening for another instance of our key after start"
	}
	if e.dupAt > 0 && nowMs-e.dupAt < DupQuietEpochs*e.p.EpochMs {
		return "another instance of our validator key is running"
	}
	return ""
}

func (e *EpochEngine) noteSentLocked(stamp int64) {
	e.sentStamps[stamp] = true
	if len(e.sentStamps) > 4000 {
		cut := e.nowMs() - 600*e.p.EpochMs
		for s := range e.sentStamps {
			if s < cut {
				delete(e.sentStamps, s)
			}
		}
	}
}

// ---------------- engine: gossip, pruning ----------------

// s5Gossip re-sends the proofs we hold against members every few epochs.
func (e *EpochEngine) s5Gossip(ctx context.Context, cur uint64) {
	if cur%3 != 1 {
		return
	}
	e.mtx.Lock()
	if e.s5SentAt == cur {
		e.mtx.Unlock()
		return
	}
	e.s5SentAt = cur
	vs := e.valsLocked(cur)
	var msgs []*epochMsg
	for v, pe := range e.proofs {
		if vs.IsValidator(v) {
			msgs = append(msgs, &epochMsg{Type: epMsgProof, Proof: pe.proof})
		}
	}
	forge := e.forgeMode == "gossip" && e.forgeAgainst != ""
	if forge {
		if f := e.forgedProofLocked(e.forgeAgainst); f != nil {
			msgs = append(msgs, &epochMsg{Type: epMsgProof, Proof: f})
			e.log.Warnf("ADVERSARY: gossiping a forged equivocation proof against %s", e.nameOf(e.forgeAgainst))
		}
	}
	for k := range e.firstSigs {
		var ep uint64
		if _, err := fmt.Sscanf(k[len(k)-trailingDigits(k):], "%d", &ep); err == nil && ep+200 < cur {
			delete(e.firstSigs, k)
		}
	}
	e.mtx.Unlock()
	for _, m := range msgs {
		e.send(ctx, m)
	}
}

func trailingDigits(s string) int {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] >= '0' && s[i] <= '9'; i-- {
		n++
	}
	return n
}

// ---------------- debug (simnet adversary tests) ----------------

// SetEquivocate makes this validator also sign a conflicting result for the
// next n epochs it signs (simnet adversary tests only).
func (e *EpochEngine) SetEquivocate(n int) {
	e.mtx.Lock()
	e.equivocate = n
	e.mtx.Unlock()
	e.log.Warnf("ADVERSARY: will sign two different results for the next %d epoch(s)", n)
}

// SetForgeProof makes this node gossip ("gossip") or propose ("propose") a
// forged equivocation proof against validator id ("off"/"": stop).
func (e *EpochEngine) SetForgeProof(id, mode string) error {
	switch mode {
	case "", "off":
		id, mode = "", ""
	case "gossip", "propose":
	default:
		return errors.New("mode is gossip, propose or off")
	}
	e.mtx.Lock()
	e.forgeAgainst, e.forgeMode = id, mode
	e.mtx.Unlock()
	return nil
}

// forgedProofLocked builds a proof against an honest validator: its real
// signature of a finalized record, and that signature reused for another
// fingerprint (it cannot sign anything else with someone else's key).
func (e *EpochEngine) forgedProofLocked(id string) *EquivocationProof {
	if e.last == nil {
		return nil
	}
	for i := len(e.records) - 1; i >= 0 && i >= len(e.records)-20; i-- {
		r := e.records[i]
		for _, a := range r.Sigs {
			if a.Validator != id {
				continue
			}
			other := sha256.Sum256(append([]byte("forged"), a.Fingerprint[:]...))
			ra := &SignedResult{Fingerprint: a.Fingerprint, Stamp: a.Stamp, Sig: a.Sig}
			rb := &SignedResult{Fingerprint: other, Stamp: a.Stamp, Sig: a.Sig}
			if bytes.Compare(ra.Fingerprint[:], rb.Fingerprint[:]) > 0 {
				ra, rb = rb, ra
			}
			return &EquivocationProof{Validator: id, Epoch: r.Epoch, A: ra, B: rb}
		}
	}
	return nil
}

// equivocateLocked returns a second, conflicting attestation for candidate
// c if the adversary switch is on.
func (e *EpochEngine) equivocateLocked(c *epochCandidate, nowMs int64) *EpochAttestation {
	if e.equivocate <= 0 || c.res == nil {
		return nil
	}
	e.equivocate--
	e.equivocated++
	alt := sha256.Sum256(append([]byte("tatanka-alt-state"), c.res.fp[:]...))
	fp := epochFingerprint(c.prop.Epoch, c.prop.Prev, c.hash, c.prop.Active, nil, alt)
	a := &EpochAttestation{Epoch: c.prop.Epoch, Fingerprint: fp, Stamp: nowMs + 1}
	if err := a.sign(e.priv); err != nil {
		return nil
	}
	e.noteSentLocked(a.Stamp)
	e.log.Warnf("ADVERSARY: signed a second result for epoch %d (%s besides %s)", c.prop.Epoch, fp, c.res.fp)
	return a
}

// ---------------- status ----------------

// BansStatus is the S5 state for the debug API and the Mesh page.
type BansStatus struct {
	Banned       []*BanInfo `json:"banned"`
	Held         []*BanInfo `json:"held"` // proofs we hold that the chain lacks
	BadProofs    int        `json:"badProofs"`
	DupKey       bool       `json:"duplicateKey"`
	DupCount     int        `json:"duplicateKeyMsgs"`
	DupAt        int64      `json:"duplicateKeyAt,omitempty"`
	SignHold     string     `json:"signHold,omitempty"`
	GuardEpoch   uint64     `json:"signGuardEpoch"`
	Equivocated  int        `json:"equivocatedDebug,omitempty"`
	ForgeMode    string     `json:"forgeModeDebug,omitempty"`
	ForgeAgainst string     `json:"forgeAgainstDebug,omitempty"`
}

// BansStatus reports bans in our chain, proofs we hold, and our own guards.
func (e *EpochEngine) BansStatus() *BansStatus {
	nowMs := e.nowMs()
	e.mtx.Lock()
	defer e.mtx.Unlock()
	st := &BansStatus{BadProofs: e.badProofs, DupCount: e.dupCount, DupAt: e.dupAt, GuardEpoch: e.guardEpoch,
		Equivocated: e.equivocated, ForgeMode: e.forgeMode, ForgeAgainst: e.forgeAgainst}
	st.DupKey = e.dupAt > 0 && nowMs-e.dupAt < DupQuietEpochs*e.p.EpochMs
	if e.signer {
		st.SignHold = e.signHoldLocked(nowMs)
	}
	var ids []string
	for id := range e.ledger.bans {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b := e.ledger.bans[id]
		st.Banned = append(st.Banned, &BanInfo{Validator: id, Name: e.nameOf(id), ProofEpoch: b.proof.Epoch, BanRecord: b.epoch,
			BannedFrom: b.epoch + 1, Seeded: e.rel.IsValidator(id), Proof: b.proof, ProofID: b.proof.ID().String()})
	}
	for id, pe := range e.proofs {
		if e.ledger.bans[id] == nil {
			st.Held = append(st.Held, &BanInfo{Validator: id, Name: e.nameOf(id), ProofEpoch: pe.proof.Epoch, Proof: pe.proof,
				ProofID: pe.proof.ID().String(), Seeded: e.rel.IsValidator(id)})
		}
	}
	return st
}
