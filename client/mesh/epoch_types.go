// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S1 wire types: proposals, attestations, records and their hashes.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
)

// EpochHash is a 32-byte hash (hex in JSON).
type EpochHash = OrderID

// EpochTopic carries orders, cancels, proposals, attestations, records and
// heartbeats for epoch consensus.
const EpochTopic = "epochs.tatanka-dex"

const (
	epochPropDomain = "tatanka-epoch-prop-v2" // v2 (S3): proposals carry Credits
	epochFPDomain   = "tatanka-epoch-fp-v1"
	epochAttDomain  = "tatanka-epoch-att-v1"
	epochHBDomain   = "tatanka-epoch-hb-v1"
)

// GenesisHash is the Prev of the first record of a release's chain.
func GenesisHash(r *Release) EpochHash {
	h := r.Hash()
	return sha256.Sum256(append([]byte("tatanka-epoch-genesis-v1"), h[:]...))
}

// EpochProposal is a proposer's list of orders and cancels for an epoch.
// Active is the epoch's active validator set (S3, epoch_active.go): every
// peer computes it from the signer credits in the finalized chain and
// rejects a proposal whose list differs. The finalization threshold is
// relative to the active validators' power. Credits are the validator
// signatures of the previous record (Prev) that the proposer has seen,
// including late ones: once finalized they are the canonical signer list of
// that record for the active-set computation.
type EpochProposal struct {
	Epoch    uint64         `json:"epoch"`
	Prev     EpochHash      `json:"prev"`
	Proposer string         `json:"proposer"`
	Rank     int            `json:"rank"`
	Active   []string       `json:"active"`
	Credits  []*EpochCredit `json:"credits,omitempty"`
	Orders   []*MeshOrder   `json:"orders"`
	Cancels  []*MeshCancel  `json:"cancels"`
	Stamp    int64          `json:"stamp"`
	PubKey   []byte         `json:"pubkey"`
	Sig      []byte         `json:"sig"`
}

// EpochCredit is a validator's attestation of the previous record (the
// epoch and fingerprint are the proposal's Prev record, so they are not
// repeated). The public key comes from the validator's peer ID.
type EpochCredit struct {
	Validator string `json:"v"`
	Stamp     int64  `json:"t"`
	Sig       []byte `json:"s"`
}

// verify checks the credit as an attestation of (epoch, fp).
func (c *EpochCredit) verify(epoch uint64, fp EpochHash) error {
	a := &EpochAttestation{Epoch: epoch, Fingerprint: fp, Stamp: c.Stamp}
	return verifyByID(c.Validator, a.msg(), c.Sig)
}

// Hash commits to everything but the signature.
func (p *EpochProposal) Hash() EpochHash {
	b := new(bytes.Buffer)
	b.WriteString(epochPropDomain)
	_ = binary.Write(b, binary.BigEndian, p.Epoch)
	b.Write(p.Prev[:])
	writeStr(b, p.Proposer)
	_ = binary.Write(b, binary.BigEndian, int32(p.Rank))
	_ = binary.Write(b, binary.BigEndian, uint16(len(p.Active)))
	for _, a := range p.Active {
		writeStr(b, a)
	}
	_ = binary.Write(b, binary.BigEndian, uint16(len(p.Credits)))
	for _, c := range p.Credits {
		writeStr(b, c.Validator)
		_ = binary.Write(b, binary.BigEndian, c.Stamp)
		_ = binary.Write(b, binary.BigEndian, uint16(len(c.Sig)))
		b.Write(c.Sig)
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(p.Orders)))
	for _, o := range p.Orders {
		id := o.ID()
		b.Write(id[:])
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(p.Cancels)))
	for _, c := range p.Cancels {
		b.Write(c.serialize())
	}
	_ = binary.Write(b, binary.BigEndian, p.Stamp)
	return sha256.Sum256(b.Bytes())
}

func (p *EpochProposal) sign(priv libp2pcrypto.PrivKey) error {
	pid, pubB, err := ownerOf(priv)
	if err != nil {
		return err
	}
	p.Proposer, p.PubKey = pid.String(), pubB
	h := p.Hash()
	p.Sig, err = priv.Sign(h[:])
	return err
}

