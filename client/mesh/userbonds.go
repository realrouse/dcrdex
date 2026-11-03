// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S6: user (trader) fidelity bonds, required to trade.
//
// A release with UserBonds rules requires every order owner to hold a
// counted trading bond. The bond is the standard dcrdex v0 fidelity bond
// (MakeBondTx: a CLTV-timelocked output refundable only by its owner after
// the lock), committing to sha256("bisonary-user-bond-v1:" + peer ID), so it
// is tied to the trader's mesh key. There is no server: the trader gossips a
// signed claim with the raw transaction, validators check on chain with
// their own wallets that the output is confirmed and unspent, and proposers
// put verified claims into proposals (a validator signs a proposal with
// claims only after it verified every claim itself). Once a record admits a
// bond, every peer derives the same bond state from the finalized chain.
//
// A bond counts for epoch n when it was admitted before n, it is at least
// MinAgeSecs old (counted from the admitting record, so the age is the same
// for every peer), and its lock has at least MinLockLeftSecs left. The
// owner's tier is the sum over its counted bonds of floor(amount /
// TierAmount of the bond's asset), capped at MaxTier. With tier 0 a user
// cannot trade: validators drop the user's orders from proposals, and the
// deterministic epoch state ignores them and removes the user's resting
// orders (an expired bond drops the orders). A bigger bond raises the
// limits: an order may have at most tier*LotsPerTier lots, and a user may
// have at most tier*OrdersPerTier resting orders.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
)

// UserBondAsset is an accepted trading bond asset: TierAmount atoms buy one
// tier.
type UserBondAsset struct {
	AssetID    uint32 `json:"assetID"`
	Symbol     string `json:"symbol"`
	TierAmount uint64 `json:"tierAmount"`
}

// UserBondRules are the release-defined trading bond rules.
type UserBondRules struct {
	Assets []UserBondAsset `json:"assets"`
	// MinAgeSecs is the bond age before it counts (maturity).
	MinAgeSecs int64 `json:"minAgeSecs"`
	// MinLockSecs: at admission the lock must have at least this left.
	MinLockSecs int64 `json:"minLockSecs"`
	// MinLockLeftSecs: a bond stops counting (expires) when less than this
	// is left of its lock.
	MinLockLeftSecs int64 `json:"minLockLeftSecs"`
	// MinConfs: confirmations a validator needs to see before it signs a
	// proposal admitting the bond.
	MinConfs uint32 `json:"minConfs"`
	// LotsPerTier: an order may have at most tier*LotsPerTier lots.
	LotsPerTier uint64 `json:"lotsPerTier"`
	// OrdersPerTier: at most tier*OrdersPerTier resting orders per user.
	OrdersPerTier uint64 `json:"ordersPerTier"`
	// MaxTier caps the tier.
	MaxTier uint64 `json:"maxTier"`
	// MaxClaimsPerEpoch caps the trading bond admissions in one proposal.
	MaxClaimsPerEpoch int `json:"maxClaimsPerEpoch"`
}

// Validate checks the rules.
func (u *UserBondRules) Validate() error {
	switch {
	case len(u.Assets) == 0:
		return errors.New("user bond rules list no assets")
	case u.MinAgeSecs < 0 || u.MinLockLeftSecs < 0:
		return errors.New("negative user bond age or lock")
	case u.MinLockSecs < u.MinAgeSecs+u.MinLockLeftSecs:
		return errors.New("minLockSecs must cover minAgeSecs + minLockLeftSecs")
	case u.LotsPerTier == 0 || u.OrdersPerTier == 0 || u.MaxTier == 0:
		return errors.New("lotsPerTier, ordersPerTier and maxTier must be positive")
	case u.MaxClaimsPerEpoch <= 0:
		return errors.New("maxClaimsPerEpoch must be positive")
	}
	seen := make(map[uint32]bool)
	for _, a := range u.Assets {
		switch a.AssetID {
		case BondAssetBTC, BondAssetDCR, BondAssetLBC:
		default:
			return fmt.Errorf("unsupported user bond asset %d", a.AssetID)
		}
		if seen[a.AssetID] {
			return fmt.Errorf("duplicate user bond asset %d", a.AssetID)
		}
		seen[a.AssetID] = true
		if a.TierAmount == 0 {
			return fmt.Errorf("user bond asset %s: zero tier amount", a.Symbol)
		}
	}
	return nil
}

func (u *UserBondRules) asset(id uint32) *UserBondAsset {
	for i := range u.Assets {
		if u.Assets[i].AssetID == id {
			return &u.Assets[i]
		}
	}
	return nil
}

const (
	userBondDomain     = "bisonary-userbond-v1:"
	userBondAcctDomain = "bisonary-user-bond-v1:"
	userBondKeyDomain  = "bisonary-user-bond-key-v1:"
)

// UserBondAcct is the account commitment of a trading bond for peer ID id.
func UserBondAcct(id string) [32]byte { return sha256.Sum256([]byte(userBondAcctDomain + id)) }

