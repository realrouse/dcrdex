// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// E is a realistic epoch (2 s epochs, 2026) so lock times are 4-byte script numbers.
const E = 890_000_000

type s7Party struct {
	priv libp2pcrypto.PrivKey
	id   string
}

func newS7Party(t *testing.T) *s7Party {
	priv, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := peer.IDFromPrivateKey(priv)
	return &s7Party{priv: priv, id: pid.String()}
}

func testS7Release(t *testing.T) *Release {
	v := newS7Party(t)
	pid, _ := peer.Decode(v.id)
	r := makeUserBondRelease(t, []peer.ID{pid}, s6Params(), nil)
	r.PriceHistory = &PriceRules{MinConfs: 1, MaxProofsPerEpoch: 4, MaxAgeSecs: 600, MinLockLeftSecs: 60}
	return r
}

// s7Trade makes an epoch match and a valid, fully signed proof for it.
func s7Trade(t *testing.T, rel *Release, mk, tk *s7Party, epoch uint64, idx int, mkt string, rate, qty uint64) (*EpochMatch, *TradeProof) {
	m, _ := ParseMarket(mkt)
	em := &EpochMatch{Market: m, Maker: mk.id, Taker: tk.id, MakerSell: true, Qty: qty, Rate: rate}
	end := rel.Params.EpochEnd(epoch)
	sm := &Match{Market: m, MakerSell: true, Stamp: end, MakerLockTime: end/1000 + 7200, TakerLockTime: end/1000 + 3600}
	sm.ID[0], sm.ID[1] = byte(epoch), byte(idx)
	p := &TradeProof{Epoch: epoch, Index: idx, Qty: qty}
	var err error
	if p.MakerCoin, p.MakerContract, err = fakeTradeContract(sm, true); err != nil {
		t.Fatal(err)
	}
	if p.TakerCoin, p.TakerContract, err = fakeTradeContract(sm, false); err != nil {
		t.Fatal(err)
	}
	if p.MakerSig, err = mk.priv.Sign(p.msg()); err != nil {
		t.Fatal(err)
	}
	if p.TakerSig, err = tk.priv.Sign(p.msg()); err != nil {
		t.Fatal(err)
	}
	return em, p
}

func TestTradeProofChecks(t *testing.T) {
	rel := testS7Release(t)
	mk, tk, eve := newS7Party(t), newS7Party(t), newS7Party(t)
	em, p := s7Trade(t, rel, mk, tk, E, 0, "dcr_btc", 16_000, 2e8)
	end := rel.Params.EpochEnd(E)
	if _, err := checkTradeProof(p, em, end, true); err != nil {
		t.Fatalf("valid proof: %v", err)
	}
	half := p.copyUnsigned()
	half.MakerSig = p.MakerSig
	if _, err := checkTradeProof(half, em, end, false); err != nil {
		t.Fatalf("maker half: %v", err)
	}
	if _, err := checkTradeProof(half, em, end, true); err == nil {
		t.Fatal("half proof passed the full check")
	}
	bad := func(name string, p *TradeProof, em *EpochMatch, want string) {
		t.Helper()
		_, err := checkTradeProof(p, em, end, true)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: want %q, got %v", name, want, err)
		}
	}
	bad("no match", p, nil, "no such finalized epoch match")
	forged := *p
	forged.MakerSig, _ = eve.priv.Sign(p.msg())
	bad("eve signs as maker", &forged, em, "maker signature")
	self := *em
	self.Taker = self.Maker
	bad("self trade", p, &self, "self trade")
	more := *p
	more.Qty = em.Qty + 1e8
	bad("qty above match", &more, em, "not within")
	small := *em
	small.Qty = 5e7
	dust := *p
	dust.Qty = 5e7
	dust.MakerSig, _ = mk.priv.Sign(dust.msg())
	dust.TakerSig, _ = tk.priv.Sign(dust.msg())
	bad("dust", &dust, &small, "dust")
	swapped := *p // contracts on the wrong assets/sides: taker lock > maker lock
	swapped.MakerCoin, swapped.TakerCoin = p.TakerCoin, p.MakerCoin
	swapped.MakerContract, swapped.TakerContract = p.TakerContract, p.MakerContract
	bad("contracts on the wrong chains", &swapped, em, "bad swap contract")
	sm := &Match{Market: em.Market, MakerSell: true, Stamp: end, MakerLockTime: end/1000 + 600, TakerLockTime: end/1000 + 3600}
	lk := p.copyUnsigned()
	lk.MakerCoin, lk.MakerContract, _ = fakeTradeContract(sm, true)
	lk.TakerCoin, lk.TakerContract, _ = fakeTradeContract(sm, false)
	lk.MakerSig, _ = mk.priv.Sign(lk.msg())
	lk.TakerSig, _ = tk.priv.Sign(lk.msg())
	bad("maker lock before taker lock", lk, em, "lock times")
	sm.MakerLockTime, sm.TakerLockTime = end/1000+7200, end/1000-10
	lk.MakerCoin, lk.MakerContract, _ = fakeTradeContract(sm, true)
	lk.TakerCoin, lk.TakerContract, _ = fakeTradeContract(sm, false)
	lk.MakerSig, _ = mk.priv.Sign(lk.msg())
	lk.TakerSig, _ = tk.priv.Sign(lk.msg())
	bad("contract older than the match", lk, em, "lock times")
	usdc := *em
	usdc.Market, _ = ParseMarket("dcr_usdc.polygon")
	bad("unverifiable asset", p, &usdc, "not verified on chain")
	tampered := *p
	tampered.Qty = 1e8 // signatures over 2e8
	bad("tampered qty", &tampered, em, "signature")
}