// verify checks the signature, the form of the active list and credits, and
// the proposer's rank in the active list. Whether Active is the epoch's
// active set and the credits are valid signatures of Prev needs the chain:
// the engine checks that (checkActiveSetLocked).
func (p *EpochProposal) verify(r *Release) error {
	if !r.IsValidator(p.Proposer) {
		return errors.New("proposer is not a validator")
	}
	if err := checkActive(r, p.Active, p.Proposer); err != nil {
		return err
	}
	if rank := ProposerRankIn(p.Active, p.Epoch, p.Proposer); rank != p.Rank {
		return fmt.Errorf("wrong proposer rank %d (want %d)", p.Rank, rank)
	}
	for i, c := range p.Credits {
		if !r.IsValidator(c.Validator) {
			return fmt.Errorf("credit for non-validator %s", c.Validator)
		}
		if i > 0 && p.Credits[i-1].Validator >= c.Validator {
			return errors.New("credits not sorted or duplicated")
		}
	}
	h := p.Hash()
	return verifySig(p.Proposer, p.PubKey, h[:], p.Sig)
}

func checkActive(r *Release, active []string, proposer string) error {
	if !sort.StringsAreSorted(active) {
		return errors.New("active list not sorted")
	}
	hasProposer := false
	for i, a := range active {
		if !r.IsValidator(a) {
			return fmt.Errorf("active %s is not a validator", a)
		}
		if i > 0 && active[i-1] == a {
			return errors.New("duplicate active validator")
		}
		hasProposer = hasProposer || a == proposer
	}
	if !hasProposer {
		return errors.New("proposer not in active list")
	}
	return nil
}

// epochFingerprint is what validators sign: the epoch, the previous record,
// the proposal, the active set, the matches and the resulting state.
func epochFingerprint(epoch uint64, prev, propHash EpochHash, active []string, matches []*EpochMatch, stateHash EpochHash) EpochHash {
	b := new(bytes.Buffer)
	b.WriteString(epochFPDomain)
	_ = binary.Write(b, binary.BigEndian, epoch)
	b.Write(prev[:])
	b.Write(propHash[:])
	_ = binary.Write(b, binary.BigEndian, uint16(len(active)))
	for _, a := range active {
		writeStr(b, a)
	}
	_ = binary.Write(b, binary.BigEndian, uint32(len(matches)))
	for _, m := range matches {
		m.write(b)
	}
	b.Write(stateHash[:])
	return sha256.Sum256(b.Bytes())
}

// EpochAttestation is a validator's signature of an epoch fingerprint.
type EpochAttestation struct {
	Epoch       uint64    `json:"epoch"`
	Fingerprint EpochHash `json:"fingerprint"`
	Validator   string    `json:"validator"`
	Stamp       int64     `json:"stamp"`
	PubKey      []byte    `json:"pubkey"`
	Sig         []byte    `json:"sig"`
}

func (a *EpochAttestation) msg() []byte {
	b := new(bytes.Buffer)
	b.WriteString(epochAttDomain)
	_ = binary.Write(b, binary.BigEndian, a.Epoch)
	b.Write(a.Fingerprint[:])
	_ = binary.Write(b, binary.BigEndian, a.Stamp)
	return b.Bytes()
}

func (a *EpochAttestation) sign(priv libp2pcrypto.PrivKey) error {
	pid, pubB, err := ownerOf(priv)
	if err != nil {
		return err
	}
	a.Validator, a.PubKey = pid.String(), pubB
	a.Sig, err = priv.Sign(a.msg())
	return err
}

func (a *EpochAttestation) verify() error {
	return verifySig(a.Validator, a.PubKey, a.msg(), a.Sig)
}

// EpochHeartbeat is a validator's signed liveness and clock sample.
type EpochHeartbeat struct {
	Epoch     uint64 `json:"epoch"`
	Validator string `json:"validator"`
	Stamp     int64  `json:"stamp"`
	PubKey    []byte `json:"pubkey"`
	Sig       []byte `json:"sig"`
}

func (h *EpochHeartbeat) msg() []byte {
	b := new(bytes.Buffer)
	b.WriteString(epochHBDomain)
	_ = binary.Write(b, binary.BigEndian, h.Epoch)
	_ = binary.Write(b, binary.BigEndian, h.Stamp)
	return b.Bytes()
}

