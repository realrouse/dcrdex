// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S0: the signed release file. A release names the validator set and the
// epoch consensus parameters. It is signed with the developer release key
// (ed25519) and a client refuses to run epoch trading with a release whose
// signature does not verify. The parameters are network-wide, so they live
// in the release rather than in local config: change them by issuing (and
// signing) a new release.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// DevReleasePubKey is the bisonary developer release key (ed25519, hex). The
// private key is kept offline (not in the repository).
const DevReleasePubKey = "0efbe8636e730e6ebabd3a06807f9a4423a6ac28d5478d0a268b6f5c741bea4e"

// ReleaseVersion is the release file format version.
const ReleaseVersion = 1

// EpochParams are the epoch consensus parameters. All are configurable via
// the release file; DefaultEpochParams are the proposed defaults.
type EpochParams struct {
	// EpochMs is the epoch length. Epoch N covers unix ms [N*EpochMs, (N+1)*EpochMs).
	EpochMs int64 `json:"epochMs"`
	// SignWindowMs: validators only sign a proposal for epoch N that they
	// receive within SignWindowMs after the end of N.
	SignWindowMs int64 `json:"signWindowMs"`
	// ProposeDelayMs is how long after the epoch end the proposer waits for
	// late order messages before proposing.
	ProposeDelayMs int64 `json:"proposeDelayMs"`
	// FallbackMs: if the scheduled proposer is silent, the next validator in
	// rotation may propose FallbackMs later (and so on).
	FallbackMs int64 `json:"fallbackMs"`
	// QuorumNum/QuorumDen: a record is final when the signing validators
	// hold at least QuorumNum/QuorumDen of the active validators' power.
	QuorumNum int64 `json:"quorumNum"`
	QuorumDen int64 `json:"quorumDen"`
	// FloorNum/FloorDen: no finalization unless the active set's power is at
	// least FloorNum/FloorDen of the total bonded power.
	FloorNum int64 `json:"floorNum"`
	FloorDen int64 `json:"floorDen"`
	// MaxClockSkewMs: a node whose clock differs from the mesh by more than
	// this warns and stops signing (orders, proposals and attestations).
	// Order stamps must also be within this of the receiver's clock.
	MaxClockSkewMs int64 `json:"maxClockSkewMs"`
	// ActiveEpochs: a validator's messages within this many epochs count
	// for the clock-skew estimate and the "last seen" status.
	ActiveEpochs int64 `json:"activeEpochs"`
	// S3 active set: a validator is active for epoch N if it signed (is
	// credited in) at least ActivePct % of the finalized epochs in the
	// window of ActiveWindowEpochs epochs ending ActiveLagEpochs before N.
	// With fewer than ActiveMinEpochs finalized epochs in the window (new
	// chain, or a long halt) every release validator is active.
	ActiveWindowEpochs int64 `json:"activeWindowEpochs"`
	ActiveLagEpochs    int64 `json:"activeLagEpochs"`
	ActivePct          int64 `json:"activePct"`
	ActiveMinEpochs    int64 `json:"activeMinEpochs"`
	// OmissionGraceMs: an order received this long before the epoch end
	// that the proposal leaves out is flagged as a proposer omission.
	OmissionGraceMs int64 `json:"omissionGraceMs"`
	// MaxOrdersPerEpoch caps the orders (and cancels) in one proposal.
	MaxOrdersPerEpoch int `json:"maxOrdersPerEpoch"`
}

// DefaultEpochParams are the proposed defaults: 10 s epochs, 5 s signing
// window, 2/3 of active power with a half-of-bonded floor, 2 s clock limit.
var DefaultEpochParams = EpochParams{
	EpochMs:           10_000,
	SignWindowMs:      5_000,
	ProposeDelayMs:    300,
	FallbackMs:        1_500,
	QuorumNum:         2,
	QuorumDen:         3,
	FloorNum:          1,
	FloorDen:          2,
	MaxClockSkewMs:    2_000,
	ActiveEpochs:      3,
	OmissionGraceMs:   1_500,
	MaxOrdersPerEpoch: 500,
	// 24 h of 10 s epochs, ending 100 epochs back; 80 %.
	ActiveWindowEpochs: 8_640,
	ActiveLagEpochs:    100,
	ActivePct:          80,
	ActiveMinEpochs:    864,
}