// UserBond is a trader's signed claim of a trading bond.
type UserBond struct {
	User    string `json:"u"`
	AssetID uint32 `json:"a"`
	CoinID  []byte `json:"c"`
	RawTx   []byte `json:"tx"`
	Stamp   int64  `json:"t"`
	Sig     []byte `json:"s"`
}

func (c *UserBond) msg() []byte {
	b := new(bytes.Buffer)
	b.WriteString(userBondDomain)
	writeStr(b, c.User)
	_ = binary.Write(b, binary.BigEndian, c.AssetID)
	_ = binary.Write(b, binary.BigEndian, uint16(len(c.CoinID)))
	b.Write(c.CoinID)
	th := sha256.Sum256(c.RawTx)
	b.Write(th[:])
	_ = binary.Write(b, binary.BigEndian, c.Stamp)
	return b.Bytes()
}

func (c *UserBond) sign(priv libp2pcrypto.PrivKey) error {
	pid, _, err := ownerOf(priv)
	if err != nil {
		return err
	}
	c.User = pid.String()
	c.Sig, err = priv.Sign(c.msg())
	return err
}

// Key identifies the bond output.
func (c *UserBond) Key() string { return fmt.Sprintf("%d:%x", c.AssetID, c.CoinID) }

// checkUserBond is the static (chain-independent) check of a claim.
func checkUserBond(rules *UserBondRules, c *UserBond) (*bondDetails, error) {
	if rules.asset(c.AssetID) == nil {
		return nil, fmt.Errorf("asset %d is not a trading bond asset of this release", c.AssetID)
	}
	if err := verifyByID(c.User, c.msg(), c.Sig); err != nil {
		return nil, fmt.Errorf("bad trading bond signature: %w", err)
	}
	d, err := parseBondTx(c.AssetID, c.RawTx, c.CoinID)
	if err != nil {
		return nil, err
	}
	if d.Acct != UserBondAcct(c.User) {
		return nil, errors.New("bond commits to another account than the claimant's mesh key")
	}
	return d, nil
}

// userBondKeyBytes derives the secp256k1 key of a trading bond from the mesh
// key, the asset and the lock time, so the owner can always re-derive it to
// refund the bond (nothing secret is stored).
func userBondKeyBytes(priv libp2pcrypto.PrivKey, assetID uint32, lockTime int64) ([]byte, error) {
	raw, err := priv.Raw()
	if err != nil {
		return nil, err
	}
	b := new(bytes.Buffer)
	b.WriteString(userBondKeyDomain)
	b.Write(raw)
	_ = binary.Write(b, binary.BigEndian, assetID)
	_ = binary.Write(b, binary.BigEndian, lockTime)
	k := sha256.Sum256(b.Bytes())
	return k[:], nil
}

// ---------------- ledger part (bondLedger in bonds.go) ----------------

type ledgerUserBond struct {
	bond  *UserBond
	d     *bondDetails
	epoch uint64 // admitting record
}

func (l *bondLedger) urules() *UserBondRules { return l.rel.UserBonds }

func (l *bondLedger) applyUserBonds(rec *EpochRecord) {
	r := l.urules()
	if r == nil || rec.Proposal == nil {
		return
	}
	for _, c := range rec.Proposal.UserBonds {
		d, err := checkUserBond(r, c)
		if err != nil || l.ucoins[c.Key()] {
			continue // a finalized record passed the same check; defensive
		}
		l.ucoins[c.Key()] = true
		l.ubonds = append(l.ubonds, &ledgerUserBond{bond: c, d: d, epoch: rec.Epoch})
		l.ubondsBy[c.User] = append(l.ubondsBy[c.User], l.ubonds[len(l.ubonds)-1])
	}
}

// userMatureEpoch is the first epoch a trading bond admitted in record a
// counts.
func (l *bondLedger) userMatureEpoch(a uint64) uint64 {
	p := &l.rel.Params
	t := p.EpochEnd(a) + l.urules().MinAgeSecs*1000
	m := uint64((t + p.EpochMs - 1) / p.EpochMs)
	if m <= a {
		m = a + 1
	}
	return m
}

// userExpiryEpoch is the first epoch a trading bond no longer counts
// (less than MinLockLeftSecs of its lock left at the epoch start).
func (l *bondLedger) userExpiryEpoch(b *ledgerUserBond) uint64 {
	p := &l.rel.Params
	t := b.d.LockTime*1000 - l.urules().MinLockLeftSecs*1000 // last counting epoch start <= t
	if t < 0 {
		return 0
	}
	return uint64(t/p.EpochMs) + 1
}

// userBondCounts: b counts for epoch n with records up to asOf.
func (l *bondLedger) userBondCounts(b *ledgerUserBond, n, asOf uint64) bool {
	return b.epoch < n && b.epoch <= asOf && n >= l.userMatureEpoch(b.epoch) && n < l.userExpiryEpoch(b)
}

// userTier is the trading tier of user id for epoch n.
func (l *bondLedger) userTier(id string, n, asOf uint64) uint64 {
	r := l.urules()
	if r == nil {
		return 0
	}
	var tier uint64
	for _, b := range l.ubondsBy[id] {
		if !l.userBondCounts(b, n, asOf) {
			continue
		}
		if a := r.asset(b.bond.AssetID); a != nil {
			tier += b.d.Amount / a.TierAmount
		}
	}
	return min(tier, r.MaxTier)
}

