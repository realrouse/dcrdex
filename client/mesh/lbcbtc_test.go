// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"strings"
	"testing"
	"time"

	"decred.org/dcrdex/dex/calc"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
)

var lbcBTC = Market{Base: 140, Quote: 0}

const lbcLot = 100_000e8

// withRateStep sets a market's rate step for one test.
func withRateStep(t *testing.T, name string, step uint64) {
	t.Helper()
	for i := range DefaultMarkets {
		if DefaultMarkets[i].Name == name {
			old := DefaultMarkets[i].RateStep
			DefaultMarkets[i].RateStep = step
			t.Cleanup(func() { DefaultMarkets[i].RateStep = old })
			return
		}
	}
	t.Fatalf("no market %s", name)
}

func lbcOrder(t *testing.T, priv libp2pcrypto.PrivKey, sell bool, lots, rate uint64, stamp int64) *MeshOrder {
	t.Helper()
	o := &MeshOrder{Market: lbcBTC, Sell: sell, Qty: lots * lbcLot, LotSize: lbcLot, Rate: rate, Stamp: stamp,
		Expiry: stamp + 60_000, Nonce: uint64(stamp), EncPubKey: tEncPub}
	if err := o.Sign(priv); err != nil {
		t.Fatal(err)
	}
	return o
}

// lbc_btc prices are whole sats per LBC: 1 sat is the minimum and the step.
func TestLBCBTCRateStep(t *testing.T) {
	d, ok := MarketDefault("lbc_btc")
	if !ok || d.RateStep != 1 || d.TestRate != 1 || MarketRateStep("lbc_btc") != 1 {
		t.Fatalf("lbc_btc defaults %+v", d)
	}
	if MarketRateStep("dcr_btc") != 1 || MarketRateStep("no_such") != 1 {
		t.Fatal("default rate step is not 1")
	}
	for _, r := range []uint64{1, 2, 3, 1000} {
		if err := CheckRate("lbc_btc", r); err != nil {
			t.Fatalf("rate %d: %v", r, err)
		}
	}
	err := CheckRate("lbc_btc", 0)
	if err == nil || !strings.Contains(err.Error(), "1 sat per LBC") {
		t.Fatalf("rate 0: %v", err)
	}
	// A coarser step: below the step and non-multiples are refused.
	withRateStep(t, "lbc_btc", 5)
	for r, want := range map[uint64]bool{1: false, 4: false, 5: true, 7: false, 10: true} {
		if err := CheckRate("lbc_btc", r); (err == nil) != want {
			t.Fatalf("step 5 rate %d: %v", r, err)
		}
	}
	if err := CheckRate("lbc_btc", 7); !strings.Contains(err.Error(), "5 sat per LBC") {
		t.Fatalf("message: %v", err)
	}
}

func TestLBCBTCOrderValidate(t *testing.T) {
	priv := newPriv(t)
	stamp := tNow.UnixMilli()
	o := lbcOrder(t, priv, true, 1, 1, stamp)
	if err := o.Validate(tNow); err != nil {
		t.Fatalf("1 sat lbc_btc order: %v", err)
	}
	if v := calc.BaseToQuote(1, lbcLot); v != 100_000 {
		t.Fatalf("1 lot at 1 sat = %d sats", v)
	}
	withRateStep(t, "lbc_btc", 2)
	if err := lbcOrder(t, priv, true, 1, 1, stamp).Validate(tNow); err == nil || !strings.Contains(err.Error(), "minimum price step") {
		t.Fatalf("rate below the step: %v", err)
	}
	if err := lbcOrder(t, priv, true, 1, 3, stamp).Validate(tNow); err == nil {
		t.Fatal("non-multiple of the step accepted")
	}
	if err := lbcOrder(t, priv, true, 1, 4, stamp).Validate(tNow); err != nil {
		t.Fatal(err)
	}
}

func TestLBCBTCPostTakeRejectsBadRate(t *testing.T) {
	priv := newPriv(t)
	ob := NewOrderBooks(nil, priv, nil, tLogger)
	withRateStep(t, "lbc_btc", 2)
	if _, err := ob.PostOrder(tCtx, lbcBTC, true, lbcLot, lbcLot, 3, time.Minute); err == nil || !strings.Contains(err.Error(), "2 sat per LBC") {
		t.Fatalf("post: %v", err)
	}
	if _, err := ob.Take(tCtx, lbcBTC, false, lbcLot, 1, lbcLot, false, time.Minute); err == nil || !strings.Contains(err.Error(), "minimum price step") {
		t.Fatalf("take: %v", err)
	}
}

// At 1 sat, a sell and a buy from different owners match at 1 sat; the same
// owner's buy and sell at 1 sat never match each other.
func TestLBCBTCOneSatEpochMatch(t *testing.T) {
	mm, taker := newPriv(t), newPriv(t)
	p := DefaultEpochParams
	n := uint64(180_000_000)
	end := p.EpochEnd(n)
	s := newEpochState()
	ask := lbcOrder(t, mm, true, 2, 1, end-9000)
	selfBid := lbcOrder(t, mm, false, 1, 1, end-8000)
	if m := applyEpoch(s, &EpochProposal{Epoch: n, Orders: []*MeshOrder{ask, selfBid}}, &p); len(m) != 0 {
		t.Fatalf("self match at 1 sat: %+v", m)
	}
	bid := lbcOrder(t, taker, false, 1, 1, end+p.EpochMs-5000)
	m := applyEpoch(s, &EpochProposal{Epoch: n + 1, Orders: []*MeshOrder{bid}}, &p)
	if len(m) != 1 || m[0].Rate != 1 || m[0].Qty != lbcLot || m[0].MakerOrderID != ask.ID() {
		t.Fatalf("matches %+v", m)
	}
}

// Price history and candles work on whole-sat rates.
func TestLBCBTCWholeSatCandles(t *testing.T) {
	base := tNow.Truncate(time.Minute).UnixMilli()
	ts := []*VerifiedTrade{
		{Market: lbcBTC, Rate: 1, Qty: lbcLot, Stamp: base + 1000},
		{Market: lbcBTC, Rate: 2, Qty: lbcLot, Stamp: base + 2000},
		{Market: lbcBTC, Rate: 1, Qty: 2 * lbcLot, Stamp: base + 3000},
		{Market: lbcBTC, Rate: 1, Qty: lbcLot, Stamp: base + 61_000},
		{Market: lbcBTC, Rate: 2, Qty: lbcLot, Stamp: base + 62_000},
	}
	cs := BuildVerifiedCandles(ts, time.Minute)
	if len(cs) != 2 {
		t.Fatalf("%d candles", len(cs))
	}
	c := cs[0]
	if c.LowRate != 1 || c.HighRate != 2 || c.StartRate != 1 || c.EndRate != 1 || c.MedianRate != 1 || c.Trades != 3 ||
		c.MatchVolume != 4*lbcLot || c.QuoteVolume != 500_000 {
		t.Fatalf("candle %+v", c)
	}
	// Even count: the median of 1 and 2 sat stays a whole rate (1).
	if cs[1].MedianRate != 1 {
		t.Fatalf("median %d", cs[1].MedianRate)
	}
	spot := MarketSpot(lbcBTC, TradeSummaries(ts), time.UnixMilli(base+70_000))
	if spot == nil || spot.Rate != 2 || spot.Low24 != 1 || spot.High24 != 2 {
		t.Fatalf("spot %+v", spot)
	}
	if bc := BuildCandles(TradeSummaries(ts), time.Minute); bc == nil {
		t.Fatal("no candle cache")
	}
}
