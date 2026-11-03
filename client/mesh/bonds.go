// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S4: validator bonds behind a dormant one-way switch.
//
// The bond code ships dormant. A seeded validator (one named in the release)
// turns it on by signing a switch signal: a signature of the release hash.
// Proposers carry the signals they know into their proposals, so the signals
// end up in finalized records. Once the signals in the chain come from at
// least SwitchNum/SwitchDen (3/4) of the seeded validators, the switch is on
// for every later epoch. It is computed from the finalized chain, so every
// peer agrees on the epoch it turned on, and nothing turns it off: there is no
// "off" message, restarts reload the chain, and a node that saw the switch
// turn on keeps the signals (the proof) and re-proposes them if the chain is
// ever restarted from genesis. An optional release deadline turns the switch
// on without signals (the proposed safety fallback; 0 = none).
//
// While the switch is on, anyone can become a validator by locking a bond:
// a timelocked (CLTV) output in BTC, LBC or DCR, refundable to its owner after
// the lock and spendable by nobody before. The bond transaction is the
// standard dcrdex v0 fidelity bond (MakeBondTx); its account commitment is
// sha256("bisonary-validator-bond-v1:" + peer ID), which ties the bond to the
// validator's mesh key. The newcomer gossips a signed claim with the raw
// transaction. Validators check the transaction, check on chain that the
// output is confirmed and unspent, and proposers put verified claims into
// proposals. A validator signs a proposal with claims only after it verified
// every claim on chain itself. Records (finalized by 2/3) are accepted
// without the chain check, the same trust a trader puts in the matches.
//
// A bonded validator is a member of the validator set for epoch n when the
// switch is on, its bond was admitted in a record before n, the bond is at
// least MinAgeSecs old (counted from the admitting record, so the age is the
// same for every peer), its lock has at least MinLockLeftSecs left, and it
// meets the release's size ladder for its asset at n. Members are counted
// for the bonded power (floor) and, once they have signed enough (S3), for
// the active set and the quorum. A bonded member has power BondPower (1, the
// same as a seeded validator). Unbonded or immature nodes are not members:
// their signatures, proposals and heartbeats are ignored.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	dexbtc "decred.org/dcrdex/dex/networks/btc"
	dexdcr "decred.org/dcrdex/dex/networks/dcr"
	"github.com/btcsuite/btcd/btcutil"
	btcwire "github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/txscript/v4/stdscript"
	dcrwire "github.com/decred/dcrd/wire"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
)

// Bond asset IDs (BIP-44 coin types, as dcrdex).
const (
	BondAssetBTC uint32 = 0
	BondAssetDCR uint32 = 42
	BondAssetLBC uint32 = 140
)

// BondStep is one rung of an asset's bond size ladder: from AfterSecs after
// the switch turned on, a bond must be at least Amount atoms.
type BondStep struct {
	AfterSecs int64  `json:"afterSecs"`
	Amount    uint64 `json:"amount"`
}

// BondAsset is an accepted bond asset and its size ladder.
type BondAsset struct {
	AssetID uint32     `json:"assetID"`
	Symbol  string     `json:"symbol"`
	Steps   []BondStep `json:"steps"`
}

// BondRules are the release-defined bond rules. They are part of the signed
// release, so they change only with a new release.
type BondRules struct {
	Assets []BondAsset `json:"assets"`
	// MinAgeSecs is the bond age before it counts (PLAN: 4 weeks).
	MinAgeSecs int64 `json:"minAgeSecs"`
	// MinLockSecs: at admission the lock must have at least this left.
	MinLockSecs int64 `json:"minLockSecs"`
	// MinLockLeftSecs: a bond stops counting when less than this is left of
	// its lock (the owner can refund it after the lock).
	MinLockLeftSecs int64 `json:"minLockLeftSecs"`
	// MinConfs is the confirmations a validator needs to see before it
	// signs a proposal admitting the bond.
	MinConfs uint32 `json:"minConfs"`
	// SwitchNum/SwitchDen of the seeded validators must signal (PLAN: 3/4).
	SwitchNum int64 `json:"switchNum"`
	SwitchDen int64 `json:"switchDen"`
	// SwitchDeadline (unix s) turns the switch on even without signals
	// (proposed safety fallback). 0: no deadline.
	SwitchDeadline int64 `json:"switchDeadline,omitempty"`
	// BondPower is a bonded validator's power (seeded validators have the
	// release Bond, 1 by default).
	BondPower uint64 `json:"bondPower"`
	// MaxClaimsPerEpoch caps the bond admissions in one proposal.
	MaxClaimsPerEpoch int `json:"maxClaimsPerEpoch"`
}

