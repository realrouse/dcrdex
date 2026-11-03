// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S6 engine side: trading bond claims, their on-chain checks by validators,
// admissions in proposals, the trading gate and own bonds (userbonds.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

// UserBondMaker makes, sends and refunds trading bonds with the wallets
// (Core).
type UserBondMaker interface {
	MakeUserBond(ctx context.Context, assetID uint32, amt uint64, lockTime time.Time, bondKey []byte, acct [32]byte, broadcast bool) (coinID, rawTx []byte, err error)
	RefundUserBond(ctx context.Context, assetID uint32, coinID, bondScript []byte, amt uint64, bondKey []byte) (refundCoin string, err error)
}

type ubondEntry struct {
	bond     *UserBond
	d        *bondDetails
	recv     int64
	verified bool
	confs    uint32
	checking bool
	checkAt  int64
	err      string
}

// ownUserBond is a trading bond of ours (kept in a file next to the store).
type ownUserBond struct {
	Bond     *UserBond `json:"bond"`
	Refunded string    `json:"refunded,omitempty"`
}

const maxRefusedOrders = 10_000

// SetUserBondMaker sets the trading bond maker (Core wallets).
func (e *EpochEngine) SetUserBondMaker(m UserBondMaker) {
	e.mtx.Lock()
	e.ubondMaker = m
	e.mtx.Unlock()
}

// UserBondsRequired: the release requires trading bonds.
func (e *EpochEngine) UserBondsRequired() bool { return e.rel.UserBonds != nil }

func (e *EpochEngine) ubondPath() string {
	if e.cfg.StorePath == "" {
		return ""
	}
	return e.cfg.StorePath + ".userbonds.json"
}

func (e *EpochEngine) loadUserBonds() {
	if e.rel.UserBonds == nil {
		return
	}
	p := e.ubondPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var l []*ownUserBond
	if json.Unmarshal(b, &l) != nil {
		return
	}
	for _, ob := range l {
		if ob.Bond == nil || ob.Bond.User != e.self {
			continue
		}
		e.ownUBonds[ob.Bond.Key()] = ob
		e.addUBondLocked(ob.Bond)
	}
	if len(l) > 0 {
		e.log.Infof("Loaded %d own trading bond(s)", len(l))
	}
}

func (e *EpochEngine) saveUserBondsLocked() {
	p := e.ubondPath()
	if p == "" {
		return
	}
	var l []*ownUserBond
	for _, ob := range e.ownUBonds {
		l = append(l, ob)
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Bond.Key() < l[j].Bond.Key() })
	b, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// gateLocked is the trading gate for epoch n on our chain (nil: no
// trading bonds).
func (e *EpochEngine) gateLocked(n uint64) *userGate {
	if e.rel.UserBonds == nil {
		return nil
	}
	if g := e.gateCache; g != nil && e.gateEpoch == n && e.gateFP == e.lastFP {
		return g
	}
	asof, _ := e.lastEpochLocked()
	g := e.ledger.gate(n, asof)
	e.gateCache, e.gateEpoch, e.gateFP = g, n, e.lastFP
	return g
}

// UserTier is owner's trading tier for the current epoch (0: may not trade;
// releases without trading bonds: MaxUint64).
func (e *EpochEngine) UserTier(owner string) uint64 {
	if e.rel.UserBonds == nil {
		return ^uint64(0)
	}
	e.mtx.Lock()
	defer e.mtx.Unlock()
	return e.gateLocked(e.p.EpochOf(e.nowMs())).tier(owner)
}

// gateOrderLocked is the gate's reason to refuse o in epoch n, without the
// resting-count limit (the epoch state enforces that), or "".
func (e *EpochEngine) gateOrderLocked(o *MeshOrder, n uint64) string {
	g := e.gateLocked(n)
	if g == nil {
		return ""
	}
	return g.orderReason(o, 0)
}

func (e *EpochEngine) noteRefusedLocked(id OrderID, reason string) {
	if e.refused == nil {
		e.refused = make(map[OrderID]string)
	}
	if _, have := e.refused[id]; have {
		return
	}
	if len(e.refused) >= maxRefusedOrders {
		e.refused = make(map[OrderID]string)
	}
	e.refused[id] = reason
	e.ignored[reason]++
}