type fakeTradeChain struct {
	confs   map[string]uint32
	spent   map[string]bool
	expired bool
	err     error
}

func (c *fakeTradeChain) SwapConfs(_ context.Context, _ uint32, coin, _ []byte, _ time.Time) (uint32, bool, error) {
	if c.err != nil {
		return 0, false, c.err
	}
	return c.confs[string(coin)], c.spent[string(coin)], nil
}

func (c *fakeTradeChain) SwapLockExpired(context.Context, uint32, int64) (bool, error) {
	return c.expired, nil
}

func TestCheckTradeOnChain(t *testing.T) {
	rel := testS7Release(t)
	mk, tk := newS7Party(t), newS7Party(t)
	em, p := s7Trade(t, rel, mk, tk, E, 0, "lbc_dcr", 3_900, 1_000e8)
	d, err := checkTradeProof(p, em, rel.Params.EpochEnd(E), true)
	if err != nil {
		t.Fatal(err)
	}
	r := rel.PriceHistory
	ch := &fakeTradeChain{confs: map[string]uint32{}, spent: map[string]bool{}}
	if why, _ := checkTradeOnChain(context.Background(), ch, r, p, d); !strings.Contains(why, "confirmations") {
		t.Fatalf("not on chain: %q", why)
	}
	ch.confs[string(p.MakerCoin)], ch.confs[string(p.TakerCoin)] = 3, 3
	if why, _ := checkTradeOnChain(context.Background(), ch, r, p, d); !strings.Contains(why, "not spent") {
		t.Fatalf("unspent: %q", why)
	}
	ch.spent[string(p.MakerCoin)] = true
	if why, _ := checkTradeOnChain(context.Background(), ch, r, p, d); !strings.Contains(why, "taker contract") {
		t.Fatalf("taker unspent: %q", why)
	}
	ch.spent[string(p.TakerCoin)] = true
	if why, dead := checkTradeOnChain(context.Background(), ch, r, p, d); why != "" || dead {
		t.Fatalf("redeemed: %q %v", why, dead)
	}
	ch.expired = true
	if why, dead := checkTradeOnChain(context.Background(), ch, r, p, d); !dead || !strings.Contains(why, "refund") {
		t.Fatalf("refund: %q %v", why, dead)
	}
	ch.err = errors.New("output not found")
	if why, dead := checkTradeOnChain(context.Background(), ch, r, p, d); dead || !strings.Contains(why, "not found") {
		t.Fatalf("lookup error: %q %v", why, dead)
	}
}

func TestLedgerVerifiedHistory(t *testing.T) {
	rel := testS7Release(t)
	mk, tk := newS7Party(t), newS7Party(t)
	build := func(order []int) (*bondLedger, *PriceHistoryStatus) {
		l := newBondLedger(rel)
		var ems []*EpochMatch
		var ps []*TradeProof
		for i, rate := range []uint64{16_000, 16_400, 15_800} {
			em, p := s7Trade(t, rel, mk, tk, E, i, "dcr_btc", rate, 1e8)
			ems = append(ems, em)
			ps = append(ps, p)
		}
		// A forged proof: no such match.
		_, ghost := s7Trade(t, rel, mk, tk, E, 7, "dcr_btc", 99_999, 1e8)
		l.apply(&EpochRecord{Epoch: E, Matches: ems, Proposal: &EpochProposal{Epoch: E}})
		var adm []*TradeProof
		for _, i := range order {
			adm = append(adm, ps[i])
		}
		adm = append(adm, ghost)
		l.apply(&EpochRecord{Epoch: E + 3, Proposal: &EpochProposal{Epoch: E + 3, Trades: adm}})
		// Same proof again later: counted once.
		l.apply(&EpochRecord{Epoch: E + 4, Proposal: &EpochProposal{Epoch: E + 4, Trades: ps[:1]}})
		e := &EpochEngine{rel: rel, ledger: l, tradePool: map[string]*tradeEntry{}, tradeRefused: map[string]string{}, ownTrades: map[string]*TradeProof{}}
		return l, e.PriceHistoryStatus(time.Minute)
	}
	l1, s1 := build([]int{0, 1, 2})
	_, s2 := build([]int{2, 0, 1})
	if s1.Hash != s2.Hash {
		t.Fatal("verified history depends on admission order")
	}
	m, _ := ParseMarket("dcr_btc")
	ts := l1.verifiedTrades(m)
	if len(ts) != 3 || l1.vcount != 3 {
		t.Fatalf("%d verified trades (count %d), want 3", len(ts), l1.vcount)
	}
	for _, vt := range ts {
		if vt.Rate == 99_999 {
			t.Fatal("ghost trade counted")
		}
		if vt.Stamp != rel.Params.EpochEnd(E) || vt.Admitted != E+3 {
			t.Fatalf("stamp %d admitted %d", vt.Stamp, vt.Admitted)
		}
	}
	cs := s1.Markets["dcr_btc"].Candles
	if len(cs) != 1 || cs[0].Trades != 3 || cs[0].MedianRate != 16_000 || cs[0].HighRate != 16_400 || cs[0].LowRate != 15_800 || cs[0].MatchVolume != 3e8 {
		t.Fatalf("candle %+v", cs[0])
	}
	// The proposal hash commits to the trades.
	p1 := &EpochProposal{Epoch: 5}
	h0 := p1.Hash()
	_, tp := s7Trade(t, rel, mk, tk, E, 0, "dcr_btc", 16_000, 1e8)
	p1.Trades = []*TradeProof{tp}
	if p1.Hash() == h0 {
		t.Fatal("proposal hash ignores trades")
	}
}