// Validate checks the rules.
func (b *BondRules) Validate() error {
	switch {
	case len(b.Assets) == 0:
		return errors.New("bond rules list no assets")
	case b.MinAgeSecs < 0 || b.MinLockLeftSecs < 0:
		return errors.New("negative bond age or lock")
	case b.MinLockSecs < b.MinAgeSecs+b.MinLockLeftSecs:
		return errors.New("minLockSecs must cover minAgeSecs + minLockLeftSecs")
	case b.SwitchDen <= 0 || b.SwitchNum <= 0 || b.SwitchNum > b.SwitchDen:
		return errors.New("switch threshold must be a fraction in (0, 1]")
	case b.BondPower == 0:
		return errors.New("bondPower must be positive")
	case b.MaxClaimsPerEpoch <= 0:
		return errors.New("maxClaimsPerEpoch must be positive")
	}
	seen := make(map[uint32]bool)
	for _, a := range b.Assets {
		switch a.AssetID {
		case BondAssetBTC, BondAssetDCR, BondAssetLBC:
		default:
			return fmt.Errorf("unsupported bond asset %d", a.AssetID)
		}
		if seen[a.AssetID] {
			return fmt.Errorf("duplicate bond asset %d", a.AssetID)
		}
		seen[a.AssetID] = true
		if len(a.Steps) == 0 || a.Steps[0].AfterSecs != 0 {
			return fmt.Errorf("bond asset %s: the ladder must start at afterSecs 0", a.Symbol)
		}
		for i, s := range a.Steps {
			if s.Amount == 0 {
				return fmt.Errorf("bond asset %s: zero amount", a.Symbol)
			}
			if i > 0 && s.AfterSecs <= a.Steps[i-1].AfterSecs {
				return fmt.Errorf("bond asset %s: ladder steps not increasing", a.Symbol)
			}
		}
	}
	return nil
}

func (b *BondRules) asset(id uint32) *BondAsset {
	for i := range b.Assets {
		if b.Assets[i].AssetID == id {
			return &b.Assets[i]
		}
	}
	return nil
}

// required is the bond size for asset at sinceSwitchMs after the switch.
func (a *BondAsset) required(sinceSwitchMs int64) uint64 {
	amt := a.Steps[0].Amount
	for _, s := range a.Steps {
		if s.AfterSecs*1000 <= sinceSwitchMs {
			amt = s.Amount
		}
	}
	return amt
}

// switchNeed is the number of seeded signals that turn the switch on.
func (b *BondRules) switchNeed(seeded int) int {
	return int((int64(seeded)*b.SwitchNum + b.SwitchDen - 1) / b.SwitchDen)
}

const (
	switchSigDomain = "bisonary-bondswitch-v1:"
	bondClaimDomain = "bisonary-valbond-v1:"
	bondAcctDomain  = "bisonary-validator-bond-v1:"
	bondKeyDomain   = "bisonary-validator-bond-key-v1:"
)

// SwitchSignal is a seeded validator's signal to turn the bond switch on.
type SwitchSignal struct {
	Validator string `json:"v"`
	Stamp     int64  `json:"t"`
	Sig       []byte `json:"s"`
}

func switchSigMsg(relHash [32]byte, stamp int64) []byte {
	b := new(bytes.Buffer)
	b.WriteString(switchSigDomain)
	b.Write(relHash[:])
	_ = binary.Write(b, binary.BigEndian, stamp)
	return b.Bytes()
}

func signSwitch(priv libp2pcrypto.PrivKey, relHash [32]byte, stamp int64) (*SwitchSignal, error) {
	pid, _, err := ownerOf(priv)
	if err != nil {
		return nil, err
	}
	sig, err := priv.Sign(switchSigMsg(relHash, stamp))
	if err != nil {
		return nil, err
	}
	return &SwitchSignal{Validator: pid.String(), Stamp: stamp, Sig: sig}, nil
}

