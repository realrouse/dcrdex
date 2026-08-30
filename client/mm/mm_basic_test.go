//go:build !harness && !botlive

package mm

import (
	"math"
	"testing"
	"time"

	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/orderbook"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/order"
)

type tBasicMMCalculator struct {
	bp    uint64
	bpErr error

	hs uint64
}

var _ basicMMCalculator = (*tBasicMMCalculator)(nil)

func (r *tBasicMMCalculator) basisPrice() (uint64, error) {
	return r.bp, r.bpErr
}
func (r *tBasicMMCalculator) halfSpread(basisPrice uint64) (uint64, error) {
	return r.hs, nil
}

func (r *tBasicMMCalculator) feeGapStats(basisPrice uint64) (*FeeGapStats, error) {
	return &FeeGapStats{FeeGap: r.hs * 2}, nil
}
func TestBasisPrice(t *testing.T) {
	mkt := &core.Market{
		RateStep:   1,
		BaseID:     42,
		QuoteID:    0,
		AtomToConv: 1,
	}

	tests := []*struct {
		name        string
		oraclePrice uint64
		fiatRate    uint64
		exp         uint64
	}{
		{
			name:        "oracle price",
			oraclePrice: 2000,
			fiatRate:    1900,
			exp:         2000,
		},
		{
			name:        "failed sanity check",
			oraclePrice: 2000,
			fiatRate:    1850, // mismatch > 5%
			exp:         0,
		},
		{
			name:        "no oracle price",
			oraclePrice: 0,
			fiatRate:    1000,
			exp:         1000,
		},
		{
			name:        "no oracle price or fiat rate",
			oraclePrice: 0,
			fiatRate:    0,
			exp:         0,
		},
	}

	for _, tt := range tests {
		oracle := &tOracle{
			marketPrice: mkt.MsgRateToConventional(tt.oraclePrice),
		}

		tCore := newTCore()
		adaptor := newTBotCoreAdaptor(tCore)
		adaptor.fiatExchangeRate = tt.fiatRate

		calculator := &basicMMCalculatorImpl{
			market: mustParseMarket(mkt),
			oracle: oracle,
			cfg:    &BasicMarketMakingConfig{},
			log:    tLogger,
			core:   adaptor,
		}

		rate, _ := calculator.basisPrice()
		if rate != tt.exp {
			t.Fatalf("%s: %d != %d", tt.name, rate, tt.exp)
		}
	}
}

func TestBreakEvenHalfSpread(t *testing.T) {
	tests := []*struct {
		name                 string
		basisPrice           uint64
		mkt                  *core.Market
		buyFeesInBaseUnits   uint64
		sellFeesInBaseUnits  uint64
		buyFeesInQuoteUnits  uint64
		sellFeesInQuoteUnits uint64
		singleLotFeesErr     error
		expErr               bool
	}{
		{
			name:   "basis price = 0 not allowed",
			expErr: true,
			mkt: &core.Market{
				LotSize: 20e8,
				BaseID:  42,
				QuoteID: 0,
			},
		},
		{
			name:       "dcr/btc",
			basisPrice: 5e7, // 0.4 BTC/DCR, quote lot = 8 BTC
			mkt: &core.Market{
				LotSize: 20e8,
				BaseID:  42,
				QuoteID: 0,
			},
			buyFeesInBaseUnits:   2.2e6,
			sellFeesInBaseUnits:  2e6,
			buyFeesInQuoteUnits:  calc.BaseToQuote(2.2e6, 5e7),
			sellFeesInQuoteUnits: calc.BaseToQuote(2e6, 5e7),
		},
		{
			name:       "btc/usdc.eth",
			basisPrice: calc.MessageRateAlt(43000, 1e8, 1e6),
			mkt: &core.Market{
				BaseID:  0,
				QuoteID: 60001,
				LotSize: 1e7,
			},
			buyFeesInBaseUnits:   1e6,
			sellFeesInBaseUnits:  2e6,
			buyFeesInQuoteUnits:  calc.BaseToQuote(calc.MessageRateAlt(43000, 1e8, 1e6), 1e6),
			sellFeesInQuoteUnits: calc.BaseToQuote(calc.MessageRateAlt(43000, 1e8, 1e6), 2e6),
		},
	}

	for _, tt := range tests {
		tCore := newTCore()
		coreAdaptor := newTBotCoreAdaptor(tCore)
		coreAdaptor.buyFeesInBase = tt.buyFeesInBaseUnits
		coreAdaptor.sellFeesInBase = tt.sellFeesInBaseUnits
		coreAdaptor.buyFeesInQuote = tt.buyFeesInQuoteUnits
		coreAdaptor.sellFeesInQuote = tt.sellFeesInQuoteUnits

		calculator := &basicMMCalculatorImpl{
			market: mustParseMarket(tt.mkt),
			core:   coreAdaptor,
			log:    tLogger,
		}

		halfSpread, err := calculator.halfSpread(tt.basisPrice)
		if (err != nil) != tt.expErr {
			t.Fatalf("expErr = %t, err = %v", tt.expErr, err)
		}
		if tt.expErr {
			continue
		}

		afterSell := calc.BaseToQuote(tt.basisPrice+halfSpread, tt.mkt.LotSize)
		afterBuy := calc.QuoteToBase(tt.basisPrice-halfSpread, afterSell)
		fees := afterBuy - tt.mkt.LotSize
		expectedFees := tt.buyFeesInBaseUnits + tt.sellFeesInBaseUnits

		if expectedFees > fees*10001/10000 || expectedFees < fees*9999/10000 {
			t.Fatalf("%s: expected fees %d, got %d", tt.name, expectedFees, fees)
		}

	}
}