// PostUserBond makes, broadcasts and claims a trading bond of amt atoms
// locked for lockSecs (fake: a valid-looking bond transaction that is never
// broadcast, simnet tests).
func (e *EpochEngine) PostUserBond(ctx context.Context, assetID uint32, amt uint64, lockSecs int64, fake bool) (*UserBond, error) {
	r := e.rel.UserBonds
	if r == nil {
		return nil, errors.New("this release has no trading bonds")
	}
	if r.asset(assetID) == nil {
		return nil, fmt.Errorf("asset %d is not a trading bond asset of this release", assetID)
	}
	e.mtx.Lock()
	maker := e.ubondMaker
	e.mtx.Unlock()
	if maker == nil {
		return nil, errors.New("no bond maker (wallets not available)")
	}
	lock := e.now().Add(time.Duration(lockSecs) * time.Second).Truncate(time.Second)
	key, err := userBondKeyBytes(e.priv, assetID, lock.Unix())
	if err != nil {
		return nil, err
	}
	coinID, raw, err := maker.MakeUserBond(ctx, assetID, amt, lock, key, UserBondAcct(e.self), !fake)
	if err != nil {
		return nil, err
	}
	c := &UserBond{AssetID: assetID, CoinID: coinID, RawTx: raw, Stamp: e.nowMs()}
	if err := c.sign(e.priv); err != nil {
		return nil, err
	}
	if _, err := checkUserBond(r, c); err != nil {
		return nil, fmt.Errorf("own trading bond fails the check: %w", err)
	}
	e.mtx.Lock()
	e.ownUBonds[c.Key()] = &ownUserBond{Bond: c}
	e.saveUserBondsLocked()
	e.addUBondLocked(c)
	e.mtx.Unlock()
	e.send(ctx, &epochMsg{Type: epMsgUBond, UBond: c})
	e.log.Infof("Posted trading bond %s: asset %d, %d atoms, lock until %s (broadcast %v)", c.Key(), assetID, amt, lock.Format(time.RFC3339), !fake)
	return c, nil
}

// RefundUserBond refunds an own trading bond after its lock.
func (e *EpochEngine) RefundUserBond(ctx context.Context, key string) (string, error) {
	e.mtx.Lock()
	ob := e.ownUBonds[key]
	maker := e.ubondMaker
	e.mtx.Unlock()
	if ob == nil {
		return "", fmt.Errorf("no own trading bond %s", key)
	}
	if maker == nil {
		return "", errors.New("no bond maker (wallets not available)")
	}
	d, err := parseBondTx(ob.Bond.AssetID, ob.Bond.RawTx, ob.Bond.CoinID)
	if err != nil {
		return "", err
	}
	bk, err := userBondKeyBytes(e.priv, ob.Bond.AssetID, d.LockTime)
	if err != nil {
		return "", err
	}
	coin, err := maker.RefundUserBond(ctx, ob.Bond.AssetID, ob.Bond.CoinID, d.Script, d.Amount, bk)
	if err != nil {
		return "", err
	}
	e.mtx.Lock()
	ob.Refunded = coin
	e.saveUserBondsLocked()
	e.mtx.Unlock()
	e.log.Infof("Refunded trading bond %s: %s", key, coin)
	return coin, nil
}

func (e *EpochEngine) addUBondLocked(c *UserBond) {
	if e.rel.UserBonds == nil || e.ledger.ucoins[c.Key()] {
		return
	}
	if _, have := e.ubondPool[c.Key()]; have || len(e.ubondPool) >= 1000 {
		return
	}
	d, err := checkUserBond(e.rel.UserBonds, c)
	if err != nil {
		e.log.Debugf("Rejected trading bond claim %s from %s: %v", c.Key(), c.User, err)
		return
	}
	e.ubondPool[c.Key()] = &ubondEntry{bond: c, d: d, recv: e.nowMs()}
}

// ubondChecks starts on-chain checks of pool claims (validators only).
func (e *EpochEngine) ubondChecks(ctx context.Context, nowMs int64) {
	if !e.signer || e.rel.UserBonds == nil {
		return
	}
	e.mtx.Lock()
	chain := e.bondChain
	var todo []*ubondEntry
	for _, pe := range e.ubondPool {
		if pe.verified || pe.checking || nowMs < pe.checkAt {
			continue
		}
		pe.checking = true
		todo = append(todo, pe)
	}
	if chain == nil {
		for _, pe := range todo {
			pe.checking, pe.err, pe.checkAt = false, "no wallet for bond checks", nowMs+10_000
		}
		todo = nil
	}
	e.mtx.Unlock()
	for _, pe := range todo {
		go func(pe *ubondEntry) {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			since := time.UnixMilli(pe.bond.Stamp).Add(-2 * time.Hour)
			confs, spent, err := chain.BondConfs(cctx, pe.bond.AssetID, pe.bond.CoinID, pe.d.Script, since)
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
			case confs < e.rel.UserBonds.MinConfs:
				pe.err = fmt.Sprintf("%d of %d confirmations", confs, e.rel.UserBonds.MinConfs)
			default:
				if !pe.verified {
					e.log.Infof("Verified trading bond %s of %s on chain (%d confs, %d atoms)", pe.bond.Key(), e.nameOf(pe.bond.User), confs, pe.d.Amount)
				}
				pe.verified, pe.err = true, ""
			}
		}(pe)
	}
}