func (s *SwitchSignal) verify(rel *Release) error {
	if !rel.IsValidator(s.Validator) {
		return errors.New("switch signal from a validator that is not seeded in the release")
	}
	return verifyByID(s.Validator, switchSigMsg(rel.Hash(), s.Stamp), s.Sig)
}

// BondClaim is a validator candidate's claim of a bond.
type BondClaim struct {
	Validator string `json:"v"`
	AssetID   uint32 `json:"a"`
	CoinID    []byte `json:"c"`
	RawTx     []byte `json:"tx"`
	Stamp     int64  `json:"t"`
	Sig       []byte `json:"s"`
}

func (c *BondClaim) msg() []byte {
	b := new(bytes.Buffer)
	b.WriteString(bondClaimDomain)
	writeStr(b, c.Validator)
	_ = binary.Write(b, binary.BigEndian, c.AssetID)
	_ = binary.Write(b, binary.BigEndian, uint16(len(c.CoinID)))
	b.Write(c.CoinID)
	th := sha256.Sum256(c.RawTx)
	b.Write(th[:])
	_ = binary.Write(b, binary.BigEndian, c.Stamp)
	return b.Bytes()
}

func (c *BondClaim) sign(priv libp2pcrypto.PrivKey) error {
	pid, _, err := ownerOf(priv)
	if err != nil {
		return err
	}
	c.Validator = pid.String()
	c.Sig, err = priv.Sign(c.msg())
	return err
}

// Key identifies the bond output.
func (c *BondClaim) Key() string { return fmt.Sprintf("%d:%x", c.AssetID, c.CoinID) }

// BondAcct is the account commitment of a validator bond for peer ID id.
func BondAcct(id string) [32]byte { return sha256.Sum256([]byte(bondAcctDomain + id)) }

// bondDetails are the parsed contents of a bond transaction.
type bondDetails struct {
	Amount   uint64
	LockTime int64 // unix s
	Script   []byte
	PkScript []byte
	Acct     [32]byte
}

// parseBondTx parses a v0 bond transaction of a bond asset and checks that
// coinID is its output 0.
func parseBondTx(assetID uint32, rawTx, coinID []byte) (*bondDetails, error) {
	if len(coinID) != 36 || binary.BigEndian.Uint32(coinID[32:]) != 0 {
		return nil, errors.New("bond coin must be output 0 of the bond transaction")
	}
	switch assetID {
	case BondAssetBTC, BondAssetLBC:
		var tx btcwire.MsgTx
		if err := tx.Deserialize(bytes.NewReader(rawTx)); err != nil {
			return nil, fmt.Errorf("bad bond tx: %w", err)
		}
		h := tx.TxHash()
		if !bytes.Equal(h[:], coinID[:32]) {
			return nil, errors.New("bond tx hash does not match the coin ID")
		}
		if len(tx.TxOut) < 2 {
			return nil, errors.New("bond tx has fewer than 2 outputs")
		}
		acct, lock, pkh, err := dexbtc.ExtractBondCommitDataV0(0, tx.TxOut[1].PkScript)
		if err != nil {
			return nil, fmt.Errorf("bad bond commitment: %w", err)
		}
		script, err := dexbtc.MakeBondScript(0, lock, pkh[:])
		if err != nil {
			return nil, err
		}
		pk := tx.TxOut[0].PkScript
		sh := dexbtc.ExtractScriptHash(pk)
		var want []byte
		switch len(sh) {
		case 32:
			s := sha256.Sum256(script)
			want = s[:]
		case 20:
			want = btcutil.Hash160(script)
		default:
			return nil, errors.New("bond output is not a script hash")
		}
		if !bytes.Equal(sh, want) {
			return nil, errors.New("bond output does not pay to the committed bond script")
		}
		if tx.TxOut[0].Value <= 0 {
			return nil, errors.New("zero bond value")
		}
		return &bondDetails{Amount: uint64(tx.TxOut[0].Value), LockTime: int64(lock), Script: script, PkScript: pk, Acct: acct}, nil
	case BondAssetDCR:
		tx := dcrwire.NewMsgTx()
		if err := tx.Deserialize(bytes.NewReader(rawTx)); err != nil {
			return nil, fmt.Errorf("bad bond tx: %w", err)
		}
		h := tx.TxHash()
		if !bytes.Equal(h[:], coinID[:32]) {
			return nil, errors.New("bond tx hash does not match the coin ID")
		}
		if len(tx.TxOut) < 2 {
			return nil, errors.New("bond tx has fewer than 2 outputs")
		}
		acct, lock, pkh, err := dexdcr.ExtractBondCommitDataV0(tx.TxOut[1].Version, tx.TxOut[1].PkScript)
		if err != nil {
			return nil, fmt.Errorf("bad bond commitment: %w", err)
		}
		script, err := dexdcr.MakeBondScript(0, lock, pkh[:])
		if err != nil {
			return nil, err
		}
		out := tx.TxOut[0]
		sh := stdscript.ExtractScriptHashV0(out.PkScript)
		if out.Version != 0 || sh == nil || !bytes.Equal(sh, dcrutil.Hash160(script)) {
			return nil, errors.New("bond output does not pay to the committed bond script")
		}
		if out.Value <= 0 {
			return nil, errors.New("zero bond value")
		}
		return &bondDetails{Amount: uint64(out.Value), LockTime: int64(lock), Script: script, PkScript: out.PkScript, Acct: acct}, nil
	}
	return nil, fmt.Errorf("unsupported bond asset %d", assetID)
}

