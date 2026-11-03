// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func testUserBondRules() *UserBondRules {
	return &UserBondRules{
		Assets: []UserBondAsset{
			{AssetID: BondAssetBTC, Symbol: "btc", TierAmount: 10_000},
			{AssetID: BondAssetLBC, Symbol: "lbc", TierAmount: 10_000e8},
			{AssetID: BondAssetDCR, Symbol: "dcr", TierAmount: 50_000_000},
		},
		MinAgeSecs: 20, MinLockSecs: 200, MinLockLeftSecs: 60, MinConfs: 1,
		LotsPerTier: 2, OrdersPerTier: 3, MaxTier: 10, MaxClaimsPerEpoch: 8,
	}
}

func s6Params() EpochParams {
	p := s3Params()
	p.EpochMs, p.SignWindowMs, p.ProposeDelayMs, p.FallbackMs, p.OmissionGraceMs = 2000, 1000, 200, 300, 500
	return p
}

func makeUserBondRelease(t *testing.T, ids []peer.ID, params EpochParams, rules *UserBondRules) *Release {
	t.Helper()
	r := &Release{Version: ReleaseVersion, Name: "test-s6", Network: "simnet", Issued: 1, Protocol: ProtocolVersion,
		MinProtocol: MinProtocolVersion, Params: params, UserBonds: rules}
	for i, id := range ids {
		r.Validators = append(r.Validators, ReleaseValidator{PeerID: id.String(), Name: fmt.Sprintf("v%d", i), Bond: 1})
	}
	b, err := SignRelease(r, testReleaseKey)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := ParseRelease(b, testReleaseKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

type fakeUserBondMaker struct {
	t       *testing.T
	chain   *fakeBondChain
	mtx     sync.Mutex
	refunds []string
}

func (m *fakeUserBondMaker) MakeUserBond(_ context.Context, a uint32, amt uint64, lock time.Time, key []byte, acct [32]byte, broadcast bool) ([]byte, []byte, error) {
	c, raw := makeTestBondTx(m.t, a, amt, lock, key, acct, false)
	if broadcast {
		m.chain.mtx.Lock()
		m.chain.confs[bkey(a, c)] = 0
		m.chain.mtx.Unlock()
	}
	return c, raw, nil
}

func (m *fakeUserBondMaker) RefundUserBond(_ context.Context, a uint32, coinID, _ []byte, _ uint64, key []byte) (string, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	if len(key) != 32 {
		return "", fmt.Errorf("bad key")
	}
	m.refunds = append(m.refunds, bkey(a, coinID))
	return "refund-" + bkey(a, coinID), nil
}

// TestUserGateRules checks the deterministic S6 rules of the epoch state.
func TestUserGateRules(t *testing.T) {
	p := DefaultEpochParams
	a, b, c := newPriv(t), newPriv(t), newPriv(t)
	tiers := map[string]uint64{pid(t, a).String(): 1, pid(t, b).String(): 2}
	rules := testUserBondRules()
	g := &userGate{rules: rules, tier: func(o string) uint64 { return tiers[o] }}
	n := uint64(180_000_000)
	stamp := p.EpochEnd(n) - 3000
	mk := func(priv libp2pcrypto.PrivKey, sell bool, lots, rate uint64) *MeshOrder {
		o := &MeshOrder{Market: dcrBTC, Sell: sell, Qty: lots * 1e8, LotSize: 1e8, Rate: rate, Stamp: stamp, Expiry: stamp + 600_000,
			Nonce: uint64(time.Now().UnixNano()), EncPubKey: tEncPub}
		if err := o.Sign(priv); err != nil {
			t.Fatal(err)
		}
		return o
	}
	s := newEpochState()
	// Unbonded c: ignored, not seen. a (tier 1): 3 lots > 2 ignored; 2 lots rest.
	cOrd := mk(c, true, 1, 100_000)
	aBig := mk(a, true, 3, 100_000)
	aOK := mk(a, true, 2, 100_000)
	if m := applyEpoch(s, &EpochProposal{Epoch: n, Orders: []*MeshOrder{cOrd, aBig, aOK}}, &p, g); len(m) != 0 {
		t.Fatal("matches")
	}
	if _, ok := s.resting[cOrd.ID()]; ok {
		t.Fatal("unbonded order rested")
	}
	if _, ok := s.seen[cOrd.ID()]; ok {
		t.Fatal("unbonded order marked seen")
	}
	if _, ok := s.resting[aBig.ID()]; ok {
		t.Fatal("order above the tier rested")
	}
	if _, ok := s.resting[aOK.ID()]; !ok {
		t.Fatal("bonded order not resting")
	}
	// a may have 3 resting orders at tier 1: two more rest, the 4th is ignored.
	o2, o3, o4 := mk(a, true, 1, 101_000), mk(a, true, 1, 102_000), mk(a, true, 1, 103_000)
	applyEpoch(s, &EpochProposal{Epoch: n + 1, Orders: []*MeshOrder{o2, o3, o4}}, &p, g)
	if len(s.resting) != 3 {
		t.Fatalf("resting %d, want 3", len(s.resting))
	}
	out := 0
	for _, o := range []*MeshOrder{o2, o3, o4} {
		if _, ok := s.resting[o.ID()]; !ok {
			out++
		}
	}
	if out != 1 {
		t.Fatalf("%d orders beyond the resting limit, want 1", out)
	}
	// b (tier 2) takes 1 lot from a, freeing nothing (a's order has 2 lots).
	bb := mk(b, false, 1, 100_000)
	if m := applyEpoch(s, &EpochProposal{Epoch: n + 2, Orders: []*MeshOrder{bb}}, &p, g); len(m) != 1 || m[0].Maker != pid(t, a).String() {
		t.Fatalf("bonded trade: %v", m)
	}
	// a's bond stops counting: its resting orders are dropped.
	tiers[pid(t, a).String()] = 0
	applyEpoch(s, &EpochProposal{Epoch: n + 3}, &p, g)
	if len(s.resting) != 0 {
		t.Fatalf("expired bond: %d orders still resting", len(s.resting))
	}
	// No gate (release without trading bonds): everything rests.
	s2 := newEpochState()
	applyEpoch(s2, &EpochProposal{Epoch: n, Orders: []*MeshOrder{mk(c, true, 9, 100_000)}}, &p)
	if len(s2.resting) != 1 {
		t.Fatal("no gate")
	}
}

func TestUserBondTiers(t *testing.T) {
	var ids []peer.ID
	for i := 0; i < 4; i++ {
		ids = append(ids, pid(t, newPriv(t)))
	}
	rel := makeUserBondRelease(t, ids, s6Params(), testUserBondRules())
	l := newBondLedger(rel)
	u := newPriv(t)
	uid := pid(t, u).String()
	n := uint64(900_000_000)
	lock := time.UnixMilli(rel.Params.EpochEnd(n)).Add(300 * time.Second)
	mkBond := func(a uint32, amt uint64, acct [32]byte) *UserBond {
		key, _ := userBondKeyBytes(u, a, lock.Unix())
		c, raw := makeTestBondTx(t, a, amt, lock, key, acct, false)
		b := &UserBond{AssetID: a, CoinID: c, RawTx: raw, Stamp: 1}
		if err := b.sign(u); err != nil {
			t.Fatal(err)
		}
		return b
	}
	good := mkBond(BondAssetDCR, 120_000_000, UserBondAcct(uid)) // 2 tiers
	btc := mkBond(BondAssetBTC, 35_000, UserBondAcct(uid))       // 3 tiers
	wrong := mkBond(BondAssetLBC, 20_000e8, UserBondAcct("someone else"))
	if _, err := checkUserBond(rel.UserBonds, wrong); err == nil {
		t.Fatal("bond committing to another account accepted")
	}
	vb := mkBond(BondAssetBTC, 35_000, BondAcct(uid)) // a validator bond commitment
	if _, err := checkUserBond(rel.UserBonds, vb); err == nil {
		t.Fatal("validator bond accepted as a trading bond")
	}
	l.apply(&EpochRecord{Epoch: n, Proposal: &EpochProposal{Epoch: n, UserBonds: []*UserBond{good, btc}}})
	mature := l.userMatureEpoch(n)
	if mature != n+11 { // 20 s / 2 s after the record's end
		t.Fatalf("mature %d want %d", mature, n+11)
	}
	if l.userTier(uid, mature-1, n) != 0 || l.userTier(uid, mature, n) != 5 {
		t.Fatalf("tier before/after maturity: %d %d", l.userTier(uid, mature-1, n), l.userTier(uid, mature, n))
	}
	exp := l.userExpiryEpoch(l.ubonds[0])
	if l.userTier(uid, exp-1, n) != 5 || l.userTier(uid, exp, n) != 0 {
		t.Fatalf("tier before/after expiry %d: %d %d", exp, l.userTier(uid, exp-1, n), l.userTier(uid, exp, n))
	}
	// Lock 300 s, stops with 60 s left: last counting epoch starts 240 s after n's end.
	if got := int64(exp-n) * 2; got < 240 || got > 244 {
		t.Fatalf("expiry %d s after admission", got)
	}
}

type ubondSim struct {
	*epochSim
	chain *fakeBondChain
	maker *fakeUserBondMaker
	dir   string
}

func newUBondSim(t *testing.T, nTraders int) *ubondSim {
	s := &epochSim{t: t, net: newEpochNet(), clock: &fakeClock{t: time.UnixMilli(1_800_000_000_000)},
		ctx: context.Background(), quiet: testLogger()}
	s.rng = newTestRand()
	us := &ubondSim{epochSim: s, chain: newFakeBondChain(), dir: t.TempDir()}
	us.maker = &fakeUserBondMaker{t: t, chain: us.chain}
	var ids []peer.ID
	for i := 0; i < 4; i++ {
		priv := newPriv(t)
		s.vals = append(s.vals, &simNode{name: fmt.Sprintf("v%d", i), priv: priv, id: pid(t, priv)})
		ids = append(ids, s.vals[i].id)
	}
	for i := 0; i < nTraders; i++ {
		tp := newPriv(t)
		s.traders = append(s.traders, &simNode{name: fmt.Sprintf("t%d", i), priv: tp, id: pid(t, tp)})
	}
	s.rel = makeUserBondRelease(t, ids, s6Params(), testUserBondRules())
	for _, n := range s.vals {
		us.start(n, true)
	}
	for _, n := range s.traders {
		us.start(n, false)
	}
	return us
}

func (us *ubondSim) start(n *simNode, validator bool) {
	us.startNode(n, validator, filepath.Join(us.dir, n.name+".jsonl"))
	n.e.SetBondHooks(us.chain, nil)
	n.e.SetUserBondMaker(us.maker)
}

func (us *ubondSim) epochsMining(k int) {
	for i := 0; i < k; i++ {
		us.chain.mineAll()
		us.epochs(1)
	}
}

func (us *ubondSim) tryOrder(n *simNode, sell bool, lots, rate uint64) (*MeshOrder, error) {
	now := n.e.now()
	o := &MeshOrder{Market: dcrBTC, Sell: sell, Qty: lots * 1e8, LotSize: 1e8, Rate: rate, Stamp: now.UnixMilli(),
		Expiry: now.Add(10 * time.Minute).UnixMilli(), Nonce: us.rng.Uint64(), EncPubKey: tEncPub}
	if err := o.Sign(n.priv); err != nil {
		us.t.Fatal(err)
	}
	return o, n.e.SubmitOrder(us.ctx, o)
}

func (us *ubondSim) tiersAgree(who []*simNode) map[string]uint64 {
	us.t.Helper()
	var ref map[string]uint64
	var refName string
	asof := lastEpoch(us.vals[0].e)
	for _, n := range us.all() {
		if lastEpoch(n.e) < asof {
			asof = lastEpoch(n.e)
		}
	}
	for _, n := range us.all() {
		st := n.e.UserBondsStatus(asof+1, asof)
		m := make(map[string]uint64)
		for _, w := range who {
			for _, u := range st.Users {
				if u.User == w.id.String() {
					m[w.name] = u.Tier
				}
			}
		}
		if ref == nil {
			ref, refName = m, n.name
			continue
		}
		if fmt.Sprint(m) != fmt.Sprint(ref) {
			us.t.Fatalf("tiers differ: %s %v, %s %v", refName, ref, n.name, m)
		}
	}
	return ref
}

func inRecords(e *EpochEngine, id OrderID) bool {
	for _, r := range e.Records(0, 1e6) {
		for _, o := range r.Proposal.Orders {
			if o.ID() == id {
				return true
			}
		}
	}
	return false
}

// TestUserBondsSim is the S6 plan test on a simulated mesh: an unbonded
// client's orders are ignored by validators; BTC, LBC and DCR bonds count
// only once mature; a bonded client trades; tiers limit the order size;
// under-bonded and unbroadcast bonds never count; an expired bond drops the
// orders; every peer agrees on the tiers; the bond is refundable after the
// lock; a restart keeps the bond state.
func TestUserBondsSim(t *testing.T) {
	us := newUBondSim(t, 5)
	tBTC, tLBC, tDCR, tNone, tUnder := us.traders[0], us.traders[1], us.traders[2], us.traders[3], us.traders[4]
	us.epochs(12)
	if us.checkAgreement(us.all()) == 0 {
		t.Fatal("no records")
	}
	// Unbonded: refused locally; sent anyway, ignored by every validator.
	o, err := us.tryOrder(tNone, true, 1, 100_000)
	if err == nil {
		t.Fatal("unbonded order accepted")
	}
	tNone.e.SendOrderUnchecked(us.ctx, o)
	us.epochs(3)
	for _, v := range us.vals {
		if st := v.e.OrderStatus(o.ID()); st != "refused: "+gateNoBond {
			t.Fatalf("%s: unbonded order status %q", v.name, st)
		}
	}
	// Bonds: BTC 2 tiers, LBC 1 tier, DCR 1 tier (short lock, expires), an
	// under-sized LBC bond and an unbroadcast DCR bond.
	post := func(n *simNode, a uint32, amt uint64, lock int64, fake bool) *UserBond {
		b, err := n.e.PostUserBond(us.ctx, a, amt, lock, fake)
		if err != nil {
			t.Fatalf("%s bond: %v", n.name, err)
		}
		return b
	}
	post(tBTC, BondAssetBTC, 20_000, 3600, false)
	post(tLBC, BondAssetLBC, 10_000e8, 3600, false)
	dcrBond := post(tDCR, BondAssetDCR, 50_000_000, 260, false)
	post(tUnder, BondAssetLBC, 5_000e8, 3600, false)
	post(tUnder, BondAssetDCR, 50_000_000, 3600, true) // never broadcast
	us.epochsMining(4)
	st := us.vals[0].e.UserBondsStatus(0, 0)
	if len(st.Admitted) != 3 {
		t.Fatalf("admitted %d: %+v pending %+v", len(st.Admitted), st.Admitted, st.Pending)
	}
	if tiers := us.tiersAgree(us.traders); tiers["t0"]+tiers["t1"]+tiers["t2"] != 0 {
		t.Fatalf("immature bonds count: %v", tiers)
	}
	if _, err := us.tryOrder(tBTC, true, 1, 100_000); err == nil {
		t.Fatal("order with an immature bond accepted")
	}
	us.epochsMining(11)
	tiers := us.tiersAgree(us.traders)
	if tiers["t0"] != 2 || tiers["t1"] != 1 || tiers["t2"] != 1 || tiers["t3"] != 0 || tiers["t4"] != 0 {
		t.Fatalf("mature tiers: %v", tiers)
	}
	for _, p := range us.vals[1].e.UserBondsStatus(0, 0).Pending {
		if p.User == tUnder.id.String() && p.Err == "" {
			t.Fatalf("under-bonded pending without reason: %+v", p)
		}
	}
	// Bonded clients trade; tier limits: t1 (tier 1) may not post 3 lots.
	if _, err := us.tryOrder(tLBC, true, 3, 100_000); err == nil {
		t.Fatal("order above the tier accepted")
	}
	sell, err := us.tryOrder(tBTC, true, 4, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := us.tryOrder(tLBC, false, 2, 100_000); err != nil {
		t.Fatal(err)
	}
	us.epochs(3)
	var matched int
	for _, r := range us.vals[0].e.Records(0, 1e6) {
		for _, m := range r.Matches {
			// Same-epoch orders sort by stamp then ID, so either may be maker.
			if m.MakerOrderID == sell.ID() || m.TakerOrderID == sell.ID() {
				matched += int(m.Qty / 1e8)
			}
		}
	}
	if matched != 2 {
		t.Fatalf("bonded trade matched %d lots", matched)
	}
	if inRecords(us.vals[0].e, o.ID()) {
		t.Fatal("unbonded order in a record")
	}
	// t2 rests an order; its DCR bond expires (60 s before its 260 s lock):
	// the order is dropped on every peer.
	rest, err := us.tryOrder(tDCR, true, 1, 200_000)
	if err != nil {
		t.Fatal(err)
	}
	us.epochs(3)
	if _, _, ok := us.vals[0].e.RestingOrder(rest.ID()); !ok {
		t.Fatal("t2 order not resting")
	}
	if _, err := tDCR.e.RefundUserBond(us.ctx, dcrBond.Key()); err != nil {
		t.Fatal(err) // the fake maker does not check the lock; Core's wallet does
	}
	us.epochsMining(100)
	for _, n := range us.all() {
		if _, _, ok := n.e.RestingOrder(rest.ID()); ok {
			t.Fatalf("%s: order of an expired bond still resting", n.name)
		}
	}
	if tiers := us.tiersAgree(us.traders); tiers["t2"] != 0 || tiers["t0"] != 2 {
		t.Fatalf("after expiry: %v", tiers)
	}
	us.checkAgreement(us.all())
	// Restart a client and a validator from their stores: same tiers.
	us.start(tLBC, false)
	us.start(us.vals[3], true)
	us.epochsMining(6)
	us.checkAgreement(us.all())
	if tiers := us.tiersAgree(us.traders); tiers["t0"] != 2 || tiers["t1"] != 1 {
		t.Fatalf("after restart: %v", tiers)
	}
	if len(us.maker.refunds) != 1 {
		t.Fatal("refund")
	}
}
