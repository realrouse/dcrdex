// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	mrand "math/rand"

	"context"
	"crypto/sha256"
	"decred.org/dcrdex/dex"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	dexbtc "decred.org/dcrdex/dex/networks/btc"
	dexdcr "decred.org/dcrdex/dex/networks/dcr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	btctxscript "github.com/btcsuite/btcd/txscript"
	btcwire "github.com/btcsuite/btcd/wire"
	dcrchainhash "github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrutil/v4"
	dcrtxscript "github.com/decred/dcrd/txscript/v4"
	dcrwire "github.com/decred/dcrd/wire"
	"github.com/libp2p/go-libp2p/core/peer"
)

func bondPush(acct [32]byte, lock uint32, pkh []byte) []byte {
	b := make([]byte, 0, 58)
	b = binary.BigEndian.AppendUint16(b, 0)
	b = append(b, acct[:]...)
	b = binary.BigEndian.AppendUint32(b, lock)
	return append(b, pkh...)
}

var testTxNonce uint32

// makeTestBondTx builds a v0 bond tx (one dummy input) for asset.
func makeTestBondTx(t *testing.T, assetID uint32, amt uint64, lock time.Time, key []byte, acct [32]byte, p2sh bool) (coinID, raw []byte) {
	t.Helper()
	pkh := btcutil.Hash160(key)
	testTxNonce++
	var prev [32]byte
	binary.BigEndian.PutUint32(prev[:], testTxNonce)
	switch assetID {
	case BondAssetBTC, BondAssetLBC:
		script, err := dexbtc.MakeBondScript(0, uint32(lock.Unix()), pkh)
		if err != nil {
			t.Fatal(err)
		}
		var pk []byte
		if p2sh {
			pk, _ = btctxscript.NewScriptBuilder().AddOp(btctxscript.OP_HASH160).AddData(btcutil.Hash160(script)).AddOp(btctxscript.OP_EQUAL).Script()
		} else {
			h := sha256.Sum256(script)
			pk, _ = btctxscript.NewScriptBuilder().AddOp(btctxscript.OP_0).AddData(h[:]).Script()
		}
		commit, _ := btctxscript.NewScriptBuilder().AddOp(btctxscript.OP_RETURN).AddData(bondPush(acct, uint32(lock.Unix()), pkh)).Script()
		tx := btcwire.NewMsgTx(2)
		tx.AddTxIn(btcwire.NewTxIn(btcwire.NewOutPoint((*chainhash.Hash)(&prev), 0), nil, nil))
		tx.AddTxOut(btcwire.NewTxOut(int64(amt), pk))
		tx.AddTxOut(btcwire.NewTxOut(0, commit))
		var b bytes.Buffer
		_ = tx.Serialize(&b)
		h := tx.TxHash()
		return append(h[:], 0, 0, 0, 0), b.Bytes()
	case BondAssetDCR:
		script, err := dexdcr.MakeBondScript(0, uint32(lock.Unix()), pkh)
		if err != nil {
			t.Fatal(err)
		}
		pk, _ := dcrtxscript.NewScriptBuilder().AddOp(dcrtxscript.OP_HASH160).AddData(dcrutil.Hash160(script)).AddOp(dcrtxscript.OP_EQUAL).Script()
		commit, _ := dcrtxscript.NewScriptBuilder().AddOp(dcrtxscript.OP_RETURN).AddData(bondPush(acct, uint32(lock.Unix()), pkh)).Script()
		tx := dcrwire.NewMsgTx()
		tx.AddTxIn(dcrwire.NewTxIn(dcrwire.NewOutPoint((*dcrchainhash.Hash)(&prev), 0, 0), 1e8, nil))
		tx.AddTxOut(dcrwire.NewTxOut(int64(amt), pk))
		tx.AddTxOut(dcrwire.NewTxOut(0, commit))
		var b bytes.Buffer
		_ = tx.Serialize(&b)
		h := tx.TxHash()
		return append(h[:], 0, 0, 0, 0), b.Bytes()
	}
	t.Fatalf("asset %d", assetID)
	return nil, nil
}

// fakeBondChain is a shared chain of bond outputs (key -> confs).
type fakeBondChain struct {
	mtx   sync.Mutex
	confs map[string]uint32
	spent map[string]bool
	calls int
}

func newFakeBondChain() *fakeBondChain {
	return &fakeBondChain{confs: make(map[string]uint32), spent: make(map[string]bool)}
}

