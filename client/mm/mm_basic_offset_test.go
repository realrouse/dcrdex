// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"decred.org/dcrdex/client/core"
)

func TestOracleOffset(t *testing.T) {
	const (
		baseID  = uint32(42)
		quoteID = uint32(0)
		lotSize = uint64(1e8)
	)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newMM := func(offset *float64, rateStep uint64) (*basicMarketMaker, *tBasicMMCalculator, *time.Time) {
		now := start
		mm := &basicMarketMaker{
			unifiedExchangeAdaptor: mustParseAdaptorFromMarket(&core.Market{
				RateStep:   rateStep,
				AtomToConv: 1,
				LotSize:    lotSize,
				BaseID:     baseID,
				QuoteID:    quoteID,
			}),
			nowFn: func() time.Time { return now },
		}
		c := &tBasicMMCalculator{bp: 100_000}
		mm.calculator = c
		mm.botCfgV.Store(&BotConfig{
			BasicMMConfig: &BasicMarketMakingConfig{
				GapStrategy:           GapStrategyAbsolute,
				SellPlacements:        []*OrderPlacement{{Lots: 1, GapFactor: 1e-5}},
				BuyPlacements:         []*OrderPlacement{{Lots: 1, GapFactor: 1e-5}},
				InventorySkew:         floatPtr(0),
				DoNotCross:            boolPtr(false),
				OracleHalfLifeMinutes: floatPtr(0),
				OracleOffset:          offset,
			},
		})
		return mm, c, &now
	}

	ladder := func(t *testing.T, mm *basicMarketMaker) (bid, ask uint64) {
		t.Helper()
		buys, sells, err := mm.ordersToPlace()
		if err != nil {
			t.Fatal(err)
		}
		return buys[0].Rate, sells[0].Rate
	}

	// GapFactor 1e-5 in conventional units is 1000 msg-rate atoms here.
	for _, tt := range []struct {
		name     string
		offset   *float64
		bid, ask uint64
	}{
		{"absent", nil, 99_000, 101_000},
		{"zero", floatPtr(0), 99_000, 101_000},
		{"premium", floatPtr(0.02), 101_000, 103_000},
		{"discount", floatPtr(-0.05), 94_000, 96_000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mm, _, _ := newMM(tt.offset, 1)
			bid, ask := ladder(t, mm)
			if bid != tt.bid || ask != tt.ask {
				t.Fatalf("ladder %d / %d, want %d / %d", bid, ask, tt.bid, tt.ask)
			}
		})
	}

	for _, tt := range []struct {
		name             string
		both, bidO, askO *float64
		bid, ask         uint64
	}{
		{"bid only", nil, floatPtr(-0.01), nil, 98_000, 101_000},
		{"ask only", nil, nil, floatPtr(0.03), 99_000, 104_000},
		{"both sides", nil, floatPtr(-0.01), floatPtr(0.03), 98_000, 104_000},
		{"fallback for missing side", floatPtr(0.02), floatPtr(-0.01), nil, 98_000, 103_000},
		{"per-side overrides fallback", floatPtr(0.02), floatPtr(0), floatPtr(0), 99_000, 101_000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mm, _, _ := newMM(tt.both, 1)
			mm.cfg().OracleBidOffset = tt.bidO
			mm.cfg().OracleAskOffset = tt.askO
			bid, ask := ladder(t, mm)
			if bid != tt.bid || ask != tt.ask {
				t.Fatalf("ladder %d / %d, want %d / %d", bid, ask, tt.bid, tt.ask)
			}
		})
	}

	t.Run("rate step", func(t *testing.T) {
		mm, _, _ := newMM(floatPtr(0.0123), 100)
		if got := mm.offsetRate(100_000, 0.0123); got != 101_200 {
			t.Fatalf("got %d, want 101200 (stepped)", got)
		}
	})

	t.Run("offset applies after damping", func(t *testing.T) {
		mm, c, now := newMM(floatPtr(0.1), 1)
		mm.cfg().OracleHalfLifeMinutes = floatPtr(10)
		ladder(t, mm) // center 110000
		c.bp = 200_000
		*now = start.Add(10 * time.Minute)
		bid, ask := ladder(t, mm)
		// Damped 150000, +10% = 165000 for bids. Asks are held at the
		// raw oracle +10% (220000) by the ask floor.
		if bid != 164_000 || ask != 221_000 {
			t.Fatalf("ladder %d / %d, want 164000 / 221000", bid, ask)
		}
	})

	t.Run("config", func(t *testing.T) {
		var cfg BasicMarketMakingConfig
		if err := json.Unmarshal([]byte(`{"gapStrategy":"absolute-plus","oracleOffset":-0.03}`), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		if *cfg.OracleOffset != -0.03 {
			t.Fatalf("offset %v", *cfg.OracleOffset)
		}
		cp := cfg.copy()
		if cp.OracleOffset == cfg.OracleOffset || *cp.OracleOffset != -0.03 {
			t.Fatal("copy should clone OracleOffset")
		}
		var absent BasicMarketMakingConfig
		if err := json.Unmarshal([]byte(`{"gapStrategy":"absolute-plus"}`), &absent); err != nil {
			t.Fatal(err)
		}
		if err := absent.validate(); err != nil || absent.OracleOffset != nil {
			t.Fatalf("absent offset: err %v, value %v", err, absent.OracleOffset)
		}
		for _, v := range []float64{-0.51, 0.51, math.NaN()} {
			for _, bad := range []*BasicMarketMakingConfig{
				{GapStrategy: GapStrategyPercent, OracleOffset: floatPtr(v)},
				{GapStrategy: GapStrategyPercent, OracleBidOffset: floatPtr(v)},
				{GapStrategy: GapStrategyPercent, OracleAskOffset: floatPtr(v)},
			} {
				if err := bad.validate(); err == nil {
					t.Fatalf("expected error for offset %v in %+v", v, bad)
				}
			}
		}
		for _, tt := range []struct {
			name             string
			both, bidO, askO *float64
			ok               bool
		}{
			{"bid above ask", nil, floatPtr(0.05), floatPtr(-0.05), false},
			{"bid above fallback ask", floatPtr(0), floatPtr(0.01), nil, false},
			{"fallback bid above ask", floatPtr(0.02), nil, floatPtr(0.01), false},
			{"equal", nil, floatPtr(0.02), floatPtr(0.02), true},
			{"shared only", floatPtr(-0.03), nil, nil, true},
			{"bid below ask", floatPtr(0.02), floatPtr(-0.01), nil, true},
		} {
			c := &BasicMarketMakingConfig{GapStrategy: GapStrategyPercent,
				OracleOffset: tt.both, OracleBidOffset: tt.bidO, OracleAskOffset: tt.askO}
			if err := c.validate(); (err == nil) != tt.ok {
				t.Fatalf("%s: validate error = %v, want ok = %v", tt.name, err, tt.ok)
			}
		}
		var sides BasicMarketMakingConfig
		if err := json.Unmarshal([]byte(`{"gapStrategy":"absolute-plus","oracleBidOffset":-0.01,"oracleAskOffset":0.03}`), &sides); err != nil {
			t.Fatal(err)
		}
		if err := sides.validate(); err != nil {
			t.Fatal(err)
		}
		cs := sides.copy()
		if cs.OracleBidOffset == sides.OracleBidOffset || cs.OracleAskOffset == sides.OracleAskOffset ||
			*cs.OracleBidOffset != -0.01 || *cs.OracleAskOffset != 0.03 {
			t.Fatal("copy should clone per-side offsets")
		}
	})
}
