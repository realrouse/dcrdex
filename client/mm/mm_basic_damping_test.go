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

func TestOracleDamping(t *testing.T) {
	const (
		baseID  = uint32(42)
		quoteID = uint32(0)
		lotSize = uint64(1e8)
	)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newMM := func(halfLife, maxStep *float64, now *time.Time) *basicMarketMaker {
		mm := &basicMarketMaker{
			unifiedExchangeAdaptor: mustParseAdaptorFromMarket(&core.Market{
				RateStep:   1,
				AtomToConv: 1,
				LotSize:    lotSize,
				BaseID:     baseID,
				QuoteID:    quoteID,
			}),
			nowFn: func() time.Time { return *now },
		}
		mm.botCfgV.Store(&BotConfig{
			BasicMMConfig: &BasicMarketMakingConfig{
				GapStrategy:           GapStrategyPercent,
				SellPlacements:        []*OrderPlacement{{Lots: 1, GapFactor: 0.01}},
				BuyPlacements:         []*OrderPlacement{{Lots: 1, GapFactor: 0.01}},
				InventorySkew:         floatPtr(0),
				DoNotCross:            boolPtr(false),
				OracleHalfLifeMinutes: halfLife,
				OracleMaxStep:         maxStep,
			},
		})
		return mm
	}

	t.Run("first price taken as is", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(10), nil, &now)
		if got := mm.dampedBasis(100_000, now); got != 100_000 {
			t.Fatalf("got %d", got)
		}
	})

	t.Run("half-life", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(10), nil, &now)
		mm.dampedBasis(100_000, now)
		// Same instant: no movement.
		if got := mm.dampedBasis(200_000, now); got != 100_000 {
			t.Fatalf("dt=0 moved to %d", got)
		}
		// One half-life closes half the gap.
		now = start.Add(10 * time.Minute)
		if got := mm.dampedBasis(200_000, now); got != 150_000 {
			t.Fatalf("one half-life: got %d, want 150000", got)
		}
		// Another closes half of what is left.
		now = start.Add(20 * time.Minute)
		if got := mm.dampedBasis(200_000, now); got != 175_000 {
			t.Fatalf("two half-lives: got %d, want 175000", got)
		}
	})

	t.Run("short spike barely moves the center", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(10), nil, &now)
		mm.dampedBasis(100_000, now)
		now = now.Add(15 * time.Second)
		spike := mm.dampedBasis(150_000, now)
		now = now.Add(15 * time.Second)
		back := mm.dampedBasis(100_000, now)
		if spike > 101_000 {
			t.Fatalf("15 s spike moved center to %d", spike)
		}
		if back > spike || back < 100_000 {
			t.Fatalf("center did not return: %d", back)
		}
	})

	t.Run("default when absent", func(t *testing.T) {
		now := start
		mm := newMM(nil, nil, &now)
		mm.dampedBasis(100_000, now)
		now = start.Add(time.Duration(defaultOracleHalfLifeMinutes * float64(time.Minute)))
		if got := mm.dampedBasis(200_000, now); got != 150_000 {
			t.Fatalf("absent half-life should use default, got %d", got)
		}
	})

	t.Run("explicit zero is off", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(0), nil, &now)
		mm.dampedBasis(100_000, now)
		if got := mm.dampedBasis(200_000, now); got != 200_000 {
			t.Fatalf("damping off should follow at once, got %d", got)
		}
		// Explicit zero for max step too.
		mm = newMM(floatPtr(0), floatPtr(0), &now)
		mm.dampedBasis(100_000, now)
		if got := mm.dampedBasis(50_000, now); got != 50_000 {
			t.Fatalf("got %d", got)
		}
	})

	t.Run("max step", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(0), floatPtr(0.01), &now)
		mm.dampedBasis(100_000, now)
		if got := mm.dampedBasis(200_000, now); got != 101_000 {
			t.Fatalf("up step: got %d, want 101000", got)
		}
		if got := mm.dampedBasis(200_000, now); got != 102_010 {
			t.Fatalf("second up step: got %d, want 102010", got)
		}
		mm = newMM(floatPtr(0), floatPtr(0.01), &now)
		mm.dampedBasis(100_000, now)
		if got := mm.dampedBasis(10_000, now); got != 99_000 {
			t.Fatalf("down step: got %d, want 99000", got)
		}
	})

	t.Run("half-life and max step", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(10), floatPtr(0.1), &now)
		mm.dampedBasis(100_000, now)
		now = start.Add(10 * time.Minute)
		// The EMA wants 150000, the cap allows 110000.
		if got := mm.dampedBasis(200_000, now); got != 110_000 {
			t.Fatalf("got %d, want 110000", got)
		}
	})

	t.Run("orders follow the damped center", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(10), nil, &now)
		calc := &tBasicMMCalculator{bp: 100_000}
		mm.calculator = calc
		buys, sells, err := mm.ordersToPlace()
		if err != nil {
			t.Fatal(err)
		}
		if buys[0].Rate != 99_000 || sells[0].Rate != 101_000 {
			t.Fatalf("initial ladder %d / %d", buys[0].Rate, sells[0].Rate)
		}
		calc.bp = 200_000
		now = start.Add(10 * time.Minute)
		buys, sells, err = mm.ordersToPlace()
		if err != nil {
			t.Fatal(err)
		}
		if buys[0].Rate != 148_500 || sells[0].Rate != 151_500 {
			t.Fatalf("damped ladder %d / %d, want 148500 / 151500", buys[0].Rate, sells[0].Rate)
		}
	})

	t.Run("config", func(t *testing.T) {
		var cfg BasicMarketMakingConfig
		if err := json.Unmarshal([]byte(`{"gapStrategy":"absolute-plus"}`), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.OracleHalfLifeMinutes == nil || *cfg.OracleHalfLifeMinutes != defaultOracleHalfLifeMinutes {
			t.Fatalf("default half-life not filled in: %v", cfg.OracleHalfLifeMinutes)
		}
		if cfg.OracleMaxStep != nil {
			t.Fatal("absent max step should stay nil")
		}
		for _, bad := range []*BasicMarketMakingConfig{
			{GapStrategy: GapStrategyPercent, OracleHalfLifeMinutes: floatPtr(-1)},
			{GapStrategy: GapStrategyPercent, OracleHalfLifeMinutes: floatPtr(maxOracleHalfLifeMinutes + 1)},
			{GapStrategy: GapStrategyPercent, OracleHalfLifeMinutes: floatPtr(math.NaN())},
			{GapStrategy: GapStrategyPercent, OracleMaxStep: floatPtr(-0.1)},
			{GapStrategy: GapStrategyPercent, OracleMaxStep: floatPtr(0.6)},
		} {
			if err := bad.validate(); err == nil {
				t.Fatalf("expected error for %+v", bad)
			}
		}
		ok := &BasicMarketMakingConfig{GapStrategy: GapStrategyPercent, OracleHalfLifeMinutes: floatPtr(0), OracleMaxStep: floatPtr(0.005)}
		if err := ok.validate(); err != nil {
			t.Fatal(err)
		}
		if *ok.OracleHalfLifeMinutes != 0 {
			t.Fatal("explicit 0 half-life should stay 0")
		}
		cp := ok.copy()
		if cp.OracleHalfLifeMinutes == ok.OracleHalfLifeMinutes || cp.OracleMaxStep == ok.OracleMaxStep {
			t.Fatal("copy should clone damping pointers")
		}
		if *cp.OracleMaxStep != 0.005 {
			t.Fatal("copy lost max step")
		}
	})
}