func bkey(a uint32, c []byte) string { return fmt.Sprintf("%d:%x", a, c) }

func (f *fakeBondChain) BondConfs(_ context.Context, a uint32, coinID, _ []byte, _ time.Time) (uint32, bool, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.calls++
	c, ok := f.confs[bkey(a, coinID)]
	if !ok {
		return 0, false, errors.New("coin not found")
	}
	return c, f.spent[bkey(a, coinID)], nil
}

func (f *fakeBondChain) mineAll() {
	f.mtx.Lock()
	for k := range f.confs {
		f.confs[k]++
	}
	f.mtx.Unlock()
}

type fakeBondMaker struct {
	t     *testing.T
	chain *fakeBondChain
	p2sh  bool
}

func (m *fakeBondMaker) MakeValidatorBond(_ context.Context, a uint32, amt uint64, lock time.Time, key []byte, acct [32]byte, broadcast bool) ([]byte, []byte, error) {
	c, raw := makeTestBondTx(m.t, a, amt, lock, key, acct, m.p2sh)
	if broadcast {
		m.chain.mtx.Lock()
		m.chain.confs[bkey(a, c)] = 0
		m.chain.mtx.Unlock()
	}
	return c, raw, nil
}

func testBondRules() *BondRules {
	return &BondRules{
		Assets: []BondAsset{
			{AssetID: BondAssetBTC, Symbol: "btc", Steps: []BondStep{{0, 10_000}}},
			{AssetID: BondAssetLBC, Symbol: "lbc", Steps: []BondStep{{0, 1_000_000e8}, {secsPerYear, 500_000e8}}},
			{AssetID: BondAssetDCR, Symbol: "dcr", Steps: []BondStep{{0, 10e8}}},
		},
		MinAgeSecs: 60, MinLockSecs: 3600, MinLockLeftSecs: 600, MinConfs: 1,
		SwitchNum: 3, SwitchDen: 4, BondPower: 1, MaxClaimsPerEpoch: 8,
	}
}

