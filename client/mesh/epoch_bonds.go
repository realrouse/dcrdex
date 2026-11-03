// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S4 engine side: the validator set per epoch, switch signals, bond claims,
// their on-chain checks and the switch latch (see bonds.go).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

// BondChain looks up a bond output on chain (implemented by Core with the
// asset wallets). since is a time before the bond transaction was mined.
type BondChain interface {
	BondConfs(ctx context.Context, assetID uint32, coinID, bondScript []byte, since time.Time) (confs uint32, spent bool, err error)
}

// BondMaker makes and (if broadcast) sends a v0 bond transaction (Core).
type BondMaker interface {
	MakeValidatorBond(ctx context.Context, assetID uint32, amt uint64, lockTime time.Time, bondKey []byte, acct [32]byte, broadcast bool) (coinID, rawTx []byte, err error)
}

type bondPoolEntry struct {
	claim    *BondClaim
	d        *bondDetails
	recv     int64
	verified bool
	confs    uint32
	checking bool
	checkAt  int64
	err      string
}

// SetBondHooks sets the chain lookup and bond maker.
func (e *EpochEngine) SetBondHooks(chain BondChain, maker BondMaker) {
	e.mtx.Lock()
	e.bondChain, e.bondMaker = chain, maker
	e.mtx.Unlock()
}

func (e *EpochEngine) bondsEnabled() bool { return e.rel.Bonds != nil }

func (e *EpochEngine) latchPath() string {
	if e.cfg.StorePath == "" {
		return ""
	}
	return e.cfg.StorePath + ".bondswitch.json"
}

func (e *EpochEngine) signalPath() string {
	if e.cfg.StorePath == "" {
		return ""
	}
	return e.cfg.StorePath + ".bondsignal"
}

// loadBondFiles loads the latch (switch proof) and our own pending signal.
func (e *EpochEngine) loadBondFiles() {
	if !e.bondsEnabled() {
		return
	}
	if p := e.latchPath(); p != "" {
		if l, err := readLatch(p, e.rel); err == nil {
			e.latched = true
			e.latchSigs = l.Signals
			for _, s := range l.Signals {
				if _, have := e.sigPool[s.Validator]; !have {
					e.sigPool[s.Validator] = s
				}
			}
			e.log.Infof("Bond switch latch: on since epoch %d (%d signals kept as proof)", l.Epoch, len(l.Signals))
		} else if !errors.Is(err, os.ErrNotExist) {
			e.log.Warnf("Bond switch latch %s: %v", p, err)
		}
	}
	if p := e.signalPath(); p != "" && e.rel.IsValidator(e.self) {
		if _, err := os.Stat(p); err == nil {
			e.signalOwnLocked()
		}
	}
}

// valsLocked is the validator set for epoch n on top of our chain.
func (e *EpochEngine) valsLocked(n uint64) *valSet {
	if c := e.valsCache; c != nil && c.Epoch == n && e.valsCacheFP == e.lastFP {
		return c
	}
	asof, _ := e.lastEpochLocked()
	vs := e.ledger.members(n, asof)
	e.valsCache, e.valsCacheFP = vs, e.lastFP
	return vs
}

// isMemberLocked: we are in the validator set of epoch n.
func (e *EpochEngine) isMemberLocked(n uint64) bool {
	return e.signer && e.valsLocked(n).IsValidator(e.self)
}

// SignalBondSwitch makes this seeded validator signal the bond switch. The
// signal is kept (and re-sent) until a finalized record carries it.
func (e *EpochEngine) SignalBondSwitch() error {
	if !e.bondsEnabled() {
		return errors.New("this release has no bond rules")
	}
	if !e.rel.IsValidator(e.self) {
		return errors.New("only seeded validators signal the bond switch")
	}
	e.mtx.Lock()
	defer e.mtx.Unlock()
	if p := e.signalPath(); p != "" {
		_ = os.WriteFile(p, []byte("signal\n"), 0o600)
	}
	return e.signalOwnLocked()
}

func (e *EpochEngine) signalOwnLocked() error {
	if s := e.sigPool[e.self]; s != nil {
		return nil
	}
	s, err := signSwitch(e.priv, e.rel.Hash(), e.nowMs())
	if err != nil {
		return err
	}
	e.sigPool[e.self] = s
	e.ownSignal = s
	e.log.Infof("Signalling the bond switch")
	return nil
}