// checkClaim is the static (chain-independent) check of a claim: signature,
// transaction, commitment to the claimant and release asset.
func checkClaim(rules *BondRules, c *BondClaim) (*bondDetails, error) {
	if rules.asset(c.AssetID) == nil {
		return nil, fmt.Errorf("asset %d is not a bond asset of this release", c.AssetID)
	}
	if err := verifyByID(c.Validator, c.msg(), c.Sig); err != nil {
		return nil, fmt.Errorf("bad claim signature: %w", err)
	}
	d, err := parseBondTx(c.AssetID, c.RawTx, c.CoinID)
	if err != nil {
		return nil, err
	}
	if d.Acct != BondAcct(c.Validator) {
		return nil, errors.New("bond commits to another account than the claimant's validator key")
	}
	return d, nil
}

// ---------------- ledger: bond state from the finalized chain ----------------

type ledgerBond struct {
	claim *BondClaim
	d     *bondDetails
	epoch uint64 // admitting record
}

// bondLedger is the S4 state derived from the current chain's records.
type bondLedger struct {
	rel       *Release
	signals   map[string]uint64 // seeded validator -> epoch of the record with its signal
	sigs      map[string]*SwitchSignal
	switchRec uint64 // record whose signals reached the threshold
	switchSet bool
	bonds     []*ledgerBond
	coins     map[string]bool
	bans      map[string]*ledgerBan // S5 (equivocation.go)
	// S6 trading bonds (userbonds.go)
	ubonds   []*ledgerUserBond
	ubondsBy map[string][]*ledgerUserBond
	ucoins   map[string]bool
	// S7 verified price history (pricehist.go)
	matches map[string]*ledgerMatch // recent epoch matches by tradeKey
	tcoins  map[string]bool         // contract coins counted
	tkeys   map[string]bool         // admitted epoch matches
	vtrades map[Market][]*VerifiedTrade
	vcount  int
}

func newBondLedger(rel *Release) *bondLedger {
	return &bondLedger{rel: rel, signals: make(map[string]uint64), sigs: make(map[string]*SwitchSignal), coins: make(map[string]bool),
		bans: make(map[string]*ledgerBan), ubondsBy: make(map[string][]*ledgerUserBond), ucoins: make(map[string]bool),
		matches: make(map[string]*ledgerMatch), tcoins: make(map[string]bool), tkeys: make(map[string]bool), vtrades: make(map[Market][]*VerifiedTrade)}
}

func (l *bondLedger) rules() *BondRules { return l.rel.Bonds }

