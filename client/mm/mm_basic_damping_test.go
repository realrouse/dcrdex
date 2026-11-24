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

	t.Run("drops followed at once by default", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(10), nil, &now)
		mm.dampedBasis(100_000, now)
		now = now.Add(time.Second)
		if got := mm.dampedBasis(60_000, now); got != 60_000 {
			t.Fatalf("drop: got %d, want 60000", got)
		}
		// A rise from the new low is damped again.
		now = now.Add(10 * time.Minute)
		if got := mm.dampedBasis(100_000, now); got != 80_000 {
			t.Fatalf("rise after drop: got %d, want 80000", got)
		}
		// A drop below the damped center while rising snaps down.
		now = now.Add(time.Second)
		if got := mm.dampedBasis(70_000, now); got != 70_000 {
			t.Fatalf("drop below center: got %d, want 70000", got)
		}
		// A dip that stays above the damped center keeps the center.
		mm = newMM(floatPtr(10), nil, &now)
		mm.dampedBasis(100_000, now)
		now = now.Add(10 * time.Minute)
		mm.dampedBasis(200_000, now) // center 150000
		if got := mm.dampedBasis(180_000, now); got != 150_000 {
			t.Fatalf("dip above center: got %d, want 150000", got)
		}
	})

	t.Run("damp drops option is symmetric", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(10), nil, &now)
		mm.cfg().OracleDampDrops = boolPtr(true)
		mm.dampedBasis(100_000, now)
		now = now.Add(10 * time.Minute)
		if got := mm.dampedBasis(50_000, now); got != 75_000 {
			t.Fatalf("damped drop: got %d, want 75000", got)
		}
	})

	t.Run("bids never above raw oracle plus bid offset", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(10), nil, &now)
		mm.cfg().OracleDampDrops = boolPtr(true)
		c := &tBasicMMCalculator{bp: 100_000}
		mm.calculator = c
		mm.cfg().BuyPlacements = []*OrderPlacement{{Lots: 1, GapFactor: 0}, {Lots: 1, GapFactor: 0.01}}
		mm.ordersToPlace()
		// Oracle drops 20%; the damped center lags above it.
		c.bp = 80_000
		now = start.Add(time.Minute)
		buys, sells, err := mm.ordersToPlace()
		if err != nil {
			t.Fatal(err)
		}
		// A 0 gap factor still steps one rate step away from the basis.
		if buys[0].Rate != 79_999 || buys[1].Rate != 79_200 {
			t.Fatalf("bids %d, %d, want capped at 79999 / 79200", buys[0].Rate, buys[1].Rate)
		}
		if sells[0].Rate <= 80_800 {
			t.Fatalf("asks should stay on the damped center, got %d", sells[0].Rate)
		}
		// The cap moves with the bid offset.
		mm.cfg().OracleBidOffset = floatPtr(-0.05)
		buys, _, _ = mm.ordersToPlace()
		if buys[0].Rate != 75_999 {
			t.Fatalf("bid %d, want 75999 with -5%% bid offset", buys[0].Rate)
		}
	})

	t.Run("asks never below raw oracle plus ask offset", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(10), nil, &now)
		c := &tBasicMMCalculator{bp: 100_000}
		mm.calculator = c
		mm.cfg().SellPlacements = []*OrderPlacement{{Lots: 1, GapFactor: 0}, {Lots: 1, GapFactor: 0.01}}
		mm.ordersToPlace()
		// A 20% pump: the damped center lags far below it.
		c.bp = 120_000
		now = start.Add(time.Minute)
		buys, sells, err := mm.ordersToPlace()
		if err != nil {
			t.Fatal(err)
		}
		// A 0 gap factor still steps one rate step away from the basis.
		if sells[0].Rate != 120_001 || sells[1].Rate != 121_200 {
			t.Fatalf("asks %d, %d, want floored at 120001 / 121200", sells[0].Rate, sells[1].Rate)
		}
		if buys[0].Rate >= 110_000 {
			t.Fatalf("bids should stay on the lagging center, got %d", buys[0].Rate)
		}
		// The floor moves with the ask offset.
		mm.cfg().OracleAskOffset = floatPtr(0.05)
		_, sells, _ = mm.ordersToPlace()
		if sells[0].Rate != 126_001 {
			t.Fatalf("ask %d, want 126001 with +5%% ask offset", sells[0].Rate)
		}
	})

	t.Run("ask floor beats inventory skew", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(0), nil, &now)
		mm.calculator = &tBasicMMCalculator{bp: 100_000}
		cfg := mm.cfg()
		cfg.InventorySkew = floatPtr(1)
		cfg.InventorySkewCap = floatPtr(0.03)
		cfg.BuyPlacements = []*OrderPlacement{{Lots: 1, GapFactor: 0}}
		cfg.SellPlacements = []*OrderPlacement{{Lots: 1, GapFactor: 0}}
		// Holding 200 lots vs a 100 lot target pushes quotes down 3%.
		mm.initialBalances = map[uint32]uint64{baseID: 100 * lotSize}
		mm.inventoryMods = map[uint32]int64{}
		mm.baseDexBalances[baseID] = int64(200 * lotSize)
		buys, sells, err := mm.ordersToPlace()
		if err != nil {
			t.Fatal(err)
		}
		if sells[0].Rate != 100_001 {
			t.Fatalf("ask %d, want floored at oracle (100001)", sells[0].Rate)
		}
		if buys[0].Rate != 96_999 {
			t.Fatalf("bid %d, want skewed (96999)", buys[0].Rate)
		}
	})

	t.Run("bid cap beats inventory skew", func(t *testing.T) {
		now := start
		mm := newMM(floatPtr(0), nil, &now)
		mm.calculator = &tBasicMMCalculator{bp: 100_000}
		cfg := mm.cfg()
		cfg.InventorySkew = floatPtr(1)
		cfg.InventorySkewCap = floatPtr(0.03)
		cfg.BuyPlacements = []*OrderPlacement{{Lots: 1, GapFactor: 0}}
		cfg.SellPlacements = []*OrderPlacement{{Lots: 1, GapFactor: 0}}
		// Holding no base vs a 100 lot target pushes quotes up 3%.
		mm.initialBalances = map[uint32]uint64{baseID: 100 * lotSize}
		mm.inventoryMods = map[uint32]int64{}
		buys, sells, err := mm.ordersToPlace()
		if err != nil {
			t.Fatal(err)
		}
		// A 0 gap factor still steps one rate step away from the basis.
		if buys[0].Rate != 99_999 {
			t.Fatalf("bid %d, want capped at oracle (99999)", buys[0].Rate)
		}
		if sells[0].Rate != 103_001 {
			t.Fatalf("ask %d, want skewed (103001)", sells[0].Rate)
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
		// Drops are followed at once by default.
		mm = newMM(floatPtr(0), floatPtr(0.01), &now)
		mm.dampedBasis(100_000, now)
		if got := mm.dampedBasis(10_000, now); got != 10_000 {
			t.Fatalf("default drop: got %d, want 10000", got)
		}
		// With OracleDampDrops the cap applies downwards too.
		mm = newMM(floatPtr(0), floatPtr(0.01), &now)
		mm.cfg().OracleDampDrops = boolPtr(true)
		mm.dampedBasis(100_000, now)
		if got := mm.dampedBasis(10_000, now); got != 99_000 {
			t.Fatalf("damped down step: got %d, want 99000", got)
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
		// Bids follow the damped center (150000); asks are held at the
		// raw oracle by the ask floor.
		if buys[0].Rate != 148_500 || sells[0].Rate != 202_000 {
			t.Fatalf("damped ladder %d / %d, want 148500 / 202000", buys[0].Rate, sells[0].Rate)
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
		if ok.OracleDampDrops != nil {
			t.Fatal("absent oracleDampDrops should stay nil (drops followed at once)")
		}
		ok.OracleDampDrops = boolPtr(true)
		cp := ok.copy()
		if cp.OracleDampDrops == ok.OracleDampDrops || !*cp.OracleDampDrops {
			t.Fatal("copy should clone OracleDampDrops")
		}
		if cp.OracleHalfLifeMinutes == ok.OracleHalfLifeMinutes || cp.OracleMaxStep == ok.OracleMaxStep {
			t.Fatal("copy should clone damping pointers")
		}
		if *cp.OracleMaxStep != 0.005 {
			t.Fatal("copy lost max step")
		}
	})
}