// PostBond makes, broadcasts and claims a validator bond (fake: make a
// valid-looking bond transaction but never broadcast it, simnet tests).
func (e *EpochEngine) PostBond(ctx context.Context, assetID uint32, amt uint64, lockSecs int64, fake bool) (*BondClaim, error) {
	r := e.rel.Bonds
	if r == nil {
		return nil, errors.New("this release has no bond rules")
	}
	if r.asset(assetID) == nil {
		return nil, fmt.Errorf("asset %d is not a bond asset of this release", assetID)
	}
	e.mtx.Lock()
	maker := e.bondMaker
	idx := e.bondIndex
	e.bondIndex++
	e.mtx.Unlock()
	if maker == nil {
		return nil, errors.New("no bond maker (wallets not available)")
	}
	key, err := bondKeyBytes(e.priv, assetID, uint32(time.Now().Unix())^idx)
	if err != nil {
		return nil, err
	}
	lock := e.now().Add(time.Duration(lockSecs) * time.Second)
	coinID, raw, err := maker.MakeValidatorBond(ctx, assetID, amt, lock, key, BondAcct(e.self), !fake)
	if err != nil {
		return nil, err
	}
	c := &BondClaim{AssetID: assetID, CoinID: coinID, RawTx: raw, Stamp: e.nowMs()}
	if err := c.sign(e.priv); err != nil {
		return nil, err
	}
	if _, err := checkClaim(r, c); err != nil {
		return nil, fmt.Errorf("own bond fails the check: %w", err)
	}
	e.mtx.Lock()
	e.ownClaims[c.Key()] = c
	e.addClaimLocked(c)
	e.mtx.Unlock()
	e.send(ctx, &epochMsg{Type: epMsgBond, Bond: c})
	e.log.Infof("Posted validator bond %s: asset %d, %d atoms, lock until %s (broadcast %v)", c.Key(), assetID, amt, lock.Format(time.RFC3339), !fake)
	return c, nil
}

// SubmitClaim adds an arbitrary claim (simnet tests: a forged claim).
func (e *EpochEngine) SubmitClaim(ctx context.Context, c *BondClaim) {
	e.mtx.Lock()
	e.addClaimLocked(c)
	e.mtx.Unlock()
	e.send(ctx, &epochMsg{Type: epMsgBond, Bond: c})
}

func (e *EpochEngine) addClaimLocked(c *BondClaim) {
	if e.rel.Bonds == nil || e.ledger.coins[c.Key()] {
		return
	}
	if _, have := e.bondPool[c.Key()]; have {
		return
	}
	if len(e.bondPool) >= 1000 {
		return
	}
	d, err := checkClaim(e.rel.Bonds, c)
	if err != nil {
		e.log.Debugf("Rejected bond claim %s from %s: %v", c.Key(), c.Validator, err)
		return
	}
	e.bondPool[c.Key()] = &bondPoolEntry{claim: c, d: d, recv: e.nowMs()}
}

func (e *EpochEngine) handleSignal(s *SwitchSignal) {
	if !e.bondsEnabled() || s.verify(e.rel) != nil {
		return
	}
	e.mtx.Lock()
	if _, have := e.sigPool[s.Validator]; !have {
		e.sigPool[s.Validator] = s
	}
	e.mtx.Unlock()
}

// bondChecks starts on-chain checks of pool claims (signers only).
func (e *EpochEngine) bondChecks(ctx context.Context, nowMs int64) {
	if !e.signer || !e.bondsEnabled() {
		return
	}
	e.mtx.Lock()
	chain := e.bondChain
	var todo []*bondPoolEntry
	for _, pe := range e.bondPool {
		if pe.verified || pe.checking || nowMs < pe.checkAt {
			continue
		}
		pe.checking = true
		todo = append(todo, pe)
	}
	e.mtx.Unlock()
	if chain == nil {
		e.mtx.Lock()
		for _, pe := range todo {
			pe.checking, pe.err, pe.checkAt = false, "no wallet for bond checks", nowMs+10_000
		}
		e.mtx.Unlock()
		return
	}
	for _, pe := range todo {
		go func(pe *bondPoolEntry) {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			since := time.UnixMilli(pe.claim.Stamp).Add(-2 * time.Hour)
			confs, spent, err := chain.BondConfs(cctx, pe.claim.AssetID, pe.claim.CoinID, pe.d.Script, since)
			e.mtx.Lock()
			defer e.mtx.Unlock()
			pe.checking = false
			pe.confs = confs
			pe.checkAt = e.nowMs() + e.p.EpochMs
			switch {
			case err != nil:
				pe.err = err.Error()
				pe.checkAt = e.nowMs() + 3*e.p.EpochMs
			case spent:
				pe.err = "bond output is spent"
			case confs < e.rel.Bonds.MinConfs:
				pe.err = fmt.Sprintf("%d of %d confirmations", confs, e.rel.Bonds.MinConfs)
			default:
				if !pe.verified {
					e.log.Infof("Verified validator bond %s of %s on chain (%d confs, %d atoms)", pe.claim.Key(), e.rel.ValidatorName(pe.claim.Validator), confs, pe.d.Amount)
				}
				pe.verified, pe.err = true, ""
			}
		}(pe)
	}
}