// uAdmissibleLocked: trading bond c may be admitted in epoch n.
func (e *EpochEngine) uAdmissibleLocked(c *UserBond, n uint64) error {
	r := e.rel.UserBonds
	d, err := checkUserBond(r, c)
	if err != nil {
		return err
	}
	if e.ledger.ucoins[c.Key()] {
		return errors.New("already admitted")
	}
	if a := r.asset(c.AssetID); d.Amount < a.TierAmount {
		return fmt.Errorf("bond of %d atoms is below one tier (%d atoms)", d.Amount, a.TierAmount)
	}
	if left := d.LockTime*1000 - e.p.EpochEnd(n); left < r.MinLockSecs*1000 {
		return fmt.Errorf("lock has %d s left, the release needs %d s", left/1000, r.MinLockSecs)
	}
	return nil
}

// checkS6Locked is the deterministic check of a proposal's trading bond
// admissions against our chain.
func (e *EpochEngine) checkS6Locked(p *EpochProposal) error {
	if len(p.UserBonds) == 0 {
		return nil
	}
	if e.rel.UserBonds == nil {
		return errors.New("trading bonds in a release without trading bond rules")
	}
	for _, c := range p.UserBonds {
		if err := e.uAdmissibleLocked(c, p.Epoch); err != nil {
			return fmt.Errorf("trading bond %s: %v", c.Key(), err)
		}
	}
	return nil
}

// ubondsVerifiedLocked: we verified every trading bond of p on chain
// ourselves (to sign). Missing ones are queued for a check.
func (e *EpochEngine) ubondsVerifiedLocked(p *EpochProposal) bool {
	ok := true
	for _, c := range p.UserBonds {
		pe := e.ubondPool[c.Key()]
		if pe == nil {
			e.addUBondLocked(c)
			ok = false
			continue
		}
		if !pe.verified {
			ok = false
		}
	}
	return ok
}