func TestUpdateLotSize(t *testing.T) {
	tests := []struct {
		name           string
		placements     []*OrderPlacement
		originalSize   uint64
		newSize        uint64
		wantPlacements []*OrderPlacement
	}{
		{
			name: "simple halving",
			placements: []*OrderPlacement{
				{Lots: 2, GapFactor: 1.0},
				{Lots: 4, GapFactor: 2.0},
			},
			originalSize: 100,
			newSize:      200,
			wantPlacements: []*OrderPlacement{
				{Lots: 1, GapFactor: 1.0},
				{Lots: 2, GapFactor: 2.0},
			},
		},
		{
			name: "rounding up",
			placements: []*OrderPlacement{
				{Lots: 3, GapFactor: 1.0},
				{Lots: 1, GapFactor: 1.0},
			},
			originalSize: 100,
			newSize:      160,
			wantPlacements: []*OrderPlacement{
				{Lots: 2, GapFactor: 1.0},
			},
		},
		{
			name: "minimum 1 lot",
			placements: []*OrderPlacement{
				{Lots: 1, GapFactor: 1.0},
				{Lots: 1, GapFactor: 1.0},
				{Lots: 1, GapFactor: 1.0},
			},
			originalSize: 100,
			newSize:      250,
			wantPlacements: []*OrderPlacement{
				{Lots: 1, GapFactor: 1.0},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := updateLotSize(tt.placements, tt.originalSize, tt.newSize)
			if len(got) != len(tt.wantPlacements) {
				t.Fatalf("got %d placements, want %d", len(got), len(tt.wantPlacements))
			}
			for i := range got {
				if got[i].Lots != tt.wantPlacements[i].Lots {
					t.Errorf("placement %d: got %d lots, want %d", i, got[i].Lots, tt.wantPlacements[i].Lots)
				}
				if got[i].GapFactor != tt.wantPlacements[i].GapFactor {
					t.Errorf("placement %d: got %f gap factor, want %f", i, got[i].GapFactor, tt.wantPlacements[i].GapFactor)
				}
			}
		})
	}
}