// checkS4Locked is the deterministic check of a proposal's signals and bond
// admissions against our chain.
func (e *EpochEngine) checkS4Locked(p *EpochProposal) error {
	if len(p.Signals) == 0 && len(p.Bonds) == 0 {
		return nil
	}
	if e.rel.Bonds == nil {
		return errors.New("switch signals or bonds in a release without bond rules")
	}
	for _, s := range p.Signals {
		if err := s.verify(e.rel); err != nil {
			return fmt.Errorf("bad switch signal: %v", err)
		}
		if _, have := e.ledger.signals[s.Validator]; have {
			return fmt.Errorf("switch signal of %s is already in the chain", e.rel.ValidatorName(s.Validator))
		}
	}
	asof, _ := e.lastEpochLocked()
	if len(p.Bonds) > 0 && !e.ledger.switchOn(p.Epoch, asof) {
		return errors.New("bond admissions while the bond switch is off")
	}
	r := e.rel.Bonds
	for _, c := range p.Bonds {
		if err := e.admissibleLocked(c, p.Epoch); err != nil {
			return fmt.Errorf("bond %s: %v", c.Key(), err)
		}
		_ = r
	}
	return nil
}

// admissibleLocked: claim c may be admitted in epoch n.
func (e *EpochEngine) admissibleLocked(c *BondClaim, n uint64) error {
	r := e.rel.Bonds
	d, err := checkClaim(r, c)
	if err != nil {
		return err
	}
	if e.ledger.coins[c.Key()] {
		return errors.New("already admitted")
	}
	if e.ledger.bans[c.Validator] != nil {
		return errors.New("the validator is banned (S5)")
	}
	asof, _ := e.lastEpochLocked()
	if need := e.ledger.requiredAt(r.asset(c.AssetID), n, asof); d.Amount < need {
		return fmt.Errorf("bond of %d atoms is below the release size %d", d.Amount, need)
	}
	if left := d.LockTime*1000 - e.p.EpochEnd(n); left < r.MinLockSecs*1000 {
		return fmt.Errorf("lock has %d s left, the release needs %d s", left/1000, r.MinLockSecs)
	}
	return nil
}

// bondsVerifiedLocked: we verified every bond of p on chain ourselves (to
// sign). Missing ones are queued for a check.
func (e *EpochEngine) bondsVerifiedLocked(p *EpochProposal) bool {
	ok := true
	for _, c := range p.Bonds {
		pe := e.bondPool[c.Key()]
		if pe == nil {
			e.addClaimLocked(c)
			ok = false
			continue
		}
		if !pe.verified {
			ok = false
		}
	}
	return ok
}

// latchOKLocked: a node that saw the switch turn on only signs proposals
// that keep it on: if our chain has the switch off (validators restarted
// the chain), the proposal must carry the signals we hold.
func (e *EpochEngine) latchOKLocked(p *EpochProposal) bool {
	if !e.latched {
		return true
	}
	asof, _ := e.lastEpochLocked()
	if e.ledger.switchOn(p.Epoch, asof) || e.ledger.switchSet {
		return true
	}
	in := make(map[string]bool)
	for _, s := range p.Signals {
		in[s.Validator] = true
	}
	for _, s := range e.latchSigs {
		if _, have := e.ledger.signals[s.Validator]; !have && !in[s.Validator] {
			return false
		}
	}
	return true
}