// Validate checks the parameters for consistency.
func (p *EpochParams) Validate() error {
	switch {
	case p.EpochMs < 500:
		return errors.New("epochMs must be at least 500")
	case p.SignWindowMs <= 0 || p.SignWindowMs >= p.EpochMs:
		return errors.New("signWindowMs must be in (0, epochMs)")
	case p.ProposeDelayMs < 0 || p.ProposeDelayMs >= p.SignWindowMs:
		return errors.New("proposeDelayMs must be in [0, signWindowMs)")
	case p.FallbackMs <= 0:
		return errors.New("fallbackMs must be positive")
	case p.QuorumDen <= 0 || p.QuorumNum <= 0 || p.QuorumNum > p.QuorumDen:
		return errors.New("quorum must be a fraction in (0, 1]")
	case p.FloorDen <= 0 || p.FloorNum < 0 || p.FloorNum >= p.FloorDen:
		return errors.New("floor must be a fraction in [0, 1)")
	case p.MaxClockSkewMs <= 0:
		return errors.New("maxClockSkewMs must be positive")
	case p.ActiveEpochs <= 0:
		return errors.New("activeEpochs must be positive")
	case p.OmissionGraceMs < 0:
		return errors.New("omissionGraceMs must not be negative")
	case p.MaxOrdersPerEpoch <= 0:
		return errors.New("maxOrdersPerEpoch must be positive")
	case p.ActiveWindowEpochs <= 0 || p.ActiveWindowEpochs > 100_000:
		return errors.New("activeWindowEpochs must be in (0, 100000]")
	case p.ActiveLagEpochs < 1:
		return errors.New("activeLagEpochs must be at least 1")
	case p.ActivePct <= 0 || p.ActivePct > 100:
		return errors.New("activePct must be in (0, 100]")
	case p.ActiveMinEpochs <= 0 || p.ActiveMinEpochs > p.ActiveWindowEpochs:
		return errors.New("activeMinEpochs must be in (0, activeWindowEpochs]")
	}
	return nil
}

// EpochOf is the epoch containing unix ms t.
func (p *EpochParams) EpochOf(tMs int64) uint64 {
	if tMs < 0 {
		return 0
	}
	return uint64(tMs / p.EpochMs)
}

// EpochEnd is the end (exclusive) of epoch n in unix ms.
func (p *EpochParams) EpochEnd(n uint64) int64 { return int64(n+1) * p.EpochMs }

// ReleaseValidator is a validator node in a release. Bond is its power
// (bonded amount in a common unit; 1 each until S3 wires real bonds).
type ReleaseValidator struct {
	PeerID string `json:"peerID"`
	Name   string `json:"name,omitempty"`
	Bond   uint64 `json:"bond"`
}

// Release is the content of a signed release file.
type Release struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	Network  string `json:"network"`
	Issued   int64  `json:"issued"` // unix s
	Protocol uint32 `json:"protocol"`
	// MinProtocol is the oldest protocol version peers may run.
	MinProtocol uint32             `json:"minProtocol"`
	Validators  []ReleaseValidator `json:"validators"`
	Params      EpochParams        `json:"params"`

	hash [32]byte
	vals map[string]*ReleaseValidator
	ids  []string // sorted validator peer IDs (proposer rotation order)
}

// SignedRelease is the release file: the release JSON exactly as signed.
type SignedRelease struct {
	Release json.RawMessage `json:"release"`
	Key     string          `json:"key"` // hex ed25519 public key
	Sig     string          `json:"sig"` // hex ed25519 signature of Release
}

const releaseSigDomain = "bisonary-release-v1:"

func releaseSigMsg(raw []byte) []byte {
	return append([]byte(releaseSigDomain), raw...)
}

// SignRelease signs a release with an ed25519 key.
func SignRelease(r *Release, priv ed25519.PrivateKey) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(r, "  ", "  ")
	if err != nil {
		return nil, err
	}
	sr := &SignedRelease{
		Release: raw,
		Key:     hex.EncodeToString(priv.Public().(ed25519.PublicKey)),
		Sig:     hex.EncodeToString(ed25519.Sign(priv, releaseSigMsg(raw))),
	}
	return json.MarshalIndent(sr, "", "  ")
}