func TestBasicMMRebalance(t *testing.T) {
	const basisPrice uint64 = 5e6
	const halfSpread uint64 = 2e5
	const rateStep uint64 = 1e3
	const atomToConv float64 = 1

	calculator := &tBasicMMCalculator{
		bp: basisPrice,
		hs: halfSpread,
	}

	type test struct {
		name              string
		strategy          GapStrategy
		cfgBuyPlacements  []*OrderPlacement
		cfgSellPlacements []*OrderPlacement

		expBuyPlacements  []*TradePlacement
		expSellPlacements []*TradePlacement
	}
	tests := []*test{
		{
			name:     "multiplier",
			strategy: GapStrategyMultiplier,
			cfgBuyPlacements: []*OrderPlacement{
				{Lots: 1, GapFactor: 3},
				{Lots: 2, GapFactor: 2},
				{Lots: 3, GapFactor: 1},
			},
			cfgSellPlacements: []*OrderPlacement{
				{Lots: 3, GapFactor: 1},
				{Lots: 2, GapFactor: 2},
				{Lots: 1, GapFactor: 3},
			},
			expBuyPlacements: []*TradePlacement{
				{Lots: 1, Rate: steppedRate(basisPrice-3*halfSpread, rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice-2*halfSpread, rateStep)},
				{Lots: 3, Rate: steppedRate(basisPrice-1*halfSpread, rateStep)},
			},
			expSellPlacements: []*TradePlacement{
				{Lots: 3, Rate: steppedRate(basisPrice+1*halfSpread, rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice+2*halfSpread, rateStep)},
				{Lots: 1, Rate: steppedRate(basisPrice+3*halfSpread, rateStep)},
			},
		},
		{
			name:     "percent",
			strategy: GapStrategyPercent,
			cfgBuyPlacements: []*OrderPlacement{
				{Lots: 1, GapFactor: 0.05},
				{Lots: 2, GapFactor: 0.1},
				{Lots: 3, GapFactor: 0.15},
			},
			cfgSellPlacements: []*OrderPlacement{
				{Lots: 3, GapFactor: 0.15},
				{Lots: 2, GapFactor: 0.1},
				{Lots: 1, GapFactor: 0.05},
			},
			expBuyPlacements: []*TradePlacement{
				{Lots: 1, Rate: steppedRate(basisPrice-uint64(math.Round((float64(basisPrice)*0.05))), rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice-uint64(math.Round((float64(basisPrice)*0.1))), rateStep)},
				{Lots: 3, Rate: steppedRate(basisPrice-uint64(math.Round((float64(basisPrice)*0.15))), rateStep)},
			},
			expSellPlacements: []*TradePlacement{
				{Lots: 3, Rate: steppedRate(basisPrice+uint64(math.Round((float64(basisPrice)*0.15))), rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice+uint64(math.Round((float64(basisPrice)*0.1))), rateStep)},
				{Lots: 1, Rate: steppedRate(basisPrice+uint64(math.Round((float64(basisPrice)*0.05))), rateStep)},
			},
		},
		{
			name:     "percent-plus",
			strategy: GapStrategyPercentPlus,
			cfgBuyPlacements: []*OrderPlacement{
				{Lots: 1, GapFactor: 0.05},
				{Lots: 2, GapFactor: 0.1},
				{Lots: 3, GapFactor: 0.15},
			},
			cfgSellPlacements: []*OrderPlacement{
				{Lots: 3, GapFactor: 0.15},
				{Lots: 2, GapFactor: 0.1},
				{Lots: 1, GapFactor: 0.05},
			},
			expBuyPlacements: []*TradePlacement{
				{Lots: 1, Rate: steppedRate(basisPrice-halfSpread-uint64(math.Round((float64(basisPrice)*0.05))), rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice-halfSpread-uint64(math.Round((float64(basisPrice)*0.1))), rateStep)},
				{Lots: 3, Rate: steppedRate(basisPrice-halfSpread-uint64(math.Round((float64(basisPrice)*0.15))), rateStep)},
			},
			expSellPlacements: []*TradePlacement{
				{Lots: 3, Rate: steppedRate(basisPrice+halfSpread+uint64(math.Round((float64(basisPrice)*0.15))), rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice+halfSpread+uint64(math.Round((float64(basisPrice)*0.1))), rateStep)},
				{Lots: 1, Rate: steppedRate(basisPrice+halfSpread+uint64(math.Round((float64(basisPrice)*0.05))), rateStep)},
			},
		},
		{
			name:     "absolute",
			strategy: GapStrategyAbsolute,
			cfgBuyPlacements: []*OrderPlacement{
				{Lots: 1, GapFactor: .01},
				{Lots: 2, GapFactor: .03},
				{Lots: 3, GapFactor: .06},
			},
			cfgSellPlacements: []*OrderPlacement{
				{Lots: 3, GapFactor: .06},
				{Lots: 2, GapFactor: .03},
				{Lots: 1, GapFactor: .01},
			},
			expBuyPlacements: []*TradePlacement{
				{Lots: 1, Rate: steppedRate(basisPrice-1e6, rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice-3e6, rateStep)},
			},
			expSellPlacements: []*TradePlacement{
				{Lots: 3, Rate: steppedRate(basisPrice+6e6, rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice+3e6, rateStep)},
				{Lots: 1, Rate: steppedRate(basisPrice+1e6, rateStep)},
			},
		},
		{
			name:     "absolute-plus",
			strategy: GapStrategyAbsolutePlus,
			cfgBuyPlacements: []*OrderPlacement{
				{Lots: 1, GapFactor: .01},
				{Lots: 2, GapFactor: .03},
				{Lots: 3, GapFactor: .06},
			},
			cfgSellPlacements: []*OrderPlacement{
				{Lots: 3, GapFactor: .06},
				{Lots: 2, GapFactor: .03},
				{Lots: 1, GapFactor: .01},
			},
			expBuyPlacements: []*TradePlacement{
				{Lots: 1, Rate: steppedRate(basisPrice-halfSpread-1e6, rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice-halfSpread-3e6, rateStep)},
			},
			expSellPlacements: []*TradePlacement{
				{Lots: 3, Rate: steppedRate(basisPrice+halfSpread+6e6, rateStep)},
				{Lots: 2, Rate: steppedRate(basisPrice+halfSpread+3e6, rateStep)},
				{Lots: 1, Rate: steppedRate(basisPrice+halfSpread+1e6, rateStep)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const lotSize = 5e9
			const baseID, quoteID = 42, 0
			mm := &basicMarketMaker{
				unifiedExchangeAdaptor: mustParseAdaptorFromMarket(&core.Market{
					RateStep:   rateStep,
					AtomToConv: atomToConv,
					LotSize:    lotSize,
					BaseID:     baseID,
					QuoteID:    quoteID,
				}),
				calculator: calculator,
			}
			tcore := newTCore()
			tcore.setWalletsAndExchange(&core.Market{
				BaseID:  baseID,
				QuoteID: quoteID,
			})
			mm.clientCore = tcore
			mm.botCfgV.Store(&BotConfig{})
			mm.fiatRates.Store(map[uint32]float64{baseID: 1, quoteID: 1})
			const sellSwapFees, sellRedeemFees = 3e6, 1e6
			const buySwapFees, buyRedeemFees = 2e5, 1e5
			mm.buyFees = &OrderFees{
				LotFeeRange: &LotFeeRange{
					Max: &LotFees{
						Redeem: buyRedeemFees,
						Swap:   buySwapFees,
					},
					Estimated: &LotFees{},
				},
				BookingFeesPerLot: buySwapFees,
			}
			mm.sellFees = &OrderFees{
				LotFeeRange: &LotFeeRange{
					Max: &LotFees{
						Redeem: sellRedeemFees,
						Swap:   sellSwapFees,
					},
					Estimated: &LotFees{},
				},
				BookingFeesPerLot: sellSwapFees,
			}
			mm.baseDexBalances[baseID] = lotSize * 50
			mm.baseCexBalances[baseID] = lotSize * 50
			mm.baseDexBalances[quoteID] = int64(calc.BaseToQuote(basisPrice, lotSize*50))
			mm.baseCexBalances[quoteID] = int64(calc.BaseToQuote(basisPrice, lotSize*50))
			mm.unifiedExchangeAdaptor.botCfgV.Store(&BotConfig{
				BasicMMConfig: &BasicMarketMakingConfig{
					GapStrategy:    tt.strategy,
					BuyPlacements:  tt.cfgBuyPlacements,
					SellPlacements: tt.cfgSellPlacements,
				}})

			mm.rebalance(100)

			if len(tcore.multiTradesPlaced) != 2 {
				t.Fatal("expected both buy and sell orders placed")
			}
			buys, sells := tcore.multiTradesPlaced[0], tcore.multiTradesPlaced[1]
			if buys.Sell {
				buys, sells = sells, buys
			}

			expOrdersN := len(tt.expBuyPlacements) + len(tt.expSellPlacements)
			if len(buys.Placements)+len(sells.Placements) != expOrdersN {
				t.Fatalf("expected %d orders, got %d", expOrdersN, len(buys.Placements)+len(sells.Placements))
			}

			buyRateLots := make(map[uint64]uint64, len(buys.Placements))
			for _, p := range buys.Placements {
				buyRateLots[p.Rate] = p.Qty / lotSize
			}
			for _, expBuy := range tt.expBuyPlacements {
				if lots, found := buyRateLots[expBuy.Rate]; !found {
					t.Fatalf("buy rate %d not found", expBuy.Rate)
				} else {
					if expBuy.Lots != lots {
						t.Fatalf("wrong lots %d for buy at rate %d", lots, expBuy.Rate)
					}
				}
			}
			sellRateLots := make(map[uint64]uint64, len(sells.Placements))
			for _, p := range sells.Placements {
				sellRateLots[p.Rate] = p.Qty / lotSize
			}
			for _, expSell := range tt.expSellPlacements {
				if lots, found := sellRateLots[expSell.Rate]; !found {
					t.Fatalf("sell rate %d not found", expSell.Rate)
				} else {
					if expSell.Lots != lots {
						t.Fatalf("wrong lots %d for sell at rate %d", lots, expSell.Rate)
					}
				}
			}
		})
	}
}

func TestBasicMMConfigInventorySkewDefaults(t *testing.T) {
	cfg := &BasicMarketMakingConfig{GapStrategy: GapStrategyPercentPlus}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.InventorySkew == nil || *cfg.InventorySkew != defaultInventorySkew {
		t.Fatalf("expected default skew %v, got %v", defaultInventorySkew, cfg.InventorySkew)
	}
	if cfg.InventorySkewCap == nil || *cfg.InventorySkewCap != defaultInventorySkewCap {
		t.Fatalf("expected default cap %v, got %v", defaultInventorySkewCap, cfg.InventorySkewCap)
	}
	if cfg.DoNotCross == nil || *cfg.DoNotCross != defaultDoNotCross {
		t.Fatalf("expected default doNotCross %v, got %v", defaultDoNotCross, cfg.DoNotCross)
	}
	if cfg.BidAnchorFadeHours == nil || *cfg.BidAnchorFadeHours != defaultBidAnchorFadeHours {
		t.Fatalf("expected default fade hours %v, got %v", defaultBidAnchorFadeHours, cfg.BidAnchorFadeHours)
	}

	off := 0.0
	cfgOff := &BasicMarketMakingConfig{
		GapStrategy:        GapStrategyPercentPlus,
		InventorySkew:      &off,
		BidAnchorFadeHours: &off,
		DoNotCross:         boolPtr(false),
	}
	if err := cfgOff.validate(); err != nil {
		t.Fatalf("validate explicit 0: %v", err)
	}
	if *cfgOff.InventorySkew != 0 {
		t.Fatalf("explicit 0 should stay off, got %v", *cfgOff.InventorySkew)
	}
	if *cfgOff.BidAnchorFadeHours != 0 {
		t.Fatalf("explicit fade 0 should stay 0, got %v", *cfgOff.BidAnchorFadeHours)
	}
	if *cfgOff.DoNotCross {
		t.Fatal("explicit doNotCross false should stay false")
	}

	tooHigh := 1.1
	cfgBad := &BasicMarketMakingConfig{
		GapStrategy:   GapStrategyPercentPlus,
		InventorySkew: &tooHigh,
	}
	if err := cfgBad.validate(); err == nil {
		t.Fatal("expected error for skew > 1")
	}

	tooLong := 25.0
	cfgLong := &BasicMarketMakingConfig{
		GapStrategy:        GapStrategyPercentPlus,
		BidAnchorFadeHours: &tooLong,
	}
	if err := cfgLong.validate(); err == nil {
		t.Fatal("expected error for fade hours > 24")
	}

	copied := cfgOff.copy()
	if copied.DoNotCross == cfgOff.DoNotCross {
		t.Fatal("copy should clone DoNotCross pointer")
	}
	if *copied.BidAnchorFadeHours != 0 || *copied.DoNotCross {
		t.Fatal("copy lost explicit zeros")
	}
}

func TestApplyInventorySkew(t *testing.T) {
	const (
		baseID     = uint32(42)
		quoteID    = uint32(0)
		basis      = uint64(100_000)
		rateStep   = uint64(1)
		lotSize    = uint64(1e8)
		quotedLots = uint64(10)
		quotedAmt  = lotSize * quotedLots
		warehouse  = uint64(100)
		targetAmt  = lotSize * warehouse
	)

	newBot := func(current uint64, skew, cap float64, sellLots, buyLots uint64) *basicMarketMaker {
		mm := &basicMarketMaker{
			unifiedExchangeAdaptor: mustParseAdaptorFromMarket(&core.Market{
				RateStep:   rateStep,
				AtomToConv: 1,
				LotSize:    lotSize,
				BaseID:     baseID,
				QuoteID:    quoteID,
			}),
		}
		mm.initialBalances = map[uint32]uint64{baseID: targetAmt}
		mm.inventoryMods = map[uint32]int64{}
		mm.baseDexBalances[baseID] = int64(current)
		mm.botCfgV.Store(&BotConfig{
			BasicMMConfig: &BasicMarketMakingConfig{
				GapStrategy:      GapStrategyPercentPlus,
				SellPlacements:   []*OrderPlacement{{Lots: sellLots, GapFactor: 0.01}},
				BuyPlacements:    []*OrderPlacement{{Lots: buyLots, GapFactor: 0.01}},
				InventorySkew:    floatPtr(skew),
				InventorySkewCap: floatPtr(cap),
			},
		})
		return mm
	}

	want := func(basis uint64, n, skew, cap float64) uint64 {
		shift := -n * skew * cap
		if shift > cap {
			shift = cap
		} else if shift < -cap {
			shift = -cap
		}
		return steppedRate(uint64(math.Round(float64(basis)*(1+shift))), rateStep)
	}

	t.Run("balanced", func(t *testing.T) {
		got := newBot(targetAmt, 1, 0.03, quotedLots, quotedLots).applyInventorySkew(basis)
		if got != basis {
			t.Fatalf("got %d, want %d", got, basis)
		}
	})
	t.Run("one quoted lot sold", func(t *testing.T) {
		got := newBot(targetAmt-lotSize, 1, 0.03, quotedLots, quotedLots).applyInventorySkew(basis)
		exp := want(basis, -0.1, 1, 0.03)
		if got != exp {
			t.Fatalf("got %d, want %d", got, exp)
		}
	})
	t.Run("warehouse does not dilute", func(t *testing.T) {
		a := newBot(targetAmt-lotSize, 1, 0.03, quotedLots, quotedLots).applyInventorySkew(basis)
		mm := newBot(targetAmt*10-lotSize, 1, 0.03, quotedLots, quotedLots)
		mm.initialBalances[baseID] = targetAmt * 10
		b := mm.applyInventorySkew(basis)
		if a != b {
			t.Fatalf("warehouse size changed n: %d vs %d", a, b)
		}
		if a == basis {
			t.Fatal("expected a visible shift from one quoted lot")
		}
	})
	t.Run("all quoted lots sold hits cap", func(t *testing.T) {
		got := newBot(targetAmt-quotedAmt, 1, 0.03, quotedLots, quotedLots).applyInventorySkew(basis)
		exp := want(basis, -1, 1, 0.03)
		if got != exp {
			t.Fatalf("got %d, want %d", got, exp)
		}
	})
	t.Run("over-sold capped", func(t *testing.T) {
		got := newBot(targetAmt-50*lotSize, 1, 0.03, quotedLots, quotedLots).applyInventorySkew(basis)
		exp := want(basis, -5, 1, 0.03)
		if got != exp {
			t.Fatalf("got %d, want %d", got, exp)
		}
	})
	t.Run("one quoted lot bought", func(t *testing.T) {
		got := newBot(targetAmt+lotSize, 1, 0.03, quotedLots, quotedLots).applyInventorySkew(basis)
		exp := want(basis, 0.1, 1, 0.03)
		if got != exp {
			t.Fatalf("got %d, want %d", got, exp)
		}
	})
	t.Run("skew off", func(t *testing.T) {
		got := newBot(0, 0, 0.03, quotedLots, quotedLots).applyInventorySkew(basis)
		if got != basis {
			t.Fatalf("got %d, want %d", got, basis)
		}
	})
	t.Run("no target", func(t *testing.T) {
		mm := newBot(targetAmt, 1, 0.03, quotedLots, quotedLots)
		mm.initialBalances = map[uint32]uint64{}
		got := mm.applyInventorySkew(basis)
		if got != basis {
			t.Fatalf("got %d, want %d", got, basis)
		}
	})
	t.Run("inventory add raises target", func(t *testing.T) {
		mm := newBot(targetAmt*2, 1, 0.03, quotedLots, quotedLots)
		mm.inventoryMods[baseID] = int64(targetAmt)
		got := mm.applyInventorySkew(basis)
		if got != basis {
			t.Fatalf("got %d, want %d after inventory add", got, basis)
		}
	})
}

type tBook struct {
	bids, asks []*orderbook.Order
	matches    []*orderbook.MatchSummary
}

func (b *tBook) BestNOrders(n int, sell bool) ([]*orderbook.Order, bool, error) {
	src := b.bids
	if sell {
		src = b.asks
	}
	if n > len(src) {
		n = len(src)
	}
	out := make([]*orderbook.Order, n)
	copy(out, src[:n])
	return out, len(src) <= n, nil
}

func (b *tBook) RecentMatches() []*orderbook.MatchSummary { return b.matches }

func TestLerpAndClampAnchor(t *testing.T) {
	if got := lerpRate(17400, 16360, 0); got != 17400 {
		t.Fatalf("t=0: got %d", got)
	}
	if got := lerpRate(17400, 16360, 1); got != 16360 {
		t.Fatalf("t=1: got %d", got)
	}
	if got := lerpRate(17400, 16360, 0.25); got != 17140 {
		t.Fatalf("t=0.25: got %d, want 17140", got)
	}
	if got := lerpRate(17400, 16360, 0.5); got != 16880 {
		t.Fatalf("t=0.5: got %d, want 16880", got)
	}
	if got := lerpRate(17400, 16360, 0.75); got != 16620 {
		t.Fatalf("t=0.75: got %d, want 16620", got)
	}

	const inv = uint64(16360)
	if got := clampAnchor(17400, inv, true); got != 17400 {
		t.Fatalf("6%% bid should not collar, got %d", got)
	}
	if got := clampAnchor(inv*10, inv, true); got != inv*2 {
		t.Fatalf("10x bid should collar at 2x, got %d", got)
	}
	if got := clampAnchor(inv/10, inv, false); got != inv/2 {
		t.Fatalf("0.1x ask should collar at 0.5x, got %d", got)
	}
}

func TestBookProtection(t *testing.T) {
	const (
		baseID   = uint32(42)
		quoteID  = uint32(0)
		oracle   = uint64(16360)
		rateStep = uint64(10)
		lotSize  = uint64(1e8)
		bid1740  = uint64(17400)
	)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newMM := func(book *tBook, fadeHours float64, doNotCross bool) (*basicMarketMaker, *time.Time) {
		now := start
		mm := &basicMarketMaker{
			unifiedExchangeAdaptor: mustParseAdaptorFromMarket(&core.Market{
				RateStep:   rateStep,
				AtomToConv: 1,
				LotSize:    lotSize,
				BaseID:     baseID,
				QuoteID:    quoteID,
			}),
			book:  book,
			nowFn: func() time.Time { return now },
		}
		mm.calculator = &tBasicMMCalculator{bp: oracle, hs: 0}
		mm.botCfgV.Store(&BotConfig{
			BasicMMConfig: &BasicMarketMakingConfig{
				GapStrategy:        GapStrategyPercent,
				SellPlacements:     []*OrderPlacement{{Lots: 1, GapFactor: 0}, {Lots: 1, GapFactor: 0.01}},
				BuyPlacements:      []*OrderPlacement{{Lots: 1, GapFactor: 0}, {Lots: 1, GapFactor: 0.01}},
				InventorySkew:      floatPtr(0),
				DoNotCross:         boolPtr(doNotCross),
				BidAnchorFadeHours: floatPtr(fadeHours),
			},
		})
		return mm, &now
	}

	sellRates := func(mm *basicMarketMaker) []uint64 {
		_, sells, err := mm.ordersToPlace()
		if err != nil {
			t.Fatalf("ordersToPlace: %v", err)
		}
		out := make([]uint64, len(sells))
		for i, p := range sells {
			out[i] = p.Rate
		}
		return out
	}
	buyRates := func(mm *basicMarketMaker) []uint64 {
		buys, _, err := mm.ordersToPlace()
		if err != nil {
			t.Fatalf("ordersToPlace: %v", err)
		}
		out := make([]uint64, len(buys))
		for i, p := range buys {
			out[i] = p.Rate
		}
		return out
	}

	t.Run("do-not-cross live bid", func(t *testing.T) {
		book := &tBook{bids: []*orderbook.Order{{Rate: bid1740, OrderID: order.OrderID{9}}}}
		mm, _ := newMM(book, 4, true)
		sells := sellRates(mm)
		if sells[0] <= bid1740 {
			t.Fatalf("closest ask %d still crosses bid %d", sells[0], bid1740)
		}
		if sells[0] != bid1740+rateStep {
			t.Fatalf("closest ask %d, want %d", sells[0], bid1740+rateStep)
		}
		if sells[1] <= sells[0] {
			t.Fatalf("ladder collapsed: %v", sells)
		}
	})

	t.Run("own bid is ignored", func(t *testing.T) {
		var oid order.OrderID
		oid[0] = 7
		book := &tBook{bids: []*orderbook.Order{{Rate: bid1740, OrderID: oid}}}
		mm, _ := newMM(book, 4, true)
		po := &pendingDEXOrder{}
		po.state.Store(&dexOrderState{order: &core.Order{
			ID:     oid[:],
			Sell:   false,
			Rate:   bid1740,
			Status: order.OrderStatusBooked,
			Qty:    lotSize,
		}})
		mm.pendingDEXOrders[oid] = po
		sells := sellRates(mm)
		// Gap 0 still adds one rateStep (steppedRate never returns 0).
		if sells[0] != oracle+rateStep {
			t.Fatalf("own bid should not raise asks, got %d want %d", sells[0], oracle+rateStep)
		}
	})

	t.Run("live bid does not fade", func(t *testing.T) {
		book := &tBook{bids: []*orderbook.Order{{Rate: bid1740, OrderID: order.OrderID{9}}}}
		mm, now := newMM(book, 4, true)
		_ = sellRates(mm)
		*now = start.Add(2 * time.Hour)
		sells := sellRates(mm)
		if sells[0] != bid1740+rateStep {
			t.Fatalf("still-live bid faded: got %d", sells[0])
		}
	})

	t.Run("linear fade after cancel", func(t *testing.T) {
		book := &tBook{bids: []*orderbook.Order{{Rate: bid1740, OrderID: order.OrderID{9}}}}
		mm, now := newMM(book, 4, true)
		_ = sellRates(mm) // capture live anchor
		book.bids = nil
		*now = start.Add(time.Second) // cancel
		_ = sellRates(mm)             // start fade clock

		// Closest ask is floor + rateStep (percent gap 0 still steps by rateStep).
		wantAsk := func(floor uint64) uint64 { return steppedRate(floor, rateStep) + rateStep }
		cases := []struct {
			after time.Duration
			floor uint64
			done  bool
		}{
			{0, bid1740, false},
			{time.Hour, 17140, false},
			{2 * time.Hour, 16880, false},
			{3 * time.Hour, 16620, false},
			{4 * time.Hour, oracle, true},
		}
		for _, tc := range cases {
			*now = start.Add(time.Second + tc.after)
			sells := sellRates(mm)
			got := sells[0]
			if tc.done {
				if got != oracle+rateStep {
					t.Fatalf("after 4h got %d, want %d", got, oracle+rateStep)
				}
				continue
			}
			exp := wantAsk(tc.floor)
			if got != exp {
				t.Fatalf("after %s got %d, want %d", tc.after, got, exp)
			}
		}
	})

	t.Run("re-bid resets fade", func(t *testing.T) {
		book := &tBook{bids: []*orderbook.Order{{Rate: bid1740, OrderID: order.OrderID{9}}}}
		mm, now := newMM(book, 4, true)
		_ = sellRates(mm)
		book.bids = nil
		*now = start.Add(time.Second)
		_ = sellRates(mm)
		*now = start.Add(time.Second + 2*time.Hour)
		mid := sellRates(mm)
		if mid[0] >= bid1740 {
			t.Fatalf("expected fade by 2h, got %d", mid[0])
		}
		book.bids = []*orderbook.Order{{Rate: bid1740, OrderID: order.OrderID{9}}}
		*now = start.Add(time.Second + 2*time.Hour + time.Second)
		back := sellRates(mm)
		if back[0] != bid1740+rateStep {
			t.Fatalf("re-bid should restore floor, got %d", back[0])
		}
	})

	t.Run("fade hours 0 snaps back", func(t *testing.T) {
		book := &tBook{bids: []*orderbook.Order{{Rate: bid1740, OrderID: order.OrderID{9}}}}
		mm, now := newMM(book, 0, true)
		live := sellRates(mm)
		if live[0] <= bid1740 {
			t.Fatalf("live bid must still not cross, got %d", live[0])
		}
		book.bids = nil
		*now = start.Add(5 * time.Second)
		snap := sellRates(mm)
		if snap[0] != oracle+rateStep {
			t.Fatalf("hours=0 should snap to oracle book, got %d want %d", snap[0], oracle+rateStep)
		}
	})

	t.Run("do-not-cross off restocks through bid", func(t *testing.T) {
		book := &tBook{bids: []*orderbook.Order{{Rate: bid1740, OrderID: order.OrderID{9}}}}
		mm, _ := newMM(book, 4, false)
		sells := sellRates(mm)
		if sells[0] != oracle+rateStep {
			t.Fatalf("do-not-cross off: got %d want %d", sells[0], oracle+rateStep)
		}
	})

	t.Run("full lift no remainder", func(t *testing.T) {
		book := &tBook{
			matches: []*orderbook.MatchSummary{{
				Rate:  bid1740,
				Qty:   lotSize,
				Stamp: uint64(start.UnixMilli()),
			}},
		}
		mm, now := newMM(book, 4, true)
		var oid order.OrderID
		oid[0] = 3
		po := &pendingDEXOrder{}
		po.state.Store(&dexOrderState{order: &core.Order{
			ID:     oid[:],
			Sell:   true,
			Rate:   oracle,
			Status: order.OrderStatusBooked,
			Qty:    lotSize,
		}})
		mm.pendingDEXOrders[oid] = po
		_ = sellRates(mm) // prevSellBookedLots = 1
		delete(mm.pendingDEXOrders, oid)
		*now = start.Add(time.Second)
		sells := sellRates(mm)
		if sells[0] < bid1740 {
			t.Fatalf("lift with no remainder should set floor, got %d", sells[0])
		}
	})

	t.Run("dump side", func(t *testing.T) {
		const dumpAsk = uint64(15000)
		book := &tBook{asks: []*orderbook.Order{{Rate: dumpAsk, OrderID: order.OrderID{8}}}}
		mm, now := newMM(book, 4, true)
		buys := buyRates(mm)
		if buys[0] >= dumpAsk {
			t.Fatalf("must not buy into dump ask: %d >= %d", buys[0], dumpAsk)
		}
		book.asks = nil
		*now = start.Add(time.Second)
		_ = buyRates(mm)
		*now = start.Add(time.Second + 2*time.Hour)
		mid := buyRates(mm)
		if mid[0] <= dumpAsk {
			t.Fatalf("bids should fade up off the dump, got %d", mid[0])
		}
		if mid[0] >= oracle {
			t.Fatalf("2h fade should not have reached oracle yet, got %d", mid[0])
		}
	})
}