// apply adds a finalized record's signals and admissions.
func (l *bondLedger) apply(rec *EpochRecord) {
	l.applyUserBonds(rec) // S6
	l.applyTrades(rec)    // S7
	if l.rules() == nil || rec.Proposal == nil {
		return
	}
	for _, s := range rec.Proposal.Signals {
		if _, have := l.signals[s.Validator]; !have {
			l.signals[s.Validator] = rec.Epoch
			l.sigs[s.Validator] = s
		}
	}
	if !l.switchSet && len(l.signals) >= l.rules().switchNeed(len(l.rel.Validators)) {
		l.switchSet, l.switchRec = true, rec.Epoch
	}
	for _, c := range rec.Proposal.Bonds {
		d, err := checkClaim(l.rules(), c)
		if err != nil || l.coins[c.Key()] {
			continue // a finalized record passed the same check; defensive
		}
		l.coins[c.Key()] = true
		l.bonds = append(l.bonds, &ledgerBond{claim: c, d: d, epoch: rec.Epoch})
	}
}

// switchStart is the first epoch the switch is on, considering only records
// up to asOf (inclusive).
func (l *bondLedger) switchStart(asOf uint64) (uint64, bool) {
	r := l.rules()
	if r == nil {
		return 0, false
	}
	var start uint64
	on := false
	if l.switchSet && l.switchRec <= asOf {
		start, on = l.switchRec+1, true
	}
	if r.SwitchDeadline > 0 {
		p := &l.rel.Params
		d := uint64((r.SwitchDeadline*1000 + p.EpochMs - 1) / p.EpochMs) // first epoch starting at or after it
		if !on || d < start {
			start, on = d, true
		}
	}
	return start, on
}

// switchOn: the switch is on for epoch n (records before n).
func (l *bondLedger) switchOn(n, asOf uint64) bool {
	if n > 0 && asOf >= n {
		asOf = n - 1
	}
	start, on := l.switchStart(asOf)
	return on && n >= start
}

// requiredAt is the ladder amount of asset for epoch n.
func (l *bondLedger) requiredAt(a *BondAsset, n, asOf uint64) uint64 {
	start, _ := l.switchStart(asOf)
	since := (int64(n) - int64(start)) * l.rel.Params.EpochMs
	return a.required(since)
}

// matureEpoch is the first epoch a bond admitted in record epoch a counts.
func (l *bondLedger) matureEpoch(a uint64) uint64 {
	p := &l.rel.Params
	t := p.EpochEnd(a) + l.rules().MinAgeSecs*1000
	m := uint64((t + p.EpochMs - 1) / p.EpochMs)
	if m <= a {
		m = a + 1
	}
	return m
}

// bondCounts: bond b makes its owner a member for epoch n.
func (l *bondLedger) bondCounts(b *ledgerBond, n, asOf uint64) bool {
	r := l.rules()
	if b.epoch >= n || b.epoch > asOf || n < l.matureEpoch(b.epoch) || !l.switchOn(n, asOf) {
		return false
	}
	if b.d.LockTime*1000-int64(n)*l.rel.Params.EpochMs < r.MinLockLeftSecs*1000 {
		return false
	}
	a := r.asset(b.claim.AssetID)
	return a != nil && b.d.Amount >= l.requiredAt(a, n, asOf)
}

// valMember is a member of the validator set of an epoch.
type valMember struct {
	PeerID string
	Power  uint64
	Seeded bool
	Since  uint64 // first epoch of the current membership
	Bond   *ledgerBond
}

// valSet is the validator set of an epoch: the seeded validators and, with
// the bond switch on, the bonded members.
type valSet struct {
	rel     *Release
	Epoch   uint64
	Switch  bool
	members map[string]*valMember
	ids     []string
	total   uint64
}

// members is the validator set for epoch n from records up to asOf.
func (l *bondLedger) members(n, asOf uint64) *valSet {
	vs := &valSet{rel: l.rel, Epoch: n, members: make(map[string]*valMember)}
	for _, v := range l.rel.Validators {
		if l.bannedAt(v.PeerID, n, asOf) {
			continue // S5
		}
		vs.members[v.PeerID] = &valMember{PeerID: v.PeerID, Power: v.Bond, Seeded: true}
	}
	if r := l.rules(); r != nil && l.switchOn(n, asOf) {
		vs.Switch = true
		for _, b := range l.bonds {
			if !l.bondCounts(b, n, asOf) {
				continue
			}
			id := b.claim.Validator
			if l.bannedAt(id, n, asOf) {
				continue // S5
			}
			since := l.matureEpoch(b.epoch)
			if m := vs.members[id]; m != nil {
				if !m.Seeded && since < m.Since {
					m.Since, m.Bond = since, b
				}
				continue
			}
			vs.members[id] = &valMember{PeerID: id, Power: r.BondPower, Since: since, Bond: b}
		}
	}
	for id, m := range vs.members {
		vs.ids = append(vs.ids, id)
		vs.total += m.Power
	}
	sort.Strings(vs.ids)
	return vs
}