// userGate is the S6 trading gate of one epoch: the tiers of order owners.
// nil: no trading bonds in the release (everyone trades).
type userGate struct {
	rules *UserBondRules
	tier  func(owner string) uint64
}

func (l *bondLedger) gate(n, asOf uint64) *userGate {
	r := l.urules()
	if r == nil {
		return nil
	}
	cache := make(map[string]uint64)
	return &userGate{rules: r, tier: func(owner string) uint64 {
		t, ok := cache[owner]
		if !ok {
			t = l.userTier(owner, n, asOf)
			cache[owner] = t
		}
		return t
	}}
}

// Order rejection reasons of the gate.
const (
	gateNoBond    = "no counted trading bond"
	gateTooBig    = "order larger than the trading bond tier allows"
	gateTooMany   = "too many resting orders for the trading bond tier"
	gateBadLotQty = "order quantity is not whole lots"
)

// orderReason is why the gate refuses o, given owner's resting count.
func (g *userGate) orderReason(o *MeshOrder, resting uint64) string {
	if g == nil {
		return ""
	}
	t := g.tier(o.Owner)
	if t == 0 {
		return gateNoBond
	}
	if o.LotSize == 0 || o.Qty%o.LotSize != 0 {
		return gateBadLotQty
	}
	if o.Qty/o.LotSize > t*g.rules.LotsPerTier {
		return gateTooBig
	}
	if resting >= t*g.rules.OrdersPerTier {
		return gateTooMany
	}
	return ""
}

// UserBondInfo is an admitted or pending trading bond.
type UserBondInfo struct {
	Key         string `json:"key"`
	User        string `json:"user"`
	Name        string `json:"name,omitempty"`
	AssetID     uint32 `json:"assetID"`
	Amount      uint64 `json:"amount"`
	LockTime    int64  `json:"lockTime"`
	Tiers       uint64 `json:"tiers"`
	Admitted    uint64 `json:"admittedEpoch,omitempty"`
	MatureEpoch uint64 `json:"matureEpoch,omitempty"`
	ExpiryEpoch uint64 `json:"expiryEpoch,omitempty"`
	Counts      bool   `json:"counts"`
	Verified    bool   `json:"verified,omitempty"`
	Confs       uint32 `json:"confs,omitempty"`
	Err         string `json:"err,omitempty"`
	Own         bool   `json:"own"`
	Refunded    string `json:"refundTx,omitempty"`
}

// UserTierInfo is a user's tier and limits for an epoch.
type UserTierInfo struct {
	User      string `json:"user"`
	Name      string `json:"name,omitempty"`
	Tier      uint64 `json:"tier"`
	MaxLots   uint64 `json:"maxLotsPerOrder"`
	MaxOrders uint64 `json:"maxRestingOrders"`
	Resting   int    `json:"resting"`
}

// UserBondsStatus is the S6 state (debug API and Mesh page).
type UserBondsStatus struct {
	Required bool            `json:"required"`
	Rules    *UserBondRules  `json:"rules,omitempty"`
	Epoch    uint64          `json:"epoch"`
	AsOf     uint64          `json:"asOf"`
	Admitted []*UserBondInfo `json:"admitted"`
	Pending  []*UserBondInfo `json:"pending"`
	Users    []*UserTierInfo `json:"users"` // users with an admitted bond
	Self     *UserTierInfo   `json:"self"`
	Ignored  map[string]int  `json:"ignoredOrders"` // pool orders refused by the gate, by reason
	Dropped  int             `json:"droppedResting"`
}

// DefaultUserBondRules are a PROPOSAL (PLAN S6: "a small refundable bond
// worth a few dollars in any traded coin, ~1 month lock; bigger bond raises
// trading limits; exact sizes per release"). The sizes per asset are an
// OPEN decision; these are about USD 5-8 per tier at November 2026 prices.
var DefaultUserBondRules = UserBondRules{
	Assets: []UserBondAsset{
		{AssetID: BondAssetBTC, Symbol: "btc", TierAmount: 10_000},       // 0.0001 BTC
		{AssetID: BondAssetLBC, Symbol: "lbc", TierAmount: 10_000 * 1e8}, // 10,000 LBC
		{AssetID: BondAssetDCR, Symbol: "dcr", TierAmount: 50_000_000},   // 0.5 DCR
	},
	MinAgeSecs:        3600,            // proposal
	MinLockSecs:       30 * secsPerDay, // ~1 month (PLAN)
	MinLockLeftSecs:   2 * secsPerDay,  // proposal
	MinConfs:          2,
	LotsPerTier:       5,
	OrdersPerTier:     10,
	MaxTier:           100,
	MaxClaimsPerEpoch: 8,
}

func sortUserBondInfos(l []*UserBondInfo) {
	sort.Slice(l, func(i, j int) bool { return l[i].Key < l[j].Key })
}