// EpochRecord is a finalized epoch: the fingerprint's inputs and at least a
// threshold of validator signatures. Proposal is omitted where only the
// matches are needed (EpochRef).
type EpochRecord struct {
	Epoch        uint64              `json:"epoch"`
	Prev         EpochHash           `json:"prev"`
	ProposalHash EpochHash           `json:"proposalHash"`
	Proposer     string              `json:"proposer"`
	Active       []string            `json:"active"`
	Matches      []*EpochMatch       `json:"matches"`
	StateHash    EpochHash           `json:"stateHash"`
	Fingerprint  EpochHash           `json:"fingerprint"`
	Sigs         []*EpochAttestation `json:"sigs"`
	Proposal     *EpochProposal      `json:"proposal,omitempty"`
}

// thresholdMet reports whether signed power meets the quorum of the active
// set's power and the active set meets the floor of all bonded power (PLAN
// S1/S3: finalize once >= 2/3 of active power signs AND active power >= 1/2
// of bonded power). The active set is deterministic (S3), so every peer
// counts the quorum against the same set.
func thresholdMet(p *EpochParams, signed, active, bonded uint64) bool {
	s, a, b := int64(signed), int64(active), int64(bonded)
	return a > 0 && s*p.QuorumDen >= p.QuorumNum*a && a*p.FloorDen >= p.FloorNum*b
}

// VerifyRecord checks a record's fingerprint and signatures against the
// release's validator set and thresholds. It does not need the epoch state.
func VerifyRecord(r *Release, rec *EpochRecord) error {
	if !r.IsValidator(rec.Proposer) {
		return errors.New("proposer is not a validator")
	}
	if err := checkActive(r, rec.Active, rec.Proposer); err != nil {
		return err
	}
	if rec.Proposal != nil && rec.Proposal.Hash() != rec.ProposalHash {
		return errors.New("proposal hash mismatch")
	}
	fp := epochFingerprint(rec.Epoch, rec.Prev, rec.ProposalHash, rec.Active, rec.Matches, rec.StateHash)
	if fp != rec.Fingerprint {
		return errors.New("fingerprint mismatch")
	}
	active := make(map[string]bool, len(rec.Active))
	var activePower uint64
	for _, a := range rec.Active {
		active[a] = true
		activePower += r.Power(a)
	}
	seen := make(map[string]bool)
	var signed uint64
	for _, a := range rec.Sigs {
		if a.Epoch != rec.Epoch || a.Fingerprint != fp || !active[a.Validator] || seen[a.Validator] {
			continue
		}
		if a.verify() != nil {
			continue
		}
		seen[a.Validator] = true
		signed += r.Power(a.Validator)
	}
	if !thresholdMet(&r.Params, signed, activePower, r.BondedPower()) {
		return fmt.Errorf("not finalized: %d valid signatures, signed power %d of active %d (bonded %d)",
			len(seen), signed, activePower, r.BondedPower())
	}
	return nil
}

// EpochRef names the finalized epoch match a proposal is for (S2). It is
// compact because inbox messages must fit a 1 KiB mesh publish: the maker
// checks it against the record in its own finalized chain, so nothing in the
// reference itself has to be trusted.
type EpochRef struct {
	Epoch       uint64    `json:"epoch"`
	Fingerprint EpochHash `json:"fp"`
	Index       int       `json:"index"`
}

// epochMatchNonce and the epoch end stamp make epoch match IDs
// deterministic.
func epochMatchNonce(epoch uint64, index int) uint64 { return epoch<<16 | uint64(index&0xffff) }

// epochMsg is the envelope on EpochTopic.
type epochMsg struct {
	Type     string            `json:"type"`
	Order    *MeshOrder        `json:"order,omitempty"`
	Cancel   *MeshCancel       `json:"cancel,omitempty"`
	HB       *EpochHeartbeat   `json:"hb,omitempty"`
	Prop     *EpochProposal    `json:"prop,omitempty"`
	Att      *EpochAttestation `json:"att,omitempty"`
	Rec      *EpochRecord      `json:"rec,omitempty"`
	Recs     []*EpochRecord    `json:"recs,omitempty"`
	SyncFrom uint64            `json:"syncFrom,omitempty"`
	To       string            `json:"to,omitempty"`
	Frag     *epochFrag        `json:"frag,omitempty"`
}

const (
	epMsgOrder   = "order"
	epMsgCancel  = "cancel"
	epMsgHB      = "hb"
	epMsgProp    = "prop"
	epMsgAtt     = "att"
	epMsgRec     = "rec"
	epMsgSyncReq = "syncreq"
	epMsgRecs    = "recs"
)
