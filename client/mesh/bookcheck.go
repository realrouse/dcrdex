// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S7 book cross-checks. Every validator in the current validator set
// publishes a signed book digest every few epochs: its last finalized epoch,
// that epoch's state hash, the IDs of the resting orders at that epoch and
// the IDs of the orders waiting in its pool. Every peer, validators and
// clients alike, compares each digest with its own view:
//
//   - resting orders: the book after a finalized epoch is deterministic, so
//     a digest listing fewer (hidden) or other orders than our own snapshot
//     of that epoch, or another state hash, is wrong at once;
//   - pending orders: an order we have held for a while (WithholdGraceMs)
//     and that was not yet finalized at the digest's epoch should be in the
//     validator's pool. A missing order may just have been lost on the way,
//     so the order is sent again (repair) and the validator is reported as
//     withholding only if a later digest still leaves it out.
//
// A validator whose digests are wrong twice in a row is a suspect. Digests
// also carry the sender's recent proposer-omission counts (S1 flags), so a
// client sees which proposers several validators caught leaving orders out.
// Nothing here changes consensus: a hiding validator cannot remove orders
// from the book (any other proposer includes them); it is only exposed.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sort"
)

const (
	bookDigestDomain   = "tatanka-book-digest-v1"
	epMsgBookDigest    = "bookd"
	epMsgOrderRepair   = "orep" // an order re-sent to a validator whose digest lacked it
	bookDigestEvery    = 3      // epochs between a validator's digests
	bookSnapEpochs     = 64     // resting snapshots / finalized pool orders kept
	bookMaxPending     = 1000
	bookSuspectStreak  = 2 // consecutive wrong digests
	bookMaxExamples    = 5
	bookOmitWindow     = 30 // epochs of omission counts carried in digests
	bookFlagEveryBad   = 10
	bookRepairMaxBatch = 25
)

// BookDigest is a validator's signed summary of its book.
type BookDigest struct {
	Validator string    `json:"validator"`
	Epoch     uint64    `json:"epoch"` // last finalized epoch
	HaveEpoch bool      `json:"have"`
	StateHash EpochHash `json:"stateHash"`
	Resting   []OrderID `json:"resting"`
	Pending   []OrderID `json:"pending"`
	// PendingFull: the pool had more than bookMaxPending orders.
	PendingFull bool `json:"pendingFull,omitempty"`
	// Omits: proposers this validator flagged for omissions in the last
	// bookOmitWindow epochs (S1), by peer ID.
	Omits  map[string]int `json:"omits,omitempty"`
	Stamp  int64          `json:"stamp"`
	PubKey []byte         `json:"pubkey"`
	Sig    []byte         `json:"sig"`
}