func (v *valSet) IsValidator(id string) bool { return v.members[id] != nil }

func (v *valSet) Power(id string) uint64 {
	if m := v.members[id]; m != nil {
		return m.Power
	}
	return 0
}

func (v *valSet) BondedPower() uint64 { return v.total }
func (v *valSet) IDs() []string       { return append([]string(nil), v.ids...) }

func (v *valSet) since(id string) uint64 {
	if m := v.members[id]; m != nil {
		return m.Since
	}
	return 0
}

// seededSet is the validator set without bonds (S0-S3 behaviour).
func seededSet(rel *Release) *valSet { return newBondLedger(rel).members(0, 0) }

// ---------------- switch latch file ----------------

// bondLatch is the on-disk proof that the switch turned on: the signals.
type bondLatch struct {
	Release string          `json:"release"`
	Epoch   uint64          `json:"switchOnEpoch"`
	Signals []*SwitchSignal `json:"signals"`
}

func writeLatch(path string, rel *Release, epoch uint64, sigs []*SwitchSignal) error {
	h := rel.Hash()
	b, err := json.MarshalIndent(&bondLatch{Release: hex.EncodeToString(h[:]), Epoch: epoch, Signals: sigs}, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readLatch(path string, rel *Release) (*bondLatch, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l bondLatch
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	h := rel.Hash()
	if l.Release != hex.EncodeToString(h[:]) {
		return nil, errors.New("latch is for another release")
	}
	var good []*SwitchSignal
	for _, s := range l.Signals {
		if s.verify(rel) == nil {
			good = append(good, s)
		}
	}
	l.Signals = good
	return &l, nil
}

// bondKey derives the secp256k1 bond key for asset and index from the mesh
// key (so it is recoverable from the app seed).
func bondKeyBytes(priv libp2pcrypto.PrivKey, assetID uint32, index uint32) ([]byte, error) {
	raw, err := priv.Raw()
	if err != nil {
		return nil, err
	}
	b := new(bytes.Buffer)
	b.WriteString(bondKeyDomain)
	b.Write(raw)
	_ = binary.Write(b, binary.BigEndian, assetID)
	_ = binary.Write(b, binary.BigEndian, index)
	k := sha256.Sum256(b.Bytes())
	return k[:], nil
}

const (
	secsPerDay  = 86_400
	secsPerYear = 365 * secsPerDay
)

// DefaultBondRules are the PROPOSED mainnet bond rules (PLAN, Rouse
// 2026-11-02). Every value is release-defined; the DCR size and the BTC
// ladder's later rungs (0.001 / 0.01 / 0.1 BTC) and their timing are not
// decided, so the defaults have only the first BTC rung and a DCR
// placeholder.
var DefaultBondRules = BondRules{
	Assets: []BondAsset{
		{AssetID: BondAssetBTC, Symbol: "btc", Steps: []BondStep{{0, 10_000}}}, // 0.0001 BTC
		{AssetID: BondAssetLBC, Symbol: "lbc", Steps: []BondStep{ // 1,000,000 / 500,000 / 100,000 LBC
			{0, 1_000_000 * 1e8}, {secsPerYear, 500_000 * 1e8}, {2 * secsPerYear, 100_000 * 1e8}}},
		{AssetID: BondAssetDCR, Symbol: "dcr", Steps: []BondStep{{0, 10 * 1e8}}}, // placeholder, undecided
	},
	MinAgeSecs:        28 * secsPerDay,  // 4 weeks
	MinLockSecs:       182 * secsPerDay, // ~6 months
	MinLockLeftSecs:   7 * secsPerDay,
	MinConfs:          2,
	SwitchNum:         3,
	SwitchDen:         4,
	BondPower:         1,
	MaxClaimsPerEpoch: 8,
}