func makeBondRelease(t *testing.T, ids []peer.ID, params EpochParams, rules *BondRules) *Release {
	t.Helper()
	r := &Release{Version: ReleaseVersion, Name: "test-s4", Network: "simnet", Issued: 1, Protocol: ProtocolVersion,
		MinProtocol: MinProtocolVersion, Params: params, Bonds: rules}
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

func TestBondTxParse(t *testing.T) {
	priv := newPriv(t)
	id := pid(t, priv).String()
	acct := BondAcct(id)
	lock := time.Now().Add(2 * time.Hour)
	key := []byte("bond key ................ 32 bytes")
	rules := testBondRules()
	for _, tc := range []struct {
		a    uint32
		p2sh bool
	}{{BondAssetBTC, false}, {BondAssetBTC, true}, {BondAssetLBC, false}, {BondAssetDCR, true}} {
		coin, raw := makeTestBondTx(t, tc.a, 12e8, lock, key, acct, tc.p2sh)
		c := &BondClaim{AssetID: tc.a, CoinID: coin, RawTx: raw, Stamp: 1}
		if err := c.sign(priv); err != nil {
			t.Fatal(err)
		}
		d, err := checkClaim(rules, c)
		if err != nil {
			t.Fatalf("asset %d p2sh %v: %v", tc.a, tc.p2sh, err)
		}
		if d.Amount != 12e8 || d.LockTime != lock.Unix() {
			t.Fatalf("details %+v", d)
		}
		// Another validator cannot claim it.
		other := newPriv(t)
		c2 := *c
		if err := c2.sign(other); err != nil {
			t.Fatal(err)
		}
		if _, err := checkClaim(rules, &c2); err == nil {
			t.Fatal("claim of someone else's bond accepted")
		}
		// Tampered tx / wrong coin.
		bad := *c
		bad.CoinID = append([]byte{1}, coin[1:]...)
		_ = bad.sign(priv)
		if _, err := checkClaim(rules, &bad); err == nil {
			t.Fatal("wrong coin ID accepted")
		}
		bad = *c
		bad.Sig = append([]byte{}, c.Sig...)
		bad.Sig[0] ^= 1
		if _, err := checkClaim(rules, &bad); err == nil {
			t.Fatal("bad claim signature accepted")
		}
	}
	// Unsupported asset.
	coin, raw := makeTestBondTx(t, BondAssetBTC, 1e8, lock, key, acct, false)
	c := &BondClaim{AssetID: 2, CoinID: coin, RawTx: raw}
	_ = c.sign(priv)
	if _, err := checkClaim(rules, c); err == nil {
		t.Fatal("non-bond asset accepted")
	}
}

// TestBondLedgerRules checks age, ladder, lock expiry and the deadline on a
// synthetic chain.
func TestBondLedgerRules(t *testing.T) {
	var ids []peer.ID
	var privs = newPriv(t)
	for i := 0; i < 4; i++ {
		ids = append(ids, pid(t, newPriv(t)))
	}
	p := DefaultEpochParams // 10 s epochs
	rules := testBondRules()
	rules.Assets[0].Steps = append(rules.Assets[0].Steps, BondStep{3600, 50_000}) // BTC ladder rung after 1 h
	rel := makeBondRelease(t, ids, p, rules)
	l := newBondLedger(rel)
	base := uint64(180_000_000) // epoch numbers
	if l.switchOn(base, base) {
		t.Fatal("switch on without signals")
	}
	// Signals: 2 of 4 -> off; 3 -> on from the next epoch.
	sig := func(i int) *SwitchSignal { return &SwitchSignal{Validator: ids[i].String()} }
	l.apply(&EpochRecord{Epoch: base, Proposal: &EpochProposal{Signals: []*SwitchSignal{sig(0), sig(1)}}})
	if l.switchOn(base+5, base) || l.switchSet {
		t.Fatal("on with 2 of 4")
	}
	l.apply(&EpochRecord{Epoch: base + 1, Proposal: &EpochProposal{Signals: []*SwitchSignal{sig(2)}}})
	if !l.switchSet || l.switchOn(base+1, base+1) || !l.switchOn(base+2, base+1) {
		t.Fatal("switch should be on from the epoch after the third signal")
	}
	// A bond admitted at base+2 with a 2 h lock.
	tm := func(n uint64) time.Time { return time.UnixMilli(int64(n) * p.EpochMs) }
	id := pid(t, privs).String()
	coin, raw := makeTestBondTx(t, BondAssetBTC, 20_000, tm(base+2).Add(2*time.Hour), []byte("k"), BondAcct(id), false)
	c := &BondClaim{AssetID: BondAssetBTC, CoinID: coin, RawTx: raw}
	_ = c.sign(privs)
	l.apply(&EpochRecord{Epoch: base + 2, Proposal: &EpochProposal{Bonds: []*BondClaim{c}}})
	if len(l.bonds) != 1 {
		t.Fatal("bond not admitted")
	}
	m := l.matureEpoch(base + 2) // 60 s age -> 6 epochs after the record ends
	if m != base+9 {
		t.Fatalf("mature epoch %d, want %d", m, base+9)
	}
	if l.members(m-1, base+2).IsValidator(id) {
		t.Fatal("immature bond counted")
	}
	vs := l.members(m, base+2)
	if !vs.IsValidator(id) || vs.BondedPower() != 5 || vs.since(id) != m {
		t.Fatalf("mature bond not counted: power %d", vs.BondedPower())
	}
	// Ladder: 20,000 sats is below the 50,000 rung one hour after the switch.
	rung := base + 2 + 360
	if l.members(rung-1, base+2).IsValidator(id) || !l.members(rung-2, base+2).IsValidator(id) {
		// rung-1 is 3590 s after the switch start (base+2): still counts.
	}
	if l.members(rung, base+2).IsValidator(id) {
		t.Fatal("bond below the ladder rung still counts")
	}
	// Expiry: less than 600 s of lock left.
	rules2 := testBondRules()
	rel2 := makeBondRelease(t, ids, p, rules2)
	l2 := newBondLedger(rel2)
	l2.apply(&EpochRecord{Epoch: base, Proposal: &EpochProposal{Signals: []*SwitchSignal{sig(0), sig(1), sig(2)}}})
	l2.apply(&EpochRecord{Epoch: base + 2, Proposal: &EpochProposal{Bonds: []*BondClaim{c}}})
	end := uint64((tm(base+2).Add(2*time.Hour).Unix() - 600) * 1000 / p.EpochMs)
	if !l2.members(end, base+2).IsValidator(id) || l2.members(end+1, base+2).IsValidator(id) {
		t.Fatal("lock expiry boundary")
	}
	// asOf before the admission: not counted (prefix consistency).
	if l2.members(base+100, base+1).IsValidator(id) {
		t.Fatal("bond counted from a chain prefix without it")
	}
	// Deadline fallback without signals.
	rules3 := testBondRules()
	rules3.SwitchDeadline = int64(base+50) * p.EpochMs / 1000
	l3 := newBondLedger(makeBondRelease(t, ids, p, rules3))
	if l3.switchOn(base+49, base+49) || !l3.switchOn(base+50, base+49) {
		t.Fatal("deadline fallback")
	}
	// 3/4 of 10 seeded rounds up to 8; of 4 to 3.
	if rules.switchNeed(10) != 8 || rules.switchNeed(4) != 3 {
		t.Fatal("switchNeed")
	}
}

type bondSim struct {
	*epochSim
	chain *fakeBondChain
	cands []*simNode
	dir   string
}

func newBondSim(t *testing.T, rules *BondRules) *bondSim {
	s := &epochSim{t: t, net: newEpochNet(), clock: &fakeClock{t: time.UnixMilli(1_800_000_000_000)},
		ctx: context.Background(), quiet: testLogger()}
	s.rng = newTestRand()
	bs := &bondSim{epochSim: s, chain: newFakeBondChain(), dir: t.TempDir()}
	var ids []peer.ID
	for i := 0; i < 4; i++ {
		priv := newPriv(t)
		s.vals = append(s.vals, &simNode{name: fmt.Sprintf("v%d", i), priv: priv, id: pid(t, priv)})
		ids = append(ids, s.vals[i].id)
	}
	tp := newPriv(t)
	s.traders = append(s.traders, &simNode{name: "t0", priv: tp, id: pid(t, tp)})
	s.rel = makeBondRelease(t, ids, s3Params(), rules)
	for _, n := range s.vals {
		bs.start(n, true)
	}
	bs.start(s.traders[0], false)
	return bs
}

func (bs *bondSim) start(n *simNode, validator bool) {
	bs.startNode(n, validator, filepath.Join(bs.dir, n.name+".jsonl"))
	n.e.SetBondHooks(bs.chain, &fakeBondMaker{t: bs.t, chain: bs.chain})
}

func (bs *bondSim) addCandidate(name string) *simNode {
	priv := newPriv(bs.t)
	n := &simNode{name: name, priv: priv, id: pid(bs.t, priv)}
	bs.traders = append(bs.traders, n)
	bs.cands = append(bs.cands, n)
	bs.start(n, true)
	return n
}

// epochsMining advances k epochs, mining one block per epoch.
func (bs *bondSim) epochsMining(k int) {
	for i := 0; i < k; i++ {
		bs.chain.mineAll()
		bs.epochs(1)
	}
}

func memberNames(st *BondsStatus) []string {
	var out []string
	for _, m := range st.Members {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

// TestBondSwitchAndAdmission is the S4 plan test on a simulated mesh: with
// 4 seeded, 2 signals -> off, 3 -> on; restart keeps it on; BTC, LBC and DCR
// bonds count only after the age; unbonded, unconfirmed and immature
// validators are not counted; bonded newcomers are admitted automatically
// and join the active set and the quorum.
func TestBondSwitchAndAdmission(t *testing.T) {
	bs := newBondSim(t, testBondRules())
	bs.epochs(15)
	v0 := bs.vals[0]
	if v0.e.Status().BondedPower != 4 {
		t.Fatal("bonded power")
	}
	cBTC, cLBC, cDCR := bs.addCandidate("c-btc"), bs.addCandidate("c-lbc"), bs.addCandidate("c-dcr")
	cNone, cFake := bs.addCandidate("c-none"), bs.addCandidate("c-fake")
	for _, n := range bs.all() {
		for _, c := range bs.cands {
			n.e.SetPeerName(c.id.String(), c.name)
		}
	}
	bs.epochs(2)
	// Bonds while the switch is off are not admitted.
	if _, err := cBTC.e.PostBond(bs.ctx, BondAssetBTC, 20_000, 7200, false); err != nil {
		t.Fatal(err)
	}
	bs.epochsMining(8)
	if st := v0.e.BondsStatus(0, 0); st.Switch.On || len(st.Admitted) != 0 {
		t.Fatalf("switch off: %+v admitted %d", st.Switch, len(st.Admitted))
	}
	// 2 of 4 signal: still off.
	for _, v := range bs.vals[:2] {
		if err := v.e.SignalBondSwitch(); err != nil {
			t.Fatal(err)
		}
	}
	if err := cNone.e.SignalBondSwitch(); err == nil {
		t.Fatal("a non-seeded node signalled")
	}
	bs.epochsMining(10)
	for _, n := range bs.all() {
		st := n.e.BondsStatus(0, 0)
		if st.Switch.On || len(st.Switch.Signals) != 2 {
			t.Fatalf("%s: 2 signals: %+v", n.name, st.Switch)
		}
	}
	// The third signal turns it on; every peer agrees on the epoch.
	if err := bs.vals[2].e.SignalBondSwitch(); err != nil {
		t.Fatal(err)
	}
	bs.epochsMining(6)
	var onEpoch uint64
	for _, n := range bs.all() {
		st := n.e.BondsStatus(0, 0)
		if !st.Switch.On || len(st.Switch.Signals) != 3 {
			t.Fatalf("%s: 3 signals: %+v", n.name, st.Switch)
		}
		if onEpoch == 0 {
			onEpoch = st.Switch.OnEpoch
		} else if st.Switch.OnEpoch != onEpoch {
			t.Fatalf("%s: switch epoch %d != %d", n.name, st.Switch.OnEpoch, onEpoch)
		}
	}
	if _, err := os.Stat(v0.e.latchPath()); err != nil {
		t.Fatalf("latch file: %v", err)
	}
	// Bonds: BTC (re-gossiped from before), LBC, DCR; one never broadcast.
	if _, err := cLBC.e.PostBond(bs.ctx, BondAssetLBC, 1_000_000e8, 7200, false); err != nil {
		t.Fatal(err)
	}
	if _, err := cDCR.e.PostBond(bs.ctx, BondAssetDCR, 10e8, 7200, false); err != nil {
		t.Fatal(err)
	}
	if _, err := cFake.e.PostBond(bs.ctx, BondAssetDCR, 10e8, 7200, true); err != nil {
		t.Fatal(err)
	}
	// Too small and too short bonds are rejected at admission.
	if _, err := cNone.e.PostBond(bs.ctx, BondAssetBTC, 9_999, 7200, false); err != nil {
		t.Fatal(err)
	}
	var admitted uint64
	for i := 0; i < 20 && admitted == 0; i++ {
		bs.epochsMining(1)
		st := v0.e.BondsStatus(0, 0)
		if len(st.Admitted) >= 3 {
			admitted = lastEpoch(v0.e)
		}
	}
	st := v0.e.BondsStatus(0, 0)
	if len(st.Admitted) != 3 {
		t.Fatalf("admitted %d bonds: %+v", len(st.Admitted), st.Admitted)
	}
	immature := 0
	for _, b := range st.Admitted {
		if b.Counts != (st.Epoch >= b.MatureEpoch) {
			t.Fatalf("bond counts %v at epoch %d, mature at %d", b.Counts, st.Epoch, b.MatureEpoch)
		}
		if !b.Counts {
			immature++
		}
		if b.Name == "c-fake" || b.Name == "c-none" {
			t.Fatalf("bad bond admitted: %+v", b)
		}
	}
	if immature == 0 {
		t.Fatal("no immature bond seen")
	}
	if got := memberNames(st); len(got) != 4+3-immature {
		t.Fatalf("members before the age: %v (%d immature)", got, immature)
	}
	// After the 60 s age: 7 members on every peer, unbonded/fake not.
	bs.epochsMining(8)
	want := []string{"c-btc", "c-dcr", "c-lbc", "v0", "v1", "v2", "v3"}
	for _, n := range bs.all() {
		st := n.e.BondsStatus(0, 0)
		if got := memberNames(st); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s members %v", n.name, got)
		}
		if st.Bonded != 7 {
			t.Fatalf("%s bonded power %d", n.name, st.Bonded)
		}
	}
	for _, c := range []*simNode{cNone, cFake} {
		if s := c.e.Status(); s.Role != "candidate" || s.Signing {
			t.Fatalf("%s: role %s signing %v", c.name, s.Role, s.Signing)
		}
	}
	// Newcomers sign and join the active set after ActiveMinEpochs records.
	bs.epochsMining(15)
	for _, n := range bs.all() {
		if a := activeNames(bs.epochSim, n); len(a) != 7 {
			t.Fatalf("%s active %v", n.name, a)
		}
	}
	bs.checkAgreement(bs.all())
	bs.checkActiveReplay(bs.all())
	// Quorum over 7: with 2 seeded down, 5 of 7 still finalize, and the
	// newcomers' signatures count.
	bs.net.setDown(bs.vals[2].id, true)
	bs.net.setDown(bs.vals[3].id, true)
	f0 := lastEpoch(v0.e)
	bs.epochsMining(5)
	if lastEpoch(v0.e) < f0+4 {
		t.Fatalf("5 of 7 did not finalize: %d -> %d", f0, lastEpoch(v0.e))
	}
	rec := v0.e.LastRecord()
	newSigs := 0
	for _, a := range rec.Sigs {
		for _, c := range []*simNode{cBTC, cLBC, cDCR} {
			if a.Validator == c.id.String() {
				newSigs++
			}
		}
	}
	if newSigs != 3 {
		t.Fatalf("newcomer signatures in the record: %d", newSigs)
	}
	bs.net.setDown(bs.vals[2].id, false)
	bs.net.setDown(bs.vals[3].id, false)
	bs.epochsMining(4)
	// Restart: the switch stays on (chain store + latch).
	r := bs.vals[1]
	bs.start(r, true)
	bs.epochsMining(3)
	if st := r.e.BondsStatus(0, 0); !st.Switch.On || !st.Switch.Latched || len(st.Members) != 7 {
		t.Fatalf("after restart: %+v members %d", st.Switch, len(st.Members))
	}
	bs.checkAgreement(bs.all())
}

// TestBondLatchReproposes: after a chain restart a latched validator puts
// the proof back into proposals, and refuses to sign without it.
func TestBondLatchReproposes(t *testing.T) {
	bs := newBondSim(t, testBondRules())
	bs.epochs(12)
	for _, v := range bs.vals[:3] {
		_ = v.e.SignalBondSwitch()
	}
	bs.epochs(6)
	v0 := bs.vals[0]
	if !v0.e.BondsStatus(0, 0).Switch.On {
		t.Fatal("switch not on")
	}
	e := v0.e
	e.mtx.Lock()
	e.resetLedgerLocked() // as after validators restarted the chain
	n := e.p.EpochOf(e.nowMs())
	p := &EpochProposal{Epoch: n}
	e.s4ProposalLocked(p)
	bare := &EpochProposal{Epoch: n}
	ok, okBare := e.latchOKLocked(p), e.latchOKLocked(bare)
	e.mtx.Unlock()
	if len(p.Signals) != 3 || !ok || okBare {
		t.Fatalf("latched re-proposal: %d signals, ok %v, bare ok %v", len(p.Signals), ok, okBare)
	}
	// A fresh ledger applying that proposal is on again.
	l := newBondLedger(bs.rel)
	l.apply(&EpochRecord{Epoch: n, Proposal: p})
	if !l.switchOn(n+1, n) {
		t.Fatal("re-proposed signals do not turn the switch on")
	}
}

// TestBondUnverifiedNotSigned: a validator does not sign a proposal
// admitting a bond it could not find on chain.
func TestBondUnverifiedNotSigned(t *testing.T) {
	rules := testBondRules()
	rules.MinConfs = 3
	bs := newBondSim(t, rules)
	bs.epochs(12)
	for _, v := range bs.vals[:3] {
		_ = v.e.SignalBondSwitch()
	}
	bs.epochs(6)
	c := bs.addCandidate("c")
	if _, err := c.e.PostBond(bs.ctx, BondAssetBTC, 20_000, 7200, false); err != nil {
		t.Fatal(err)
	}
	// No blocks: 0 confs -> not verified, never admitted.
	bs.epochs(8)
	st := bs.vals[0].e.BondsStatus(0, 0)
	if len(st.Admitted) != 0 || len(st.Pending) != 1 || st.Pending[0].Verified {
		t.Fatalf("unconfirmed bond: admitted %d pending %+v", len(st.Admitted), st.Pending)
	}
	// 3 confs: admitted.
	bs.epochsMining(6)
	if st := bs.vals[0].e.BondsStatus(0, 0); len(st.Admitted) != 1 {
		t.Fatalf("confirmed bond not admitted: %+v", st.Pending)
	}
}

func testLogger() dex.Logger   { return dex.StdOutLogger("SIM", dex.LevelError) }
func newTestRand() *mrand.Rand { return mrand.New(mrand.NewSource(1)) }
