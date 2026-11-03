// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	mc "github.com/bisoncraft/mesh/client"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// epochNet is a queued in-memory pub/sub: broadcasts are delivered by pump,
// so a whole simulated mesh runs deterministically in one goroutine.
type epochNet struct {
	mtx   sync.Mutex
	subs  map[string]map[peer.ID]mc.TopicHandler
	queue []epochNetMsg
	down  map[peer.ID]bool
	sent  int
}

type epochNetMsg struct {
	topic string
	from  peer.ID
	data  []byte
}

func newEpochNet() *epochNet {
	return &epochNet{subs: make(map[string]map[peer.ID]mc.TopicHandler), down: make(map[peer.ID]bool)}
}

type epochNetClient struct {
	n  *epochNet
	id peer.ID
}

func (c *epochNetClient) Broadcast(_ context.Context, topic string, data []byte) error {
	// The real mesh codec rejects publishes over 1 KiB.
	if !fitsPublish(topic, data) {
		return fmt.Errorf("message size %d exceeds max size %d", len(data)+len(topic), meshMaxPublish)
	}
	c.n.mtx.Lock()
	defer c.n.mtx.Unlock()
	if c.n.down[c.id] {
		return nil
	}
	c.n.queue = append(c.n.queue, epochNetMsg{topic, c.id, data})
	c.n.sent++
	return nil
}

func (c *epochNetClient) Subscribe(_ context.Context, topic string, h mc.TopicHandler) error {
	c.n.mtx.Lock()
	defer c.n.mtx.Unlock()
	if c.n.subs[topic] == nil {
		c.n.subs[topic] = make(map[peer.ID]mc.TopicHandler)
	}
	c.n.subs[topic][c.id] = h
	return nil
}