// s6ProposalLocked fills a new proposal's trading bond admissions.
func (e *EpochEngine) s6ProposalLocked(p *EpochProposal) {
	if e.rel.UserBonds == nil {
		return
	}
	var keys []string
	for k, pe := range e.ubondPool {
		if pe.verified && e.uAdmissibleLocked(pe.bond, p.Epoch) == nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > e.rel.UserBonds.MaxClaimsPerEpoch {
		keys = keys[:e.rel.UserBonds.MaxClaimsPerEpoch]
	}
	for _, k := range keys {
		p.UserBonds = append(p.UserBonds, e.ubondPool[k].bond)
	}
}

// afterUserBondsLocked runs after a record was applied to the ledger.
func (e *EpochEngine) afterUserBondsLocked(rec *EpochRecord) {
	if rec.Proposal == nil {
		return
	}
	for _, c := range rec.Proposal.UserBonds {
		delete(e.ubondPool, c.Key())
		if e.ownUBonds[c.Key()] != nil {
			e.log.Infof("Our trading bond %s was admitted in epoch %d; it counts from epoch %d", c.Key(), rec.Epoch, e.ledger.userMatureEpoch(rec.Epoch))
		}
	}
}

// s6Gossip re-sends our unadmitted trading bond claims every few epochs.
func (e *EpochEngine) s6Gossip(ctx context.Context, cur uint64) {
	if e.rel.UserBonds == nil || cur%3 != 1 {
		return
	}
	e.mtx.Lock()
	if e.s6SentAt == cur {
		e.mtx.Unlock()
		return
	}
	e.s6SentAt = cur
	var msgs []*epochMsg
	for k, ob := range e.ownUBonds {
		if !e.ledger.ucoins[k] && ob.Refunded == "" {
			msgs = append(msgs, &epochMsg{Type: epMsgUBond, UBond: ob.Bond})
		}
	}
	e.mtx.Unlock()
	for _, m := range msgs {
		e.send(ctx, m)
	}
}

// UserBondsStatus reports the trading bonds and tiers for epoch n (0: the
// current epoch) from our chain up to asOf (0: all).
func (e *EpochEngine) UserBondsStatus(n, asOf uint64) *UserBondsStatus {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	if n == 0 {
		n = e.p.EpochOf(e.nowMs())
	}
	if le, _ := e.lastEpochLocked(); asOf == 0 || asOf > le {
		asOf = le
	}
	r := e.rel.UserBonds
	out := &UserBondsStatus{Required: r != nil, Rules: r, Epoch: n, AsOf: asOf, Admitted: []*UserBondInfo{}, Pending: []*UserBondInfo{},
		Users: []*UserTierInfo{}, Ignored: make(map[string]int)}
	for k, v := range e.ignored {
		out.Ignored[k] = v
	}
	if r == nil {
		return out
	}
	resting := make(map[string]int)
	for _, ro := range e.state.resting {
		resting[ro.ord.Owner]++
	}
	tierInfo := func(id string) *UserTierInfo {
		t := e.ledger.userTier(id, n, asOf)
		return &UserTierInfo{User: id, Name: e.nameOf(id), Tier: t, MaxLots: t * r.LotsPerTier, MaxOrders: t * r.OrdersPerTier, Resting: resting[id]}
	}
	users := make(map[string]bool)
	for _, b := range e.ledger.ubonds {
		if b.epoch > asOf {
			continue
		}
		users[b.bond.User] = true
		bi := &UserBondInfo{Key: b.bond.Key(), User: b.bond.User, Name: e.nameOf(b.bond.User), AssetID: b.bond.AssetID, Amount: b.d.Amount,
			LockTime: b.d.LockTime, Admitted: b.epoch, MatureEpoch: e.ledger.userMatureEpoch(b.epoch), ExpiryEpoch: e.ledger.userExpiryEpoch(b),
			Counts: e.ledger.userBondCounts(b, n, asOf), Own: e.ownUBonds[b.bond.Key()] != nil}
		if a := r.asset(b.bond.AssetID); a != nil {
			bi.Tiers = b.d.Amount / a.TierAmount
		}
		if ob := e.ownUBonds[b.bond.Key()]; ob != nil {
			bi.Refunded = ob.Refunded
		}
		out.Admitted = append(out.Admitted, bi)
	}
	for _, pe := range e.ubondPool {
		bi := &UserBondInfo{Key: pe.bond.Key(), User: pe.bond.User, Name: e.nameOf(pe.bond.User), AssetID: pe.bond.AssetID, Amount: pe.d.Amount,
			LockTime: pe.d.LockTime, Verified: pe.verified, Confs: pe.confs, Err: pe.err, Own: e.ownUBonds[pe.bond.Key()] != nil}
		if err := e.uAdmissibleLocked(pe.bond, n); err != nil && bi.Err == "" {
			bi.Err = err.Error()
		}
		out.Pending = append(out.Pending, bi)
	}
	for k, ob := range e.ownUBonds {
		if e.ledger.ucoins[k] || e.ubondPool[k] != nil {
			continue
		}
		d, err := parseBondTx(ob.Bond.AssetID, ob.Bond.RawTx, ob.Bond.CoinID)
		if err != nil {
			continue
		}
		out.Pending = append(out.Pending, &UserBondInfo{Key: k, User: e.self, AssetID: ob.Bond.AssetID, Amount: d.Amount, LockTime: d.LockTime,
			Own: true, Refunded: ob.Refunded, Err: "not in the pool"})
	}
	sortUserBondInfos(out.Admitted)
	sortUserBondInfos(out.Pending)
	ids := make([]string, 0, len(users))
	for id := range users {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out.Users = append(out.Users, tierInfo(id))
	}
	out.Self = tierInfo(e.self)
	return out
}

// OrderRefusal is why the epoch pool refused order id, or "".
func (e *EpochEngine) OrderRefusal(id OrderID) string {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	return e.refused[id]
}

// SendOrderUnchecked broadcasts an order to the epoch topic without our own
// pool's checks (simnet adversary tests: an unbonded trader's order).
func (e *EpochEngine) SendOrderUnchecked(ctx context.Context, o *MeshOrder) {
	e.send(ctx, &epochMsg{Type: epMsgOrder, Order: o})
}

// CheckOwnOrder is the trading gate's reason to refuse our order o now
// (including the resting-order limit), or "".
func (e *EpochEngine) CheckOwnOrder(o *MeshOrder) string {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	g := e.gateLocked(e.p.EpochOf(e.nowMs()))
	if g == nil {
		return ""
	}
	var resting uint64
	for _, r := range e.state.resting {
		if r.ord.Owner == o.Owner {
			resting++
		}
	}
	return g.orderReason(o, resting)
}