func (d *BookDigest) msg() []byte {
	b := new(bytes.Buffer)
	b.WriteString(bookDigestDomain)
	writeStr(b, d.Validator)
	_ = binary.Write(b, binary.BigEndian, d.Epoch)
	if d.HaveEpoch {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	b.Write(d.StateHash[:])
	_ = binary.Write(b, binary.BigEndian, uint32(len(d.Resting)))
	for _, id := range d.Resting {
		b.Write(id[:])
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(d.Pending)))
	for _, id := range d.Pending {
		b.Write(id[:])
	}
	if d.PendingFull {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	ks := make([]string, 0, len(d.Omits))
	for k := range d.Omits {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	_ = binary.Write(b, binary.BigEndian, uint16(len(ks)))
	for _, k := range ks {
		writeStr(b, k)
		_ = binary.Write(b, binary.BigEndian, int32(d.Omits[k]))
	}
	_ = binary.Write(b, binary.BigEndian, d.Stamp)
	return b.Bytes()
}

// poolDone is an order that left our pool when it was finalized.
type poolDone struct {
	recv  int64
	ord   *MeshOrder
	epoch uint64
}

// bookPeer is what we learned from one validator's digests.
type bookPeer struct {
	digests      int
	lastEpoch    uint64
	lastStamp    int64
	compared     int // digests whose resting list we could compare
	hiddenNow    int
	extraNow     int
	mismatchNow  bool
	withheldNow  int
	missingNow   int // pending orders missing (first miss, repaired)
	badStreak    int
	badDigests   int
	hiddenTotal  int // digests with hidden resting orders
	withheldDigs int // digests with orders still missing after a repair
	mismatches   int
	repairs      int
	suspect      bool
	suspectSince uint64
	examples     []string
	missing      map[OrderID]int // consecutive digests an order was missing
	omits        map[string]int  // omission counts this validator reports
	later        *BookDigest     // a digest ahead of our chain, checked later
}

// bookCheck is the per-engine cross-check state (guarded by e.mtx).
type bookCheck struct {
	snaps     map[uint64][]OrderID // finalized epoch -> sorted resting IDs
	done      map[OrderID]*poolDone
	peers     map[string]*bookPeer
	sentAt    uint64
	resent    map[OrderID]int64
	omitLog   []omitEvent
	hideOwner string // debug adversary: hide this owner's orders
	sent      int
}

type omitEvent struct {
	epoch    uint64
	proposer string
}

func newBookCheck() *bookCheck {
	return &bookCheck{snaps: make(map[uint64][]OrderID), done: make(map[OrderID]*poolDone),
		peers: make(map[string]*bookPeer), resent: make(map[OrderID]int64)}
}

// withholdGraceMs is how long we must have held an order before a
// validator's digest is expected to list it.
func (e *EpochEngine) withholdGraceMs() int64 {
	g := 2 * e.p.EpochMs
	if g < 3000 {
		g = 3000
	}
	return g
}

func sortIDs(ids []OrderID) {
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
}

// bookAfterCommitLocked snapshots the resting orders of a finalized record
// and remembers the pool orders it finalized. Called from commitLocked
// before the pool is cleaned.
func (e *EpochEngine) bookAfterCommitLocked(rec *EpochRecord) {
	bc := e.book
	ids := make([]OrderID, 0, len(e.state.resting))
	for id := range e.state.resting {
		ids = append(ids, id)
	}
	sortIDs(ids)
	bc.snaps[rec.Epoch] = ids
	if rec.Proposal != nil {
		for _, o := range rec.Proposal.Orders {
			id := o.ID()
			if po := e.pool[id]; po != nil {
				bc.done[id] = &poolDone{recv: po.recv, ord: po.ord, epoch: rec.Epoch}
			}
		}
	}
	if rec.Epoch > bookSnapEpochs {
		cut := rec.Epoch - bookSnapEpochs
		for n := range bc.snaps {
			if n < cut {
				delete(bc.snaps, n)
			}
		}
		for id, d := range bc.done {
			if d.epoch < cut {
				delete(bc.done, id)
			}
		}
	}
}

// noteOmissionLocked logs a proposer omission flag for the digests.
func (e *EpochEngine) noteOmissionLocked(epoch uint64, proposer string) {
	bc := e.book
	bc.omitLog = append(bc.omitLog, omitEvent{epoch, proposer})
	if len(bc.omitLog) > 500 {
		bc.omitLog = bc.omitLog[len(bc.omitLog)-500:]
	}
}

func (e *EpochEngine) hiddenLocked(o *MeshOrder) bool {
	return e.book.hideOwner != "" && o != nil && o.Owner == e.book.hideOwner
}

// buildDigestLocked builds (unsigned) our digest.
func (e *EpochEngine) buildDigestLocked(nowMs int64) *BookDigest {
	d := &BookDigest{Validator: e.self, Stamp: nowMs}
	if e.last != nil {
		d.Epoch, d.HaveEpoch, d.StateHash = e.last.Epoch, true, e.last.StateHash
	}
	for id, r := range e.state.resting {
		if e.hiddenLocked(r.ord) {
			continue
		}
		d.Resting = append(d.Resting, id)
	}
	sortIDs(d.Resting)
	for id, po := range e.pool {
		if _, seen := e.state.seen[id]; seen || po.ord.Expiry <= nowMs || e.hiddenLocked(po.ord) {
			continue
		}
		d.Pending = append(d.Pending, id)
	}
	sortIDs(d.Pending)
	if len(d.Pending) > bookMaxPending {
		d.Pending, d.PendingFull = d.Pending[:bookMaxPending], true
	}
	cur := e.p.EpochOf(nowMs)
	for _, ev := range e.book.omitLog {
		if ev.epoch+bookOmitWindow >= cur {
			if d.Omits == nil {
				d.Omits = make(map[string]int)
			}
			d.Omits[ev.proposer]++
		}
	}
	return d
}

// bookGossip sends our digest every bookDigestEvery epochs while we are a
// member of the validator set.
func (e *EpochEngine) bookGossip(ctx context.Context, cur uint64) {
	if !e.signer || cur%bookDigestEvery != 1 {
		return
	}
	nowMs := e.nowMs()
	e.mtx.Lock()
	if e.book.sentAt == cur || !e.isMemberLocked(cur) || e.last == nil {
		e.mtx.Unlock()
		return
	}
	e.book.sentAt = cur
	d := e.buildDigestLocked(nowMs)
	e.mtx.Unlock()
	pid, pubB, err := ownerOf(e.priv)
	if err != nil {
		return
	}
	d.Validator, d.PubKey = pid.String(), pubB
	sig, err := e.priv.Sign(d.msg())
	if err != nil {
		return
	}
	d.Sig = sig
	e.mtx.Lock()
	e.book.sent++
	e.mtx.Unlock()
	e.send(ctx, &epochMsg{Type: epMsgBookDigest, Book: d})
}

// handleBookDigest cross-checks a validator's digest against our view.
func (e *EpochEngine) handleBookDigest(ctx context.Context, d *BookDigest) {
	if d.Validator == e.self || !e.isValidatorNow(d.Validator) {
		return
	}
	if verifySig(d.Validator, d.PubKey, d.msg(), d.Sig) != nil {
		return
	}
	nowMs := e.nowMs()
	if abs64(d.Stamp-nowMs) > 4*e.p.EpochMs+e.p.MaxClockSkewMs {
		return // stale or replayed
	}
	e.mtx.Lock()
	bc := e.book
	bp := bc.peers[d.Validator]
	if bp == nil {
		bp = &bookPeer{missing: make(map[OrderID]int)}
		bc.peers[d.Validator] = bp
	}
	if d.Stamp <= bp.lastStamp {
		e.mtx.Unlock()
		return
	}
	bp.digests++
	bp.lastEpoch, bp.lastStamp = d.Epoch, d.Stamp
	bp.omits = d.Omits
	var resend []*MeshOrder
	if le, ok := e.lastEpochLocked(); d.HaveEpoch && (!ok || d.Epoch > le) {
		// The validator is ahead of us: check the digest once we have
		// finalized its epoch (bookLater), not now.
		bp.later = d
	} else {
		resend = e.evalDigestLocked(bp, d, nowMs)
	}
	e.mtx.Unlock()
	for _, o := range resend {
		e.send(ctx, &epochMsg{Type: epMsgOrderRepair, Order: o})
	}
}

// bookLater checks digests that were ahead of our chain when they came.
func (e *EpochEngine) bookLater(ctx context.Context) {
	nowMs := e.nowMs()
	var resend []*MeshOrder
	e.mtx.Lock()
	le, ok := e.lastEpochLocked()
	for _, bp := range e.book.peers {
		d := bp.later
		if d == nil {
			continue
		}
		if nowMs-d.Stamp > bookDigestEvery*e.p.EpochMs*2 {
			bp.later = nil // we did not catch up in time; the next one will do
			continue
		}
		if ok && d.Epoch <= le {
			bp.later = nil
			resend = append(resend, e.evalDigestLocked(bp, d, d.Stamp)...)
		}
	}
	e.mtx.Unlock()
	for _, o := range resend {
		e.send(ctx, &epochMsg{Type: epMsgOrderRepair, Order: o})
	}
}

// evalDigestLocked compares a digest with our view. A digest we cannot
// compare at all leaves the validator's standing unchanged.
func (e *EpochEngine) evalDigestLocked(bp *bookPeer, d *BookDigest, nowMs int64) (resend []*MeshOrder) {
	bc := e.book
	bp.hiddenNow, bp.extraNow, bp.mismatchNow, bp.withheldNow, bp.missingNow = 0, 0, false, 0, 0
	var examples []string
	compared := false
	// 1. Resting orders at the digest's epoch.
	if snap, ok := bc.snaps[d.Epoch]; ok && d.HaveEpoch {
		compared = true
		bp.compared++
		if rec := e.byEpoch[d.Epoch]; rec != nil && rec.StateHash != d.StateHash {
			bp.mismatchNow = true
			examples = append(examples, fmt.Sprintf("epoch %d state hash %s, ours %s", d.Epoch, d.StateHash, rec.StateHash))
		}
		theirs := make(map[OrderID]bool, len(d.Resting))
		for _, id := range d.Resting {
			theirs[id] = true
		}
		for _, id := range snap {
			if !theirs[id] {
				bp.hiddenNow++
				if len(examples) < bookMaxExamples {
					examples = append(examples, "hidden resting "+id.String())
				}
			}
			delete(theirs, id)
		}
		bp.extraNow = len(theirs)
	}
	// 2. Pending orders, only if we are not behind the digest.
	le, haveLast := e.lastEpochLocked()
	if d.HaveEpoch && haveLast && d.Epoch <= le && !d.PendingFull {
		compared = true
		theirs := make(map[OrderID]bool, len(d.Pending))
		for _, id := range d.Pending {
			theirs[id] = true
		}
		grace := e.withholdGraceMs()
		expect := func(id OrderID, recv int64, o *MeshOrder) bool {
			return recv <= d.Stamp-grace && o.Stamp <= d.Stamp-grace && o.Expiry > d.Stamp+e.p.EpochMs
		}
		check := func(id OrderID, o *MeshOrder) {
			if theirs[id] {
				delete(bp.missing, id)
				return
			}
			bp.missing[id]++
			if bp.missing[id] >= 2 {
				bp.withheldNow++
				if len(examples) < bookMaxExamples {
					examples = append(examples, fmt.Sprintf("withheld %s (missing in %d digests)", id, bp.missing[id]))
				}
			} else {
				bp.missingNow++
			}
			if last := bc.resent[id]; nowMs-last > bookDigestEvery*e.p.EpochMs && len(resend) < bookRepairMaxBatch &&
				(e.signer || o.Owner == e.self) {
				bc.resent[id] = nowMs
				resend = append(resend, o)
			}
		}
		for id, po := range e.pool {
			if _, seen := e.state.seen[id]; seen || !expect(id, po.recv, po.ord) {
				continue
			}
			if e.gateOrderLocked(po.ord, e.p.EpochOf(d.Stamp)) != "" {
				continue
			}
			check(id, po.ord)
		}
		for id, pd := range bc.done {
			if pd.epoch > d.Epoch && expect(id, pd.recv, pd.ord) {
				check(id, pd.ord)
			}
		}
		for id := range bp.missing { // forget orders no longer expected
			if _, inPool := e.pool[id]; !inPool {
				if pd := bc.done[id]; pd == nil || pd.epoch <= d.Epoch {
					delete(bp.missing, id)
				}
			}
		}
	}
	bp.repairs += len(resend)
	if !compared {
		return resend
	}
	bad := bp.hiddenNow > 0 || bp.extraNow > 0 || bp.mismatchNow || bp.withheldNow > 0
	if bp.hiddenNow > 0 {
		bp.hiddenTotal++
	}
	if bp.withheldNow > 0 {
		bp.withheldDigs++
	}
	if bp.mismatchNow {
		bp.mismatches++
	}
	if bad {
		bp.badStreak++
		bp.badDigests++
		bp.examples = examples
		if bp.badStreak >= bookSuspectStreak && !bp.suspect {
			bp.suspect, bp.suspectSince = true, d.Epoch
		}
		if bp.badStreak == bookSuspectStreak || (bp.badStreak > bookSuspectStreak && bp.badStreak%bookFlagEveryBad == 0) {
			kind := "book-hidden"
			if bp.hiddenNow == 0 && bp.withheldNow > 0 {
				kind = "book-withheld"
			} else if bp.hiddenNow == 0 && bp.withheldNow == 0 {
				kind = "book-mismatch"
			}
			e.addFlagLocked(&EpochFlag{Epoch: d.Epoch, Kind: kind, Peer: d.Validator,
				Detail: fmt.Sprintf("book digest: %d hidden resting, %d extra, %d withheld pending, state mismatch %v; %d wrong digests in a row",
					bp.hiddenNow, bp.extraNow, bp.withheldNow, bp.mismatchNow, bp.badStreak)})
		}
	} else {
		bp.badStreak = 0
		bp.suspect = false
	}
	for id, at := range bc.resent {
		if nowMs-at > 600_000 {
			delete(bc.resent, id)
		}
	}
	return resend
}

// SetHideOwner makes this validator hide the orders of owner: they are
// left out of its proposals and its book digests (simnet adversary tests
// only; "" clears).
func (e *EpochEngine) SetHideOwner(owner string) {
	e.mtx.Lock()
	e.book.hideOwner = owner
	e.mtx.Unlock()
}

// BookPeerStatus is the cross-check result for one validator.
type BookPeerStatus struct {
	Validator      string         `json:"validator"`
	Name           string         `json:"name"`
	Digests        int            `json:"digests"`
	Compared       int            `json:"compared"`
	LastEpoch      uint64         `json:"lastEpoch"`
	HiddenNow      int            `json:"hiddenNow"`
	ExtraNow       int            `json:"extraNow"`
	MismatchNow    bool           `json:"mismatchNow"`
	WithheldNow    int            `json:"withheldNow"`
	MissingNow     int            `json:"missingNow"`
	BadStreak      int            `json:"badStreak"`
	BadDigests     int            `json:"badDigests"`
	HiddenDigests  int            `json:"hiddenDigests"`
	WithheldDigest int            `json:"withheldDigests"`
	Mismatches     int            `json:"mismatches"`
	Repairs        int            `json:"repairs"`
	Suspect        bool           `json:"suspect"`
	SuspectSince   uint64         `json:"suspectSince,omitempty"`
	Examples       []string       `json:"examples,omitempty"`
	OmitReports    map[string]int `json:"omitReports,omitempty"` // proposers this validator flagged (by name)
	OmittedBy      map[string]int `json:"omittedBy,omitempty"`   // validators reporting omissions by this one
}

// BookCheckStatus is the cross-check view of this node.
type BookCheckStatus struct {
	Self      string            `json:"self"`
	Sent      int               `json:"digestsSent"`
	HideDebug string            `json:"hideOwnerDebug,omitempty"`
	Peers     []*BookPeerStatus `json:"peers"`
	Suspects  []string          `json:"suspects"`
}

// BookCheck returns the cross-check status.
func (e *EpochEngine) BookCheck() *BookCheckStatus {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	bc := e.book
	st := &BookCheckStatus{Self: e.nameOf(e.self), Sent: bc.sent, HideDebug: bc.hideOwner, Suspects: []string{}}
	byPeer := make(map[string]*BookPeerStatus)
	ids := make([]string, 0, len(bc.peers))
	for id := range bc.peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		bp := bc.peers[id]
		ps := &BookPeerStatus{Validator: id, Name: e.nameOf(id), Digests: bp.digests, Compared: bp.compared, LastEpoch: bp.lastEpoch,
			HiddenNow: bp.hiddenNow, ExtraNow: bp.extraNow, MismatchNow: bp.mismatchNow, WithheldNow: bp.withheldNow,
			MissingNow: bp.missingNow, BadStreak: bp.badStreak, BadDigests: bp.badDigests, HiddenDigests: bp.hiddenTotal,
			WithheldDigest: bp.withheldDigs, Mismatches: bp.mismatches, Repairs: bp.repairs, Suspect: bp.suspect,
			SuspectSince: bp.suspectSince, Examples: bp.examples}
		for p, n := range bp.omits {
			if ps.OmitReports == nil {
				ps.OmitReports = make(map[string]int)
			}
			ps.OmitReports[e.nameOf(p)] = n
		}
		byPeer[id] = ps
		st.Peers = append(st.Peers, ps)
		if bp.suspect {
			st.Suspects = append(st.Suspects, ps.Name)
		}
	}
	for rid, bp := range bc.peers {
		for p, n := range bp.omits {
			if ps := byPeer[p]; ps != nil {
				if ps.OmittedBy == nil {
					ps.OmittedBy = make(map[string]int)
				}
				ps.OmittedBy[e.nameOf(rid)] = n
			}
		}
	}
	return st
}