// s4ProposalLocked fills a new proposal's signals and bond admissions.
func (e *EpochEngine) s4ProposalLocked(p *EpochProposal) {
	if e.rel.Bonds == nil {
		return
	}
	for v, s := range e.sigPool {
		if _, have := e.ledger.signals[v]; !have {
			p.Signals = append(p.Signals, s)
		}
	}
	sort.Slice(p.Signals, func(i, j int) bool { return p.Signals[i].Validator < p.Signals[j].Validator })
	asof, _ := e.lastEpochLocked()
	if !e.ledger.switchOn(p.Epoch, asof) {
		return
	}
	var keys []string
	for k, pe := range e.bondPool {
		if pe.verified && e.admissibleLocked(pe.claim, p.Epoch) == nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > e.rel.Bonds.MaxClaimsPerEpoch {
		keys = keys[:e.rel.Bonds.MaxClaimsPerEpoch]
	}
	for _, k := range keys {
		p.Bonds = append(p.Bonds, e.bondPool[k].claim)
	}
}

// afterLedgerLocked runs after a record was applied to the ledger.
func (e *EpochEngine) afterLedgerLocked(rec *EpochRecord, wasOn bool) {
	if rec.Proposal != nil {
		for _, c := range rec.Proposal.Bonds {
			delete(e.bondPool, c.Key())
			if e.ownClaims[c.Key()] != nil {
				e.log.Infof("Our validator bond %s was admitted in epoch %d; it counts from epoch %d", c.Key(), rec.Epoch, e.ledger.matureEpoch(rec.Epoch))
			}
		}
	}
	if !wasOn && e.ledger.switchSet {
		var sigs []*SwitchSignal
		for _, s := range e.ledger.sigs {
			sigs = append(sigs, s)
		}
		sort.Slice(sigs, func(i, j int) bool { return sigs[i].Validator < sigs[j].Validator })
		e.log.Infof("BOND SWITCH ON: %d of %d seeded validators signalled (record %d); bonds count from epoch %d, permanently",
			len(sigs), len(e.rel.Validators), e.ledger.switchRec, e.ledger.switchRec+1)
		for _, s := range sigs {
			if _, have := e.sigPool[s.Validator]; !have {
				e.sigPool[s.Validator] = s
			}
		}
		e.latchSigs = sigs
		if !e.latched {
			e.latched = true
			if p := e.latchPath(); p != "" {
				if err := writeLatch(p, e.rel, e.ledger.switchRec+1, sigs); err != nil {
					e.log.Errorf("Error writing the bond switch latch: %v", err)
				}
			}
		}
	}
}

// resetLedgerLocked starts a new ledger (chain restart).
func (e *EpochEngine) resetLedgerLocked() {
	e.ledger = newBondLedger(e.rel)
	e.valsCache = nil
	e.gateCache = nil
}

// s4Gossip re-sends our pending signal and claims every few epochs.
func (e *EpochEngine) s4Gossip(ctx context.Context, cur uint64) {
	if !e.bondsEnabled() || cur%3 != 0 {
		return
	}
	e.mtx.Lock()
	if e.s4SentAt == cur {
		e.mtx.Unlock()
		return
	}
	e.s4SentAt = cur
	var msgs []*epochMsg
	if s := e.ownSignal; s != nil {
		if _, have := e.ledger.signals[e.self]; !have {
			msgs = append(msgs, &epochMsg{Type: epMsgSignal, Signal: s})
		}
	}
	// A latched node re-sends the proof while the chain lacks it.
	if e.latched && !e.ledger.switchSet {
		for _, s := range e.latchSigs {
			if _, have := e.ledger.signals[s.Validator]; !have && s.Validator != e.self {
				msgs = append(msgs, &epochMsg{Type: epMsgSignal, Signal: s})
			}
		}
	}
	for k, c := range e.ownClaims {
		if !e.ledger.coins[k] {
			msgs = append(msgs, &epochMsg{Type: epMsgBond, Bond: c})
		}
	}
	e.mtx.Unlock()
	for _, m := range msgs {
		e.send(ctx, m)
	}
}

// BondSwitchStatus is the S4 switch state.
type BondSwitchStatus struct {
	Enabled  bool     `json:"enabled"` // the release has bond rules
	On       bool     `json:"on"`      // on for the current epoch
	OnEpoch  uint64   `json:"onEpoch,omitempty"`
	Signals  []string `json:"signals"` // seeded validators whose signal is in the chain
	Need     int      `json:"need"`
	Seeded   int      `json:"seeded"`
	Pending  []string `json:"pending"` // known signals not yet in the chain
	Deadline int64    `json:"deadline,omitempty"`
	Latched  bool     `json:"latched"`
	Self     bool     `json:"selfSignalled"`
}

// BondInfo is an admitted or pending bond.
type BondInfo struct {
	Key         string `json:"key"`
	Validator   string `json:"validator"`
	Name        string `json:"name"`
	AssetID     uint32 `json:"assetID"`
	Amount      uint64 `json:"amount"`
	LockTime    int64  `json:"lockTime"`
	Admitted    uint64 `json:"admittedEpoch,omitempty"`
	MatureEpoch uint64 `json:"matureEpoch,omitempty"`
	Counts      bool   `json:"counts"`
	Verified    bool   `json:"verified,omitempty"`
	Confs       uint32 `json:"confs,omitempty"`
	Err         string `json:"err,omitempty"`
	Own         bool   `json:"own"`
}

// BondsStatus is the S4 state for the debug API and Mesh page.
type BondsStatus struct {
	Switch   BondSwitchStatus `json:"switch"`
	Admitted []*BondInfo      `json:"admitted"`
	Pending  []*BondInfo      `json:"pending"`
	Members  []*MemberInfo    `json:"members"`
	Epoch    uint64           `json:"epoch"`
	Bonded   uint64           `json:"bondedPower"`
}

// MemberInfo is a validator-set member of an epoch.
type MemberInfo struct {
	PeerID string `json:"peerID"`
	Name   string `json:"name"`
	Power  uint64 `json:"power"`
	Seeded bool   `json:"seeded"`
	Since  uint64 `json:"since"`
	Bond   string `json:"bond,omitempty"`
}

// BondsStatus reports the switch, bonds and the validator set for epoch n
// (0: the current epoch) from our chain up to asOf (0: all).
func (e *EpochEngine) BondsStatus(n, asOf uint64) *BondsStatus {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	if n == 0 {
		n = e.p.EpochOf(e.nowMs())
	}
	if le, _ := e.lastEpochLocked(); asOf == 0 || asOf > le {
		asOf = le
	}
	out := &BondsStatus{Epoch: n, Switch: *e.switchStatusLocked(n, asOf)}
	vs := e.ledger.members(n, asOf)
	for _, b := range e.ledger.bonds {
		if b.epoch > asOf {
			continue
		}
		out.Admitted = append(out.Admitted, &BondInfo{Key: b.claim.Key(), Validator: b.claim.Validator, Name: e.nameOf(b.claim.Validator),
			AssetID: b.claim.AssetID, Amount: b.d.Amount, LockTime: b.d.LockTime, Admitted: b.epoch, MatureEpoch: e.ledger.matureEpoch(b.epoch),
			Counts: e.ledger.bondCounts(b, n, asOf), Own: e.ownClaims[b.claim.Key()] != nil})
	}
	for _, pe := range e.bondPool {
		out.Pending = append(out.Pending, &BondInfo{Key: pe.claim.Key(), Validator: pe.claim.Validator, Name: e.nameOf(pe.claim.Validator),
			AssetID: pe.claim.AssetID, Amount: pe.d.Amount, LockTime: pe.d.LockTime, Verified: pe.verified, Confs: pe.confs, Err: pe.err,
			Own: e.ownClaims[pe.claim.Key()] != nil})
	}
	sort.Slice(out.Pending, func(i, j int) bool { return out.Pending[i].Key < out.Pending[j].Key })
	for _, id := range vs.ids {
		m := vs.members[id]
		mi := &MemberInfo{PeerID: id, Name: e.nameOf(id), Power: m.Power, Seeded: m.Seeded, Since: m.Since}
		if m.Bond != nil {
			mi.Bond = m.Bond.claim.Key()
		}
		out.Members = append(out.Members, mi)
	}
	out.Bonded = vs.BondedPower()
	return out
}

func (e *EpochEngine) switchStatusLocked(n, asOf uint64) *BondSwitchStatus {
	sw := &BondSwitchStatus{Enabled: e.rel.Bonds != nil, Seeded: len(e.rel.Validators), Latched: e.latched, Self: e.ownSignal != nil}
	if r := e.rel.Bonds; r != nil {
		sw.Need = r.switchNeed(len(e.rel.Validators))
		sw.Deadline = r.SwitchDeadline
		sw.On = e.ledger.switchOn(n, asOf)
		if st, on := e.ledger.switchStart(asOf); on {
			sw.OnEpoch = st
		}
	}
	for v, ep := range e.ledger.signals {
		if ep <= asOf {
			sw.Signals = append(sw.Signals, e.rel.ValidatorName(v))
		}
	}
	sort.Strings(sw.Signals)
	for v := range e.sigPool {
		if _, have := e.ledger.signals[v]; !have {
			sw.Pending = append(sw.Pending, e.rel.ValidatorName(v))
		}
	}
	sort.Strings(sw.Pending)
	return sw
}

// nameOf is a validator's release name, our mesh name hint or a short ID.
func (e *EpochEngine) nameOf(id string) string {
	if n := e.peerNames[id]; n != "" {
		return n
	}
	return e.rel.ValidatorName(id)
}

// SetPeerName sets a display name for a (bonded) validator (debug/UI).
func (e *EpochEngine) SetPeerName(id, name string) {
	e.mtx.Lock()
	e.peerNames[id] = name
	e.mtx.Unlock()
}