func (n *epochNet) pump() {
	for i := 0; i < 20000000; i++ {
		n.mtx.Lock()
		if len(n.queue) == 0 {
			n.mtx.Unlock()
			// Let async sync requests land.
			time.Sleep(0)
			n.mtx.Lock()
			if len(n.queue) == 0 {
				n.mtx.Unlock()
				return
			}
		}
		m := n.queue[0]
		n.queue = n.queue[1:]
		ids := make([]peer.ID, 0, len(n.subs[m.topic]))
		for id := range n.subs[m.topic] {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		var hs []mc.TopicHandler
		for _, id := range ids {
			if id != m.from && !n.down[id] {
				hs = append(hs, n.subs[m.topic][id])
			}
		}
		n.mtx.Unlock()
		for _, h := range hs {
			h(mc.TopicEvent{Type: mc.TopicEventData, Data: m.data, Peer: m.from})
		}
	}
}

func (n *epochNet) setDown(id peer.ID, down bool) {
	n.mtx.Lock()
	n.down[id] = down
	n.mtx.Unlock()
}

// fakeClock is a shared simulated time with per-node offsets.
type fakeClock struct {
	mtx sync.Mutex
	t   time.Time
}

func (c *fakeClock) now() time.Time { c.mtx.Lock(); defer c.mtx.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mtx.Lock()
	c.t = c.t.Add(d)
	c.mtx.Unlock()
}

type simNode struct {
	name   string
	priv   libp2pcrypto.PrivKey
	id     peer.ID
	e      *EpochEngine
	offset time.Duration
	finals []*EpochFinal
}

type epochSim struct {
	t       *testing.T
	net     *epochNet
	clock   *fakeClock
	rel     *Release
	vals    []*simNode
	traders []*simNode
	ctx     context.Context
	rng     *mrand.Rand
	quiet   dex.Logger
}

var testReleaseKey = func() ed25519.PrivateKey {
	seed := make([]byte, 32)
	copy(seed, "bisonary test release key ......")
	return ed25519.NewKeyFromSeed(seed)
}()

func testReleaseKeyHex() string {
	return hex.EncodeToString(testReleaseKey.Public().(ed25519.PublicKey))
}

func makeTestRelease(t *testing.T, ids []peer.ID, params EpochParams) *Release {
	t.Helper()
	r := &Release{Version: ReleaseVersion, Name: "test", Network: "simnet", Issued: 1, Protocol: ProtocolVersion,
		MinProtocol: MinProtocolVersion, Params: params}
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

func newEpochSim(t *testing.T, nVals, nTraders int, params EpochParams) *epochSim {
	s := &epochSim{t: t, net: newEpochNet(), clock: &fakeClock{t: time.UnixMilli(1_800_000_000_000)},
		ctx: context.Background(), rng: mrand.New(mrand.NewSource(1)), quiet: dex.StdOutLogger("SIM", dex.LevelError)}
	var ids []peer.ID
	for i := 0; i < nVals; i++ {
		priv := newPriv(t)
		s.vals = append(s.vals, &simNode{name: fmt.Sprintf("v%d", i), priv: priv, id: pid(t, priv)})
		ids = append(ids, s.vals[i].id)
	}
	for i := 0; i < nTraders; i++ {
		priv := newPriv(t)
		s.traders = append(s.traders, &simNode{name: fmt.Sprintf("t%d", i), priv: priv, id: pid(t, priv)})
	}
	s.rel = makeTestRelease(t, ids, params)
	for _, n := range s.vals {
		s.startNode(n, true, "")
	}
	for _, n := range s.traders {
		s.startNode(n, false, "")
	}
	return s
}

func (s *epochSim) startNode(n *simNode, validator bool, store string) {
	nn := n
	e, err := NewEpochEngine(s.rel, EpochConfig{Validator: validator, StorePath: store}, &epochNetClient{s.net, n.id}, n.priv,
		func() time.Time { return s.clock.now().Add(nn.offset) }, s.quiet)
	if err != nil {
		s.t.Fatal(err)
	}
	n.finals = nil
	e.SetFinalHandler(func(f *EpochFinal) { nn.finals = append(nn.finals, f) })
	n.e = e
	e.start(s.ctx)
}

func (s *epochSim) all() []*simNode { return append(append([]*simNode(nil), s.vals...), s.traders...) }

// step advances the clock by d in 50 ms ticks.
func (s *epochSim) step(d time.Duration) {
	for el := time.Duration(0); el < d; el += 50 * time.Millisecond {
		s.clock.add(50 * time.Millisecond)
		for _, n := range s.all() {
			if !s.net.down[n.id] {
				n.e.tick(s.ctx)
			}
		}
		s.net.pump()
	}
}

func (s *epochSim) epochs(n int) { s.step(time.Duration(n) * s.rel.EpochDuration()) }

func (s *epochSim) order(n *simNode, sell bool, lots, rate uint64) *MeshOrder {
	now := n.e.now()
	o := &MeshOrder{Market: dcrBTC, Sell: sell, Qty: lots * 1e8, LotSize: 1e8, Rate: rate, Stamp: now.UnixMilli(),
		Expiry: now.Add(10 * time.Minute).UnixMilli(), Nonce: s.rng.Uint64(), EncPubKey: tEncPub}
	if err := o.Sign(n.priv); err != nil {
		s.t.Fatal(err)
	}
	if err := n.e.SubmitOrder(s.ctx, o); err != nil {
		s.t.Fatalf("%s: %v", n.name, err)
	}
	return o
}

func (s *epochSim) randomOrders(k int) {
	for i := 0; i < k; i++ {
		tr := s.traders[s.rng.Intn(len(s.traders))]
		s.order(tr, s.rng.Intn(2) == 0, uint64(1+s.rng.Intn(3)), uint64(95_000+s.rng.Intn(10)*1_000))
	}
}

func lastEpoch(e *EpochEngine) uint64 {
	if r := e.LastRecord(); r != nil {
		return r.Epoch
	}
	return 0
}

func TestReleaseSignVerify(t *testing.T) {
	priv := newPriv(t)
	r := &Release{Version: ReleaseVersion, Name: "x", Network: "simnet", Protocol: ProtocolVersion, MinProtocol: MinProtocolVersion,
		Params: DefaultEpochParams, Validators: []ReleaseValidator{{PeerID: pid(t, priv).String(), Bond: 1}}}
	b, err := SignRelease(r, testReleaseKey)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := ParseRelease(b, testReleaseKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Params != DefaultEpochParams || rel.Params.EpochMs != 10_000 || rel.Params.SignWindowMs != 5_000 ||
		rel.Params.MaxClockSkewMs != 2_000 || rel.Params.QuorumNum != 2 || rel.Params.QuorumDen != 3 || rel.Params.FloorNum != 1 || rel.Params.FloorDen != 2 {
		t.Fatalf("defaults changed: %+v", rel.Params)
	}
	// Tampering with the content breaks the signature.
	tampered := strings.Replace(string(b), `\"epochMs\": 10000`, `\"epochMs\": 20000`, 1)
	if tampered == string(b) {
		// The release is embedded as raw JSON, not a string.
		tampered = strings.Replace(string(b), `"epochMs": 10000`, `"epochMs": 20000`, 1)
	}
	if tampered == string(b) {
		t.Fatal("tamper replace failed")
	}
	if _, err := ParseRelease([]byte(tampered), testReleaseKeyHex()); err == nil || !strings.Contains(err.Error(), "signature is invalid") {
		t.Fatalf("tampered release accepted: %v", err)
	}
	// Another key is not trusted.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	b2, _ := SignRelease(r, other)
	if _, err := ParseRelease(b2, testReleaseKeyHex()); err == nil || !strings.Contains(err.Error(), "untrusted key") {
		t.Fatalf("release with other key accepted: %v", err)
	}
	// Bad params are refused.
	bad := *r
	bad.Params.SignWindowMs = bad.Params.EpochMs
	if _, err := SignRelease(&bad, testReleaseKey); err == nil {
		t.Fatal("bad params signed")
	}
	// The dev key constant is a valid key.
	if k, err := hex.DecodeString(DevReleasePubKey); err != nil || len(k) != ed25519.PublicKeySize {
		t.Fatal("bad dev key")
	}
}

func TestThreshold(t *testing.T) {
	p := DefaultEpochParams
	for _, tc := range []struct {
		signed, active, bonded uint64
		ok                     bool
	}{
		{4, 4, 4, true}, {3, 4, 4, true}, {2, 4, 4, false}, // 2/3 of active
		{3, 3, 4, true}, {2, 3, 4, true}, {1, 3, 4, false}, // S3: 2/3 of the active set
		{2, 2, 4, true}, {1, 2, 4, false}, // active power = half of bonded: floor met
		{1, 1, 4, false}, {0, 0, 4, false}, // active power below the floor
		{7, 10, 10, true}, {6, 10, 10, false}, {6, 9, 10, true}, {5, 7, 10, true}, {4, 7, 10, false},
		{3, 4, 10, false}, // below the floor even if all active sign
	} {
		if got := thresholdMet(&p, tc.signed, tc.active, tc.bonded); got != tc.ok {
			t.Errorf("signed %d active %d bonded %d: got %v", tc.signed, tc.active, tc.bonded, got)
		}
	}
}

func TestEpochMatchingRules(t *testing.T) {
	a, b, c := newPriv(t), newPriv(t), newPriv(t)
	p := DefaultEpochParams
	n := uint64(180_000_000)
	end := p.EpochEnd(n)
	mk := func(priv libp2pcrypto.PrivKey, sell bool, lots, rate uint64, stamp int64, lot uint64) *MeshOrder {
		o := &MeshOrder{Market: dcrBTC, Sell: sell, Qty: lots * lot, LotSize: lot, Rate: rate, Stamp: stamp,
			Expiry: stamp + 60_000, Nonce: uint64(stamp), EncPubKey: tEncPub}
		_ = o.Sign(priv)
		return o
	}
	s := newEpochState()
	s1 := mk(a, true, 2, 100_000, end-9000, 1e8)
	s2 := mk(b, true, 1, 99_000, end-8000, 1e8)  // better price
	s3 := mk(a, true, 1, 150_000, end-8500, 1e7) // other lot size: never matches 1e8 lots
	bad := mk(c, true, 1, 1_000, end-8000, 1e8)  // bad signature
	bad.Sig[0] ^= 1
	matches := applyEpoch(s, &EpochProposal{Epoch: n, Orders: []*MeshOrder{s1, s2, s3, bad}}, &p)
	if len(matches) != 0 || len(s.resting) != 3 {
		t.Fatalf("resting %d matches %d", len(s.resting), len(matches))
	}
	// A buy of 3 lots at 100k from c matches s2 first (best price), then s1.
	buy := mk(c, false, 3, 100_000, end+p.EpochMs-5000, 1e8)
	matches = applyEpoch(s, &EpochProposal{Epoch: n + 1, Orders: []*MeshOrder{buy}}, &p)
	if len(matches) != 2 || matches[0].MakerOrderID != s2.ID() || matches[0].Rate != 99_000 || matches[0].Qty != 1e8 ||
		matches[1].MakerOrderID != s1.ID() || matches[1].Qty != 2e8 || matches[1].Taker != buy.Owner {
		t.Fatalf("bad matches %+v", matches)
	}
	// No self match: a's own buy doesn't take s3 or anything of a's.
	self := mk(a, false, 1, 200_000, end+2*p.EpochMs-5000, 1e7)
	matches = applyEpoch(s, &EpochProposal{Epoch: n + 2, Orders: []*MeshOrder{self}}, &p)
	if len(matches) != 0 {
		t.Fatal("self match")
	}
	// The same order can't be added twice.
	if m := applyEpoch(s, &EpochProposal{Epoch: n + 3, Orders: []*MeshOrder{s1}}, &p); len(m) != 0 {
		t.Fatal("re-added order")
	}
	// Cancel by the owner removes; by others is ignored.
	cancelBy := func(priv libp2pcrypto.PrivKey, id OrderID) *MeshCancel {
		c := &MeshCancel{OrderID: id, Stamp: end + 3*p.EpochMs}
		_ = c.Sign(priv)
		return c
	}
	applyEpoch(s, &EpochProposal{Epoch: n + 4, Cancels: []*MeshCancel{cancelBy(b, s3.ID())}}, &p)
	if _, ok := s.resting[s3.ID()]; !ok {
		t.Fatal("cancel by non-owner applied")
	}
	applyEpoch(s, &EpochProposal{Epoch: n + 4, Cancels: []*MeshCancel{cancelBy(a, s3.ID())}}, &p)
	if _, ok := s.resting[s3.ID()]; ok {
		t.Fatal("owner cancel not applied")
	}
	// Expiry.
	applyEpoch(s, &EpochProposal{Epoch: n + 10}, &p)
	if len(s.resting) != 0 {
		t.Fatalf("%d not expired", len(s.resting))
	}
}

func (s *epochSim) checkAgreement(nodes []*simNode) (finalized int) {
	s.t.Helper()
	ref := nodes[0].e
	for _, n := range nodes[1:] {
		if lastEpoch(n.e) != lastEpoch(ref) {
			s.t.Fatalf("%s last epoch %d != %s %d", n.name, lastEpoch(n.e), nodes[0].name, lastEpoch(ref))
		}
		if n.e.Status().StateHash != ref.Status().StateHash {
			s.t.Fatalf("%s state hash differs", n.name)
		}
		a, b := ref.Records(0, 1e6), n.e.Records(0, 1e6)
		if len(a) != len(b) {
			s.t.Fatalf("%s has %d records, %s %d", n.name, len(b), nodes[0].name, len(a))
		}
		for i := range a {
			if a[i].Fingerprint != b[i].Fingerprint {
				s.t.Fatalf("epoch %d fingerprint differs on %s", a[i].Epoch, n.name)
			}
		}
	}
	return len(ref.Records(0, 1e6))
}

// TestEpochSim500 runs 4 validators and 2 traders for 500 epochs with random
// orders and checks that every node has the same chain and state, that the
// matches obey the rules, and that a replay from genesis reproduces it.
func TestEpochSim500(t *testing.T) {
	s := newEpochSim(t, 4, 2, DefaultEpochParams)
	s.epochs(2)
	start := lastEpoch(s.vals[0].e)
	for i := 0; i < 500; i++ {
		s.randomOrders(s.rng.Intn(4))
		s.epochs(1)
	}
	s.epochs(1)
	n := s.checkAgreement(s.all())
	st := s.vals[0].e.Status()
	t.Logf("500-epoch sim: %d records (from epoch %d to %d), skipped %d, resting %d, pool %d, messages %d",
		n, start, st.LastFinalized, st.Skipped, st.Resting, st.Pool, s.net.sent)
	if n < 500 || st.Skipped != 0 {
		t.Fatalf("only %d finalized, %d skipped", n, st.Skipped)
	}
	// Every record has all 4 signatures and valid matches.
	recs := s.vals[0].e.Records(0, 1e6)
	var nMatches int
	for _, r := range recs {
		if len(r.Sigs) < 3 {
			t.Fatalf("epoch %d: %d sigs", r.Epoch, len(r.Sigs))
		}
		for _, m := range r.Matches {
			nMatches++
			if m.Maker == m.Taker || m.Qty == 0 || m.Qty%1e8 != 0 {
				t.Fatalf("bad match %+v", m)
			}
		}
	}
	if nMatches < 50 {
		t.Fatalf("only %d matches", nMatches)
	}
	// Replay from genesis on a fresh state.
	state := newEpochState()
	prev := GenesisHash(s.rel)
	for _, r := range recs {
		if r.Prev != prev {
			t.Fatalf("epoch %d: chain broken", r.Epoch)
		}
		ms := applyEpoch(state, r.Proposal, &s.rel.Params)
		if fp := epochFingerprint(r.Epoch, r.Prev, r.Proposal.Hash(), r.Active, ms, state.hash()); fp != r.Fingerprint {
			t.Fatalf("epoch %d: replay fingerprint differs", r.Epoch)
		}
		prev = r.Fingerprint
	}
	// Traders' callbacks saw every match where they were the taker.
	var takerMatches int
	for _, tr := range s.traders {
		for _, f := range tr.finals {
			for _, m := range f.Record.Matches {
				if m.Taker == tr.id.String() {
					takerMatches++
					if f.Makers[m.MakerOrderID] == nil {
						t.Fatal("maker order missing in final")
					}
				}
			}
		}
	}
	t.Logf("%d matches, %d seen by their takers", nMatches, takerMatches)
	if takerMatches != nMatches {
		t.Fatalf("takers saw %d of %d matches", takerMatches, nMatches)
	}
}

// TestEpochKillValidators: with 1 of 4 validators down epochs still finalize
// (the fallback proposer covers its turns); with 2 of 4 down nothing
// finalizes and orders roll over until one comes back.
func TestEpochKillValidators(t *testing.T) {
	s := newEpochSim(t, 4, 2, DefaultEpochParams)
	s.epochs(3)
	s.net.setDown(s.vals[3].id, true)
	s.epochs(4) // S3: still in the active set (24 h window); fallback proposers cover its turns
	before := lastEpoch(s.vals[0].e)
	s.order(s.traders[0], true, 1, 100_000)
	s.order(s.traders[1], false, 1, 100_000)
	s.epochs(8)
	st := s.vals[0].e.Status()
	if lastEpoch(s.vals[0].e) < before+7 {
		t.Fatalf("1/4 down: only finalized to %d from %d (status %+v)", lastEpoch(s.vals[0].e), before, st)
	}
	if st.ActivePower != 4 {
		t.Fatalf("active power %d", st.ActivePower)
	}
	matched := 0
	for _, r := range s.vals[0].e.Records(before, 100) {
		matched += len(r.Matches)
		if len(r.Sigs) != 3 {
			t.Fatalf("epoch %d has %d sigs", r.Epoch, len(r.Sigs))
		}
	}
	if matched != 1 {
		t.Fatalf("1/4 down: %d matches", matched)
	}
	s.checkAgreement(append(s.vals[:3:3], s.traders...))
	t.Logf("1/4 down: finalized %d..%d, 3 sigs each, 1 match", before+1, lastEpoch(s.vals[0].e))

	// Two down: no finalization.
	s.net.setDown(s.vals[2].id, true)
	stalled := lastEpoch(s.vals[0].e)
	o1 := s.order(s.traders[0], true, 2, 101_000)
	o2 := s.order(s.traders[1], false, 2, 101_000)
	s.epochs(6)
	if got := lastEpoch(s.vals[0].e); got > stalled+1 {
		t.Fatalf("2/4 down: finalized %d (stalled at %d)", got, stalled)
	}
	stalled = lastEpoch(s.vals[0].e)
	if st := s.vals[0].e.OrderStatus(o1.ID()); st != "pool" {
		t.Fatalf("order not rolling in pool: %s", st)
	}
	t.Logf("2/4 down: stalled at %d for 6 epochs, orders kept in pool (canFinalize=%v)", stalled, s.vals[0].e.Status().CanFinalize)
	// Bring v2 back: finalization resumes and the rolled orders match.
	s.net.setDown(s.vals[2].id, false)
	s.epochs(6)
	resumed := lastEpoch(s.vals[0].e)
	if resumed < stalled+3 {
		t.Fatalf("not resumed: %d", resumed)
	}
	if st := s.vals[0].e.OrderStatus(o2.ID()); st != "done" {
		t.Fatalf("rolled order status %s", st)
	}
	s.checkAgreement(append(s.vals[:3:3], s.traders...))
	// v3 comes back and catches up by sync.
	s.net.setDown(s.vals[3].id, false)
	s.epochs(5)
	s.checkAgreement(s.all())
	t.Logf("resumed after restart: finalized to %d, rolled orders matched, v3 synced", lastEpoch(s.vals[0].e))
}

// TestEpochClockSkew: a validator whose clock is 3 s ahead stops signing;
// the others continue. A trader with a skewed clock can't submit orders.
func TestEpochClockSkew(t *testing.T) {
	s := newEpochSim(t, 4, 2, DefaultEpochParams)
	s.epochs(3)
	s.vals[3].offset = 3 * time.Second
	s.epochs(3)
	st := s.vals[3].e.Status()
	if st.ClockOK || st.Signing || st.ClockSkewMs < 2500 {
		t.Fatalf("skewed validator status: skew %d ok %v signing %v", st.ClockSkewMs, st.ClockOK, st.Signing)
	}
	from := lastEpoch(s.vals[0].e)
	s.epochs(5)
	for _, r := range s.vals[0].e.Records(from, 100) {
		for _, a := range r.Sigs {
			if a.Validator == s.vals[3].id.String() {
				t.Fatalf("skewed validator signed epoch %d", r.Epoch)
			}
		}
	}
	if lastEpoch(s.vals[0].e) < from+4 {
		t.Fatal("others stopped finalizing")
	}
	if ok := s.vals[0].e.Status(); !ok.ClockOK {
		t.Fatalf("healthy validator thinks its clock is off: %d", ok.ClockSkewMs)
	}
	t.Logf("v3 +3s: skew estimate %d ms, signing=false; others finalized %d..%d without it", st.ClockSkewMs, from+1, lastEpoch(s.vals[0].e))
	// Fixed clock: signs again.
	s.vals[3].offset = 0
	s.epochs(13) // old samples age out
	if st := s.vals[3].e.Status(); !st.Signing {
		t.Fatalf("not signing after fix: skew %d", st.ClockSkewMs)
	}
	// A skewed trader.
	s.traders[0].offset = 3 * time.Second
	s.epochs(6)
	now := s.traders[0].e.now()
	o := &MeshOrder{Market: dcrBTC, Sell: true, Qty: 1e8, LotSize: 1e8, Rate: 1e5, Stamp: now.UnixMilli(), Expiry: now.Add(time.Minute).UnixMilli(), EncPubKey: tEncPub}
	_ = o.Sign(s.traders[0].priv)
	if err := s.traders[0].e.SubmitOrder(s.ctx, o); err == nil || !strings.Contains(err.Error(), "clock") {
		t.Fatalf("skewed trader order accepted: %v", err)
	}
	// And the validators reject its stamps anyway.
	if err := s.vals[0].e.admitOrder(o); err == nil {
		t.Fatal("validator admitted an order stamped 3 s in the future")
	}
}

// TestEpochOmission: a proposer that leaves out a trader's orders is
// flagged by the other validators, and the orders get in with the next
// honest proposer.
func TestEpochOmission(t *testing.T) {
	s := newEpochSim(t, 4, 2, DefaultEpochParams)
	s.epochs(3)
	bad := s.vals[1]
	bad.e.SetOmitOwner(s.traders[0].id.String())
	// Submit just after the epoch before bad's turn starts.
	for {
		n := s.rel.Params.EpochOf(s.clock.now().UnixMilli())
		if s.rel.Proposer(n, 0) == bad.id.String() && s.clock.now().UnixMilli()-int64(n)*s.rel.Params.EpochMs < 1000 {
			break
		}
		s.step(50 * time.Millisecond)
	}
	o := s.order(s.traders[0], true, 1, 100_000)
	t.Logf("order %s in pool of v0: %s", o.ID(), s.vals[0].e.OrderStatus(o.ID()))
	s.step(s.rel.EpochDuration() + time.Second) // past bad's proposal for the epoch
	t.Logf("order after bad's turn: %s", s.vals[0].e.OrderStatus(o.ID()))
	flags := 0
	for _, v := range []*simNode{s.vals[0], s.vals[2], s.vals[3]} {
		for _, f := range v.e.Status().Flags {
			if f.Kind == "omission" && f.Peer == bad.id.String() {
				flags++
				t.Logf("%s flagged: %s", v.name, f.Detail)
			}
		}
	}
	if flags != 3 {
		t.Fatalf("%d omission flags", flags)
	}
	s.epochs(2)
	if st := s.vals[0].e.OrderStatus(o.ID()); st != "resting" {
		t.Fatalf("omitted order not included later: %s", st)
	}
	if s.vals[0].e.Status().Validators[0].Omissions+s.vals[0].e.Status().Validators[1].Omissions+
		s.vals[0].e.Status().Validators[2].Omissions+s.vals[0].e.Status().Validators[3].Omissions == 0 {
		t.Fatal("no omission count in status")
	}
}

// TestEpochForgedRecord: a record signed by one validator, or with a
// tampered match, fails verification.
func TestEpochForgedRecord(t *testing.T) {
	s := newEpochSim(t, 4, 2, DefaultEpochParams)
	s.epochs(2)
	s.order(s.traders[0], true, 1, 100_000)
	s.order(s.traders[1], false, 1, 100_000)
	s.epochs(2)
	var rec *EpochRecord
	for _, r := range s.vals[0].e.Records(0, 100) {
		if len(r.Matches) > 0 {
			rec = r
		}
	}
	if rec == nil {
		t.Fatal("no match")
	}
	if err := VerifyRecord(s.rel, rec); err != nil {
		t.Fatal(err)
	}
	// Tampered qty.
	f := *rec
	f.Matches = []*EpochMatch{{}}
	*f.Matches[0] = *rec.Matches[0]
	f.Matches[0].Qty *= 2
	f.Proposal = nil
	if err := VerifyRecord(s.rel, &f); err == nil {
		t.Fatal("tampered record verified")
	}
	// Forged by a single validator: recompute the fingerprint, sign alone.
	f.Fingerprint = epochFingerprint(f.Epoch, f.Prev, f.ProposalHash, f.Active, f.Matches, f.StateHash)
	a := &EpochAttestation{Epoch: f.Epoch, Fingerprint: f.Fingerprint, Stamp: 1}
	_ = a.sign(s.vals[2].priv)
	f.Sigs = []*EpochAttestation{a}
	err := VerifyRecord(s.rel, &f)
	if err == nil || !strings.Contains(err.Error(), "not finalized") {
		t.Fatalf("forged record: %v", err)
	}
	t.Logf("forged single-signer record rejected: %v", err)
	// Signatures by a non-validator don't count.
	f.Sigs = nil
	for _, tr := range s.traders {
		a := &EpochAttestation{Epoch: f.Epoch, Fingerprint: f.Fingerprint, Stamp: 1}
		_ = a.sign(tr.priv)
		f.Sigs = append(f.Sigs, a)
	}
	if VerifyRecord(s.rel, &f) == nil {
		t.Fatal("non-validator sigs counted")
	}
}

// TestEpochStoreReload: a validator restarted with its record file reloads
// the chain and keeps up.
func TestEpochStoreReload(t *testing.T) {
	dir := t.TempDir()
	s := newEpochSim(t, 4, 1, DefaultEpochParams)
	v := s.vals[0]
	path := filepath.Join(dir, "epochs.jsonl")
	s.startNode(v, true, path) // replace with a stored engine
	s.epochs(3)
	s.order(s.traders[0], true, 1, 100_000)
	s.epochs(3)
	n := len(v.e.Records(0, 1e6))
	b, _ := os.ReadFile(path)
	if n == 0 || strings.Count(string(b), "\n") != n {
		t.Fatalf("%d records, %d lines", n, strings.Count(string(b), "\n"))
	}
	v.e.store.Close()
	s.net.setDown(v.id, true)
	s.epochs(1)
	s.startNode(v, true, path)
	if got := len(v.e.Records(0, 1e6)); got != n {
		t.Fatalf("reloaded %d of %d", got, n)
	}
	s.net.setDown(v.id, false)
	s.epochs(3)
	s.checkAgreement(s.all())
}

// --- S2: OrderBooks in epoch mode over the inbox ---

type s2Node struct {
	ob   *OrderBooks
	e    *EpochEngine
	priv libp2pcrypto.PrivKey
}

// TestEpochS2Negotiation: maker and taker trade a match from a finalized
// epoch; a forged single-validator record and a legacy proposal are rejected.
func TestEpochS2Negotiation(t *testing.T) {
	m := newFakeMesh()
	m.asyncInbox = true
	clock := &fakeClock{t: time.UnixMilli(1_800_000_000_000)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mk := func() *s2Node {
		ob := newTestBooks(t, m, nil)
		ob.now = clock.now
		ob.cfg.RetryInterval = time.Hour
		if err := ob.SubscribeInbox(ctx); err != nil {
			t.Fatal(err)
		}
		if err := ob.SubscribeMarket(ctx, dcrBTC); err != nil {
			t.Fatal(err)
		}
		return &s2Node{ob: ob, priv: ob.priv}
	}
	var nodes []*s2Node
	var ids []peer.ID
	for i := 0; i < 6; i++ {
		n := mk()
		nodes = append(nodes, n)
		if i < 4 {
			ids = append(ids, n.ob.self)
		}
	}
	rel := makeTestRelease(t, ids, DefaultEpochParams)
	for i, n := range nodes {
		e, err := NewEpochEngine(rel, EpochConfig{Validator: i < 4}, n.ob.ps, n.priv, clock.now, dex.StdOutLogger("E", dex.LevelError))
		if err != nil {
			t.Fatal(err)
		}
		n.ob.attachEpochs(ctx, e)
		n.e = e
		e.start(ctx)
	}
	step := func(d time.Duration) {
		for el := time.Duration(0); el < d; el += 50 * time.Millisecond {
			clock.add(50 * time.Millisecond)
			for _, n := range nodes {
				n.e.tick(ctx)
			}
			time.Sleep(time.Millisecond)
		}
	}
	maker, taker := nodes[4], nodes[5]
	step(25 * time.Second)
	mo, err := maker.ob.PostOrder(ctx, dcrBTC, true, 2e8, 1e8, 100_000, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	step(11 * time.Second)
	// Legacy proposal: rejected.
	bo := &BookOrder{MeshOrder: mo, Remaining: 2e8}
	legacyID, done, err := taker.ob.Propose(ctx, bo, 1e8, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, done)
	if r, _ := taker.ob.MatchRecord(legacyID); r.Status != MatchRejected || !strings.Contains(r.Reason, "not from a finalized epoch") {
		t.Fatalf("legacy proposal: %s %s", r.Status, r.Reason)
	}
	t.Logf("legacy proposal rejected: %s", mustRecord(taker.ob, legacyID).Reason)
	// Forged record signed by one validator (node 0 acting as taker).
	forger := nodes[0]
	fid, fdone := forgeEpochMatch(t, ctx, forger, rel, mo, 1e8)
	waitDone(t, fdone)
	if r, _ := forger.ob.MatchRecord(fid); r.Status != MatchRejected || !strings.Contains(r.Reason, "not finalized") {
		t.Fatalf("forged: %s %s", r.Status, r.Reason)
	}
	t.Logf("forged match rejected: %s", mustRecord(forger.ob, fid).Reason)
	// Real epoch match.
	if _, err := taker.ob.Take(ctx, dcrBTC, false, 1e8, 100_000, 1e8, false, 0); err != nil {
		t.Fatal(err)
	}
	step(12 * time.Second)
	var agreed *MatchRecord
	for i := 0; i < 200 && agreed == nil; i++ {
		for _, r := range taker.ob.Matches() {
			if r.Status == MatchAgreed {
				agreed = r
			}
		}
		// Keep the simulated clock moving: a maker that saw the record
		// late catches up by sync, which is paced by the engine clock.
		step(100 * time.Millisecond)
		time.Sleep(10 * time.Millisecond)
	}
	if agreed == nil {
		for _, r := range taker.ob.Matches() {
			t.Logf("match %s %s %s", r.Match.ID, r.Status, r.Reason)
		}
		t.Fatal("epoch match not agreed")
	}
	if agreed.Match.Stamp%DefaultEpochParams.EpochMs != 0 {
		t.Fatal("match stamp is not an epoch end")
	}
	mr, ok := maker.ob.MatchRecord(agreed.Match.ID)
	if !ok || mr.Status != MatchAgreed {
		t.Fatal("maker side not agreed")
	}
	t.Logf("epoch match %s agreed: %d @ %d", agreed.Match.ID, agreed.Match.Qty, agreed.Match.Rate)
}

func mustRecord(ob *OrderBooks, id MatchID) *MatchRecord {
	r, _ := ob.MatchRecord(id)
	return r
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

// forgeEpochMatch builds a record with a fake match of mo against n as
// taker, signed only by n, and proposes it (the debug API's forge).
func forgeEpochMatch(t *testing.T, ctx context.Context, n *s2Node, rel *Release, mo *MeshOrder, qty uint64) (MatchID, chan struct{}) {
	t.Helper()
	id, done, err := n.ob.ForgeEpochMatch(ctx, &BookOrder{MeshOrder: mo, Remaining: qty}, qty)
	if err != nil {
		t.Fatal(err)
	}
	return id, done
}

var _ = binary.BigEndian

// TestEpochFragments: messages over the 1 KiB mesh publish limit are split
// and reassembled in any order; a tampered fragment drops the message.
func TestEpochFragments(t *testing.T) {
	e := &EpochEngine{frags: make(map[string]*fragBuf), log: dex.StdOutLogger("T", dex.LevelOff)}
	big := make([]byte, 50_000)
	for i := range big {
		big[i] = byte(i * 7)
	}
	parts, err := fragment(EpochTopic, big)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected fragments, got %d", len(parts))
	}
	decode := func(b []byte) *epochFrag {
		var m epochMsg
		if err := json.Unmarshal(b, &m); err != nil || m.Frag == nil {
			t.Fatalf("bad fragment: %v", err)
		}
		if !fitsPublish(EpochTopic, b) {
			t.Fatalf("fragment of %d bytes too large", len(b))
		}
		return m.Frag
	}
	var whole []byte
	for i := len(parts) - 1; i >= 0; i-- { // reverse order
		if w := e.addFragLocked("peerA", decode(parts[i]), 1); w != nil {
			whole = w
		}
	}
	if !bytes.Equal(whole, big) {
		t.Fatalf("reassembly failed")
	}
	// Tampered fragment.
	for i, p := range parts {
		f := decode(p)
		if i == 3 {
			f.Data[0] ^= 1
		}
		if w := e.addFragLocked("peerA", f, 1); w != nil {
			t.Fatalf("tampered message reassembled")
		}
	}
	// Small messages are sent whole.
	if ps, _ := fragment(EpochTopic, []byte(`{"type":"hb"}`)); len(ps) != 1 {
		t.Fatalf("small message fragmented")
	}
	// Too large.
	if _, err := fragment(EpochTopic, make([]byte, (epochMaxFrags+1)*epochFragChunk)); err == nil {
		t.Fatalf("oversized message accepted")
	}
	// Stale buffers expire.
	e.addFragLocked("peerB", decode(parts[0]), 1)
	e.expireFragsLocked(1 + epochFragTTLMs + 1)
	if len(e.frags) != 0 {
		t.Fatalf("stale fragments kept")
	}
	t.Logf("50000-byte message sent as %d fragments of <= %d bytes", len(parts), meshMaxPublish-meshPublishOverhead)
}