// ParseRelease verifies a signed release file against the hex ed25519
// public key trustedKey and parses it.
func ParseRelease(b []byte, trustedKey string) (*Release, error) {
	var sr SignedRelease
	if err := json.Unmarshal(b, &sr); err != nil {
		return nil, fmt.Errorf("bad release file: %w", err)
	}
	if sr.Key != trustedKey {
		return nil, fmt.Errorf("release is signed by an untrusted key %q", sr.Key)
	}
	pub, err := hex.DecodeString(trustedKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("bad trusted release key")
	}
	sig, err := hex.DecodeString(sr.Sig)
	if err != nil {
		return nil, errors.New("bad release signature encoding")
	}
	if !ed25519.Verify(pub, releaseSigMsg(sr.Release), sig) {
		return nil, errors.New("release signature is invalid (file tampered with or not signed by the release key)")
	}
	var r Release
	if err := json.Unmarshal(sr.Release, &r); err != nil {
		return nil, fmt.Errorf("bad release content: %w", err)
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	r.hash = sha256.Sum256(sr.Release)
	return &r, nil
}

// LoadRelease reads and verifies a release file with the developer key.
func LoadRelease(path string) (*Release, error) {
	return LoadReleaseWithKey(path, DevReleasePubKey)
}

// LoadReleaseWithKey reads and verifies a release file with trustedKey.
func LoadReleaseWithKey(path, trustedKey string) (*Release, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRelease(b, trustedKey)
}

func (r *Release) check() error {
	if r.Version != ReleaseVersion {
		return fmt.Errorf("unsupported release version %d", r.Version)
	}
	if r.Protocol < ProtocolVersion || r.MinProtocol > ProtocolVersion {
		return fmt.Errorf("release requires protocol %d..%d, this client speaks %d", r.MinProtocol, r.Protocol, ProtocolVersion)
	}
	if len(r.Validators) == 0 {
		return errors.New("release has no validators")
	}
	if err := r.Params.Validate(); err != nil {
		return fmt.Errorf("release params: %w", err)
	}
	r.vals = make(map[string]*ReleaseValidator, len(r.Validators))
	r.ids = make([]string, 0, len(r.Validators))
	for i := range r.Validators {
		v := &r.Validators[i]
		if _, err := peer.Decode(v.PeerID); err != nil {
			return fmt.Errorf("validator %d: bad peer ID: %w", i, err)
		}
		if v.Bond == 0 {
			return fmt.Errorf("validator %s has no bond", v.PeerID)
		}
		if _, dup := r.vals[v.PeerID]; dup {
			return fmt.Errorf("duplicate validator %s", v.PeerID)
		}
		r.vals[v.PeerID] = v
		r.ids = append(r.ids, v.PeerID)
	}
	sort.Strings(r.ids)
	return nil
}

// Hash is the sha256 of the signed release content.
func (r *Release) Hash() [32]byte { return r.hash }

// IsValidator is true if id is a validator of the release.
func (r *Release) IsValidator(id string) bool { _, ok := r.vals[id]; return ok }

// Power is a validator's bond power (0 if not a validator).
func (r *Release) Power(id string) uint64 {
	if v := r.vals[id]; v != nil {
		return v.Bond
	}
	return 0
}

// BondedPower is the total validator power.
func (r *Release) BondedPower() (p uint64) {
	for _, v := range r.Validators {
		p += v.Bond
	}
	return
}

// ValidatorIDs are the validator peer IDs in rotation order.
func (r *Release) ValidatorIDs() []string { return append([]string(nil), r.ids...) }

// Proposer is the validator at rank (0: scheduled, 1..: fallbacks) for epoch n.
func (r *Release) Proposer(n uint64, rank int) string {
	k := len(r.ids)
	return r.ids[(int(n%uint64(k))+rank)%k]
}

// ProposerRank is id's fallback rank for epoch n, or -1.
func (r *Release) ProposerRank(n uint64, id string) int {
	k := len(r.ids)
	for i, v := range r.ids {
		if v == id {
			return ((i-int(n%uint64(k)))%k + k) % k
		}
	}
	return -1
}

// ProposerRankIn is id's proposer rank for epoch n in the sorted active
// set (0: scheduled, 1..: fallbacks), or -1 if id is not in it. PLAN S1:
// proposer = active set[N mod n].
func ProposerRankIn(active []string, n uint64, id string) int {
	k := len(active)
	for i, v := range active {
		if v == id {
			return ((i-int(n%uint64(k)))%k + k) % k
		}
	}
	return -1
}

// ValidatorName is a validator's display name or a short peer ID.
func (r *Release) ValidatorName(id string) string {
	if v := r.vals[id]; v != nil && v.Name != "" {
		return v.Name
	}
	if len(id) > 12 {
		return id[len(id)-8:]
	}
	return id
}

// EpochDuration is the epoch length.
func (r *Release) EpochDuration() time.Duration {
	return time.Duration(r.Params.EpochMs) * time.Millisecond
}
