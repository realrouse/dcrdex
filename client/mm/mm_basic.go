// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/orderbook"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/order"
	"decred.org/dcrdex/dex/utils"
)

// GapStrategy is a specifier for an algorithm to choose the maker bot's target
// spread.
type GapStrategy string

const (
	// GapStrategyMultiplier calculates the spread by multiplying the
	// break-even gap by the specified multiplier, 1 <= r <= 100.
	GapStrategyMultiplier GapStrategy = "multiplier"
	// GapStrategyAbsolute sets the spread to the rate difference.
	GapStrategyAbsolute GapStrategy = "absolute"
	// GapStrategyAbsolutePlus sets the spread to the rate difference plus the
	// break-even gap.
	GapStrategyAbsolutePlus GapStrategy = "absolute-plus"
	// GapStrategyPercent sets the spread as a ratio of the mid-gap rate.
	// 0 <= r <= 0.1
	GapStrategyPercent GapStrategy = "percent"
	// GapStrategyPercentPlus sets the spread as a ratio of the mid-gap rate
	// plus the break-even gap.
	GapStrategyPercentPlus GapStrategy = "percent-plus"
)

// OrderPlacement represents the distance from the mid-gap and the
// amount of lots that should be placed at this distance.
type OrderPlacement struct {
	// Lots is the max number of lots to place at this distance from the
	// mid-gap rate. If there is not enough balance to place this amount
	// of lots, the max that can be afforded will be placed.
	Lots uint64 `json:"lots"`

	// GapFactor controls the gap width in a way determined by the GapStrategy.
	GapFactor float64 `json:"gapFactor"`
}

// BasicMarketMakingConfig is the configuration for a simple market
// maker that places orders on both sides of the order book.
type BasicMarketMakingConfig struct {
	// GapStrategy selects an algorithm for calculating the distance from
	// the basis price to place orders.
	GapStrategy GapStrategy `json:"gapStrategy"`

	// SellPlacements is a list of order placements for sell orders.
	// The orders are prioritized from the first in this list to the
	// last.
	SellPlacements []*OrderPlacement `json:"sellPlacements"`

	// BuyPlacements is a list of order placements for buy orders.
	// The orders are prioritized from the first in this list to the
	// last.
	BuyPlacements []*OrderPlacement `json:"buyPlacements"`

	// DriftTolerance is how far away from an ideal price orders can drift
	// before they are replaced (units: ratio of price). Default: 0.1%.
	// 0 <= x <= 0.01.
	DriftTolerance float64 `json:"driftTolerance"`

	// InventorySkew is how strongly quotes slide when the bot's base
	// inventory differs from its allocated target. 0 = off, 1 = full
	// (a 100% inventory deviation moves price by InventorySkewCap).
	// Nil means default 1. Explicit 0 stays off.
	InventorySkew *float64 `json:"inventorySkew,omitempty"`

	// InventorySkewCap is the maximum fractional shift away from the
	// oracle basis (e.g. 0.03 = 3%). Nil means default 0.03.
	InventorySkewCap *float64 `json:"inventorySkewCap,omitempty"`

	// PersistInventorySkew, when true, makes inventory skew survive bot
	// restarts. Without it, every start takes the allocation as the new
	// target, so base accumulated (or sold) in earlier runs stops producing
	// skew. With it, the bot saves its base inventory deviation from the
	// target (current minus target, in atoms) to mm_inventory_skew.json in
	// the market making data directory (next to the event log DB, e.g.
	// <appdata>/mainnet) at most once a minute and when it stops, and on
	// start shifts the target by the saved deviation. To reset, stop the
	// bot and delete its entry (or the file). Nil or false = off (the
	// default), and nothing is read or written.
	PersistInventorySkew *bool `json:"persistInventorySkew,omitempty"`

	// DoNotCross, when true, never places a sell at or below a live
	// foreign bid, or a buy at or above a live foreign ask. Nil means
	// default true. Explicit false stays off.
	DoNotCross *bool `json:"doNotCross,omitempty"`

	// BidAnchorFadeHours is how long, after an aggressive foreign bid
	// or ask that would have taken our restock is gone, to walk quotes
	// linearly back to the inventory-skewed oracle. Nil means default 4.
	// Explicit 0 means snap back as soon as it cancels (do-not-cross
	// still applies while it is live). 0 <= x <= 24.
	BidAnchorFadeHours *float64 `json:"bidAnchorFadeHours,omitempty"`

	// OracleHalfLifeMinutes damps how the ladder center follows the basis
	// (oracle) price. The center is an exponential moving average of the
	// basis price that closes half the remaining gap every this many
	// minutes, so a short spike in a volatile oracle barely moves the book
	// while a lasting move is followed with a lag. By default only rises
	// are damped and drops are followed at once (see OracleDampDrops).
	// Nil means default 10.
	// Explicit 0 turns damping off and follows the oracle immediately (the
	// behaviour before this option). The average starts at the first price
	// seen after the bot starts. 0 <= x <= 1440.
	OracleHalfLifeMinutes *float64 `json:"oracleHalfLifeMinutes,omitempty"`

	// OracleMaxStep caps how far the ladder center may rise in one update
	// (one epoch), as a fraction of the center (0.005 = 0.5%). It applies
	// after OracleHalfLifeMinutes. It limits rises only, since drops are
	// followed at once, unless OracleDampDrops is set, in which case it
	// limits drops too. Nil or 0 = no cap. 0 <= x <= 0.5.
	OracleMaxStep *float64 `json:"oracleMaxStep,omitempty"`

	// OracleDampDrops, when true, damps falls of the oracle price the same
	// way as rises. By default (nil or false) only rises are damped: when
	// the oracle drops below the damped center, the center follows it at
	// once (OracleMaxStep is not applied to drops either), so the bot never
	// keeps buying above a falling market.
	//
	// Independently of this setting, two guards always apply against the
	// raw (undamped) oracle price:
	//   - bid cap: no buy is placed above raw oracle * (1 + bid offset),
	//   - ask floor: no sell is placed below raw oracle * (1 + ask offset),
	// where the offsets are OracleBidOffset / OracleAskOffset, else
	// OracleOffset, else 0. They win over a lagging damped center (e.g.
	// selling far under market while the center catches up after a pump)
	// and over inventory skew, which can therefore move quotes away from
	// the oracle but never through it.
	OracleDampDrops *bool `json:"oracleDampDrops,omitempty"`

	// OracleOffset keeps the ladder center above or below the (damped)
	// oracle price, as a fraction of it, for both sides: 0.02 places the
	// center 2% above the oracle (a premium), -0.02 places it 2% below.
	// Inventory skew and book protection then work from the offset center.
	// Nil or 0 = no offset. -0.5 <= x <= 0.5.
	OracleOffset *float64 `json:"oracleOffset,omitempty"`

	// OracleBidOffset and OracleAskOffset set the offset separately for
	// the buy and sell ladders, in the same units as OracleOffset (0.02 =
	// 2% above the oracle, -0.02 = 2% below). A side without its own
	// offset uses OracleOffset. For example a bid offset of -0.01 and an
	// ask offset of 0.03 centers buys 1% below and sells 3% above the
	// oracle. -0.5 <= x <= 0.5.
	OracleBidOffset *float64 `json:"oracleBidOffset,omitempty"`
	OracleAskOffset *float64 `json:"oracleAskOffset,omitempty"`

	// AutoTopUp is an opt-in per-asset hard cap, in atoms, on how much the
	// running bot may add to its own allocation from the wallet's
	// unallocated balance when a side cannot be fully placed for lack of
	// funds. The cap is the total for one bot run. Nil or empty = off
	// (the default); assets without an entry, or with 0, are never
	// topped up.
	AutoTopUp map[uint32]uint64 `json:"autoTopUp,omitempty"`
}

const (
	defaultInventorySkew      = 1.0
	defaultInventorySkewCap   = 0.03
	defaultDoNotCross         = true
	defaultBidAnchorFadeHours = 4.0
	maxBidAnchorFadeHours     = 24.0
	// defaultOracleHalfLifeMinutes is the ladder center damping used when
	// OracleHalfLifeMinutes is not set.
	defaultOracleHalfLifeMinutes = 10.0
	maxOracleHalfLifeMinutes     = 1440.0
	maxOracleMaxStep             = 0.5
	maxOracleOffset              = 0.5
	anchorCollarMult             = 2.0
	recentMatchWindow            = 30 * time.Second
	// autoTopUpInterval is the minimum time between two auto top-ups of
	// the same asset.
	autoTopUpInterval = time.Minute
)

func floatPtr(v float64) *float64 { return &v }
func boolPtr(v bool) *bool        { return &v }

func needBreakEvenHalfSpread(strat GapStrategy) bool {
	return strat == GapStrategyAbsolutePlus || strat == GapStrategyPercentPlus || strat == GapStrategyMultiplier
}

func (c *BasicMarketMakingConfig) validate() error {
	if c.DriftTolerance == 0 {
		c.DriftTolerance = 0.001
	}
	if c.DriftTolerance < 0 || c.DriftTolerance > 0.01 {
		return fmt.Errorf("drift tolerance %f out of bounds", c.DriftTolerance)
	}

	if c.InventorySkew == nil {
		c.InventorySkew = floatPtr(defaultInventorySkew)
	} else if *c.InventorySkew < 0 || *c.InventorySkew > 1 {
		return fmt.Errorf("inventory skew %f out of bounds [0, 1]", *c.InventorySkew)
	}
	if c.InventorySkewCap == nil {
		c.InventorySkewCap = floatPtr(defaultInventorySkewCap)
	} else if *c.InventorySkewCap < 0 || *c.InventorySkewCap > 0.1 {
		return fmt.Errorf("inventory skew cap %f out of bounds [0, 0.1]", *c.InventorySkewCap)
	}

	if c.DoNotCross == nil {
		c.DoNotCross = boolPtr(defaultDoNotCross)
	}

	if c.BidAnchorFadeHours == nil {
		c.BidAnchorFadeHours = floatPtr(defaultBidAnchorFadeHours)
	} else if *c.BidAnchorFadeHours < 0 || *c.BidAnchorFadeHours > maxBidAnchorFadeHours {
		return fmt.Errorf("bid anchor fade hours %f out of bounds [0, %g]", *c.BidAnchorFadeHours, maxBidAnchorFadeHours)
	}

	if c.OracleHalfLifeMinutes == nil {
		c.OracleHalfLifeMinutes = floatPtr(defaultOracleHalfLifeMinutes)
	} else if v := *c.OracleHalfLifeMinutes; v < 0 || v > maxOracleHalfLifeMinutes || math.IsNaN(v) {
		return fmt.Errorf("oracle half-life minutes %f out of bounds [0, %g]", v, maxOracleHalfLifeMinutes)
	}
	if c.OracleMaxStep != nil {
		if v := *c.OracleMaxStep; v < 0 || v > maxOracleMaxStep || math.IsNaN(v) {
			return fmt.Errorf("oracle max step %f out of bounds [0, %g]", v, maxOracleMaxStep)
		}
	}

	for _, o := range []struct {
		name string
		v    *float64
	}{{"oracle offset", c.OracleOffset}, {"oracle bid offset", c.OracleBidOffset}, {"oracle ask offset", c.OracleAskOffset}} {
		if o.v == nil {
			continue
		}
		if v := *o.v; v < -maxOracleOffset || v > maxOracleOffset || math.IsNaN(v) {
			return fmt.Errorf("%s %f out of bounds [-%g, %g]", o.name, v, maxOracleOffset, maxOracleOffset)
		}
	}
	// A bid center above the ask center would make the bot's own buys and
	// sells cross.
	if bidOff, askOff := c.oracleOffsets(); bidOff > askOff {
		return fmt.Errorf("effective oracle bid offset %g is above the ask offset %g", bidOff, askOff)
	}

	if c.GapStrategy != GapStrategyMultiplier &&
		c.GapStrategy != GapStrategyPercent &&
		c.GapStrategy != GapStrategyPercentPlus &&
		c.GapStrategy != GapStrategyAbsolute &&
		c.GapStrategy != GapStrategyAbsolutePlus {
		return fmt.Errorf("unknown gap strategy %q", c.GapStrategy)
	}

	validatePlacement := func(p *OrderPlacement) error {
		var limits [2]float64
		switch c.GapStrategy {
		case GapStrategyMultiplier:
			limits = [2]float64{1, 100}
		case GapStrategyPercent, GapStrategyPercentPlus:
			limits = [2]float64{0, 0.1}
		case GapStrategyAbsolute, GapStrategyAbsolutePlus:
			limits = [2]float64{0, math.MaxFloat64} // validate at < spot price at creation time
		default:
			return fmt.Errorf("unknown gap strategy %q", c.GapStrategy)
		}

		if p.GapFactor < limits[0] || p.GapFactor > limits[1] {
			return fmt.Errorf("%s gap factor %f is out of bounds %+v", c.GapStrategy, p.GapFactor, limits)
		}

		return nil
	}

	sellPlacements := make(map[float64]bool, len(c.SellPlacements))
	for _, p := range c.SellPlacements {
		if _, duplicate := sellPlacements[p.GapFactor]; duplicate {
			return fmt.Errorf("duplicate sell placement %f", p.GapFactor)
		}
		sellPlacements[p.GapFactor] = true
		if err := validatePlacement(p); err != nil {
			return fmt.Errorf("invalid sell placement: %w", err)
		}
	}

	buyPlacements := make(map[float64]bool, len(c.BuyPlacements))
	for _, p := range c.BuyPlacements {
		if _, duplicate := buyPlacements[p.GapFactor]; duplicate {
			return fmt.Errorf("duplicate buy placement %f", p.GapFactor)
		}
		buyPlacements[p.GapFactor] = true
		if err := validatePlacement(p); err != nil {
			return fmt.Errorf("invalid buy placement: %w", err)
		}
	}

	return nil
}

func (c *BasicMarketMakingConfig) copy() *BasicMarketMakingConfig {
	cfg := *c

	copyOrderPlacement := func(p *OrderPlacement) *OrderPlacement {
		return &OrderPlacement{
			Lots:      p.Lots,
			GapFactor: p.GapFactor,
		}
	}

	cfg.SellPlacements = utils.Map(c.SellPlacements, copyOrderPlacement)
	cfg.BuyPlacements = utils.Map(c.BuyPlacements, copyOrderPlacement)

	if c.InventorySkew != nil {
		v := *c.InventorySkew
		cfg.InventorySkew = &v
	}
	if c.InventorySkewCap != nil {
		v := *c.InventorySkewCap
		cfg.InventorySkewCap = &v
	}
	if c.PersistInventorySkew != nil {
		v := *c.PersistInventorySkew
		cfg.PersistInventorySkew = &v
	}
	if c.DoNotCross != nil {
		v := *c.DoNotCross
		cfg.DoNotCross = &v
	}
	if c.BidAnchorFadeHours != nil {
		v := *c.BidAnchorFadeHours
		cfg.BidAnchorFadeHours = &v
	}
	if c.OracleHalfLifeMinutes != nil {
		v := *c.OracleHalfLifeMinutes
		cfg.OracleHalfLifeMinutes = &v
	}
	if c.OracleMaxStep != nil {
		v := *c.OracleMaxStep
		cfg.OracleMaxStep = &v
	}
	if c.OracleDampDrops != nil {
		v := *c.OracleDampDrops
		cfg.OracleDampDrops = &v
	}
	if c.OracleOffset != nil {
		v := *c.OracleOffset
		cfg.OracleOffset = &v
	}
	if c.OracleBidOffset != nil {
		v := *c.OracleBidOffset
		cfg.OracleBidOffset = &v
	}
	if c.OracleAskOffset != nil {
		v := *c.OracleAskOffset
		cfg.OracleAskOffset = &v
	}
	if c.AutoTopUp != nil {
		cfg.AutoTopUp = make(map[uint32]uint64, len(c.AutoTopUp))
		for assetID, v := range c.AutoTopUp {
			cfg.AutoTopUp[assetID] = v
		}
	}

	return &cfg
}

func updateLotSize(placements []*OrderPlacement, originalLotSize, newLotSize uint64) (updatedPlacements []*OrderPlacement) {
	var qtyCounter uint64
	for _, p := range placements {
		qtyCounter += p.Lots * originalLotSize
	}
	newPlacements := make([]*OrderPlacement, 0, len(placements))
	for _, p := range placements {
		lots := uint64(math.Round((float64(p.Lots) * float64(originalLotSize)) / float64(newLotSize)))
		lots = max(lots, 1)
		maxLots := qtyCounter / newLotSize
		lots = min(lots, maxLots)
		if lots == 0 {
			continue
		}
		qtyCounter -= lots * newLotSize
		newPlacements = append(newPlacements, &OrderPlacement{
			Lots:      lots,
			GapFactor: p.GapFactor,
		})
	}

	return newPlacements
}

// updateLotSize modifies the number of lots in each placement in the event
// of a lot size change. It will place as many lots as possible without
// exceeding the total quantity placed using the original lot size.
//
// This function is NOT thread safe.
func (c *BasicMarketMakingConfig) updateLotSize(originalLotSize, newLotSize uint64) {
	c.SellPlacements = updateLotSize(c.SellPlacements, originalLotSize, newLotSize)
	c.BuyPlacements = updateLotSize(c.BuyPlacements, originalLotSize, newLotSize)
}

type basicMMCalculator interface {
	basisPrice() (bp uint64, err error)
	halfSpread(uint64) (uint64, error)
	feeGapStats(uint64) (*FeeGapStats, error)
}

type basicMMCalculatorImpl struct {
	*market
	oracle oracle
	core   botCoreAdaptor
	cfg    *BasicMarketMakingConfig
	log    dex.Logger
}

var errNoBasisPrice = errors.New("no oracle or fiat rate available")
var errOracleFiatMismatch = errors.New("oracle rate and fiat rate mismatch")

// basisPrice calculates the basis price for the market maker.
// The mid-gap of the dex order book is used, and if oracles are
// available, and the oracle weighting is > 0, the oracle price
// is used to adjust the basis price.
// If the dex market is empty, but there are oracles available and
// oracle weighting is > 0, the oracle rate is used.
// If the dex market is empty and there are either no oracles available
// or oracle weighting is 0, the fiat rate is used.
// If there is no fiat rate available, the empty market rate in the
// configuration is used.
func (b *basicMMCalculatorImpl) basisPrice() (uint64, error) {
	oracleRate := b.msgRate(b.oracle.getMarketPrice(b.dexBaseID, b.dexQuoteID))
	b.log.Tracef("oracle rate = %s", b.fmtRate(oracleRate))

	rateFromFiat := b.core.ExchangeRateFromFiatSources()
	rateStep := b.rateStep.Load()
	if rateFromFiat == 0 {
		b.log.Meter("basisPrice_nofiat_"+b.market.name, time.Hour).Warn(
			"No fiat-based rate estimate(s) available for sanity check for %s", b.market.name,
		)
		if oracleRate == 0 { // steppedRate(0, x) => x, so we have to handle this.
			return 0, errNoBasisPrice
		}
		return steppedRate(oracleRate, rateStep), nil
	}
	if oracleRate == 0 {
		b.log.Meter("basisPrice_nooracle_"+b.market.name, time.Hour).Infof(
			"No oracle rate available. Using fiat-derived basis rate = %s for %s", b.fmtRate(rateFromFiat), b.market.name,
		)
		return steppedRate(rateFromFiat, rateStep), nil
	}
	mismatch := math.Abs((float64(oracleRate) - float64(rateFromFiat)) / float64(oracleRate))
	const maxOracleFiatMismatch = 0.05
	if mismatch > maxOracleFiatMismatch {
		b.log.Meter("basisPrice_sanity_fail+"+b.market.name, time.Minute*20).Warnf(
			"Oracle rate sanity check failed for %s. oracle rate = %s, rate from fiat = %s",
			b.market.name, b.market.fmtRate(oracleRate), b.market.fmtRate(rateFromFiat),
		)
		return 0, errOracleFiatMismatch
	}

	return steppedRate(oracleRate, rateStep), nil
}

// halfSpread calculates the distance from the mid-gap where if you sell a lot
// at the basis price plus half-gap, then buy a lot at the basis price minus
// half-gap, you will have one lot of the base asset plus the total fees in
// base units. Since the fees are in base units, basis price can be used to
// convert the quote fees to base units. In the case of tokens, the fees are
// converted using fiat rates.
func (b *basicMMCalculatorImpl) halfSpread(basisPrice uint64) (uint64, error) {
	feeStats, err := b.feeGapStats(basisPrice)
	if err != nil {
		return 0, err
	}
	return feeStats.FeeGap / 2, nil
}

// FeeGapStats is info about market and fee state. The interpretation of the
// various statistics may vary slightly with bot type.
type FeeGapStats struct {
	BasisPrice    uint64 `json:"basisPrice"`
	RemoteGap     uint64 `json:"remoteGap"`
	FeeGap        uint64 `json:"feeGap"`
	RoundTripFees uint64 `json:"roundTripFees"` // base units
}

func (b *basicMMCalculatorImpl) feeGapStats(basisPrice uint64) (*FeeGapStats, error) {
	if basisPrice == 0 { // prevent divide by zero later
		return nil, fmt.Errorf("basis price cannot be zero")
	}

	sellFeesInBaseUnits, err := b.core.OrderFeesInUnits(true, true, basisPrice)
	if err != nil {
		return nil, fmt.Errorf("error getting sell fees in base units: %w", err)
	}

	buyFeesInBaseUnits, err := b.core.OrderFeesInUnits(false, true, basisPrice)
	if err != nil {
		return nil, fmt.Errorf("error getting buy fees in base units: %w", err)
	}

	/*
	 * g = half-gap
	 * r = basis price (atomic ratio)
	 * l = lot size
	 * f = total fees in base units
	 *
	 * We must choose a half-gap such that:
	 * (r + g) * l / (r - g) = l + f
	 *
	 * This means that when you sell a lot at the basis price plus half-gap,
	 * then buy a lot at the basis price minus half-gap, you will have one
	 * lot of the base asset plus the total fees in base units.
	 *
	 * Solving for g, you get:
	 * g = f * r / (f + 2l)
	 */

	f := sellFeesInBaseUnits + buyFeesInBaseUnits
	l := b.lotSize.Load()

	r := float64(basisPrice) / calc.RateEncodingFactor
	g := float64(f) * r / float64(f+2*l)

	halfGap := uint64(math.Round(g * calc.RateEncodingFactor))

	if b.log.Level() == dex.LevelTrace {
		b.log.Tracef("halfSpread: basis price = %s, lot size = %s, aggregate fees = %s, half-gap = %s, sell fees = %s, buy fees = %s",
			b.fmtRate(basisPrice), b.fmtBase(l), b.fmtBaseFees(f), b.fmtRate(halfGap),
			b.fmtBaseFees(sellFeesInBaseUnits), b.fmtBaseFees(buyFeesInBaseUnits))
	}

	return &FeeGapStats{
		BasisPrice:    basisPrice,
		FeeGap:        halfGap * 2,
		RoundTripFees: f,
	}, nil
}

// mmBook is the DEX order book surface the basic MM needs for book protection.
type mmBook interface {
	BestNOrders(n int, sell bool) ([]*orderbook.Order, bool, error)
	RecentMatches() []*orderbook.MatchSummary
}

type basicMarketMaker struct {
	*unifiedExchangeAdaptor
	core             botCoreAdaptor
	oracle           oracle
	rebalanceRunning atomic.Bool
	calculator       basicMMCalculator
	book             mmBook
	nowFn            func() time.Time

	anchorMtx          sync.Mutex
	sellAnchor         uint64    // high-water aggressive foreign bid (msg rate)
	sellFadeStart      time.Time // zero while that bid is still live
	prevSellBookedLots uint64
	buyAnchor          uint64 // low-water aggressive foreign ask
	buyFadeStart       time.Time
	prevBuyBookedLots  uint64

	// Inventory skew persistence (PersistInventorySkew).
	skewStatePath      string
	skewCarry          atomic.Int64 // base deviation restored from the last run
	skewRestoreTried   atomic.Bool
	skewSaveMtx        sync.Mutex
	lastSavedDeviation int64
	lastSkewSave       time.Time
	lastSkewFail       time.Time
	skewSaveFails      int // failed save attempts (logged), for tests

	// Ladder center damping (OracleHalfLifeMinutes, OracleMaxStep).
	dampMtx    sync.Mutex
	dampedRate float64 // msg-rate, 0 until the first basis price
	dampLast   time.Time

	topUpMtx       sync.Mutex
	topUpAdded     map[uint32]uint64 // total auto-added this run
	topUpLast      map[uint32]time.Time
	topUpInFlight  map[uint32]bool
	topUpCapLogged map[uint32]bool
}

var _ bot = (*basicMarketMaker)(nil)

func (m *basicMarketMaker) cfg() *BasicMarketMakingConfig {
	return m.botCfg().BasicMMConfig
}

func (m *basicMarketMaker) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

func (m *basicMarketMaker) orderPrice(basisPrice, feeAdj uint64, sell bool, gapFactor float64) uint64 {
	var adj uint64

	// Apply the base strategy.
	switch m.cfg().GapStrategy {
	case GapStrategyMultiplier:
		adj = uint64(math.Round(float64(feeAdj) * gapFactor))
	case GapStrategyPercent, GapStrategyPercentPlus:
		adj = uint64(math.Round(gapFactor * float64(basisPrice)))
	case GapStrategyAbsolute, GapStrategyAbsolutePlus:
		adj = m.msgRate(gapFactor)
	}

	// Add the break-even to the "-plus" strategies
	switch m.cfg().GapStrategy {
	case GapStrategyAbsolutePlus, GapStrategyPercentPlus:
		adj += feeAdj
	}

	adj = steppedRate(adj, m.rateStep.Load())

	if sell {
		return basisPrice + adj
	}

	if basisPrice < adj {
		return 0
	}

	return basisPrice - adj
}

func (m *basicMarketMaker) targetBaseInventory() uint64 {
	var target int64
	if m.initialBalances != nil {
		target = int64(m.initialBalances[m.dexBaseID])
	}
	if m.inventoryMods != nil {
		target += m.inventoryMods[m.dexBaseID]
	}
	if carry := m.skewCarry.Load(); carry != 0 && m.persistSkewEnabled() {
		// The bot restarted with its allocation as its balance. Move the
		// target so the deviation from the last run is still there.
		target -= carry
		if target <= 0 {
			// Keep skew on (saturated) rather than switching it off.
			return 1
		}
	}
	if target < 0 {
		return 0
	}
	return uint64(target)
}

func (m *basicMarketMaker) currentBaseInventory() uint64 {
	bal := m.DEXBalance(m.dexBaseID)
	return bal.Available + bal.Locked + bal.Pending
}

func (m *basicMarketMaker) inventorySkewParams() (skew, cap float64) {
	skew, cap = defaultInventorySkew, defaultInventorySkewCap
	cfg := m.cfg()
	if cfg == nil {
		return
	}
	if cfg.InventorySkew != nil {
		skew = *cfg.InventorySkew
	}
	if cfg.InventorySkewCap != nil {
		cap = *cfg.InventorySkewCap
	}
	return
}

func (m *basicMarketMaker) doNotCrossEnabled() bool {
	cfg := m.cfg()
	if cfg == nil || cfg.DoNotCross == nil {
		return defaultDoNotCross
	}
	return *cfg.DoNotCross
}

func (m *basicMarketMaker) fadeDuration() time.Duration {
	h := defaultBidAnchorFadeHours
	cfg := m.cfg()
	if cfg != nil && cfg.BidAnchorFadeHours != nil {
		h = *cfg.BidAnchorFadeHours
	}
	if h <= 0 {
		return 0
	}
	return time.Duration(h * float64(time.Hour))
}

func (m *basicMarketMaker) oracleDampingParams() (halfLife time.Duration, maxStep float64, dampDrops bool) {
	mins := defaultOracleHalfLifeMinutes
	cfg := m.cfg()
	if cfg != nil {
		if cfg.OracleDampDrops != nil {
			dampDrops = *cfg.OracleDampDrops
		}
		if cfg.OracleHalfLifeMinutes != nil {
			mins = *cfg.OracleHalfLifeMinutes
		}
		if cfg.OracleMaxStep != nil {
			maxStep = *cfg.OracleMaxStep
		}
	}
	if mins > 0 {
		halfLife = time.Duration(mins * float64(time.Minute))
	}
	if maxStep < 0 {
		maxStep = 0
	}
	return
}

// dampedBasis returns the ladder center for a new basis (oracle) price. The
// center is an exponential moving average with the configured half-life,
// optionally limited to OracleMaxStep per update. Unless OracleDampDrops is
// set, a basis below the center is taken at once. The first price after a
// start is taken as is. With damping off, the basis is returned unchanged.
func (m *basicMarketMaker) dampedBasis(basisPrice uint64, now time.Time) uint64 {
	if basisPrice == 0 {
		return 0
	}
	halfLife, maxStep, dampDrops := m.oracleDampingParams()
	m.dampMtx.Lock()
	defer m.dampMtx.Unlock()
	raw := float64(basisPrice)
	prev := m.dampedRate
	if (halfLife <= 0 && maxStep <= 0) || prev <= 0 || (!dampDrops && raw <= prev) {
		m.dampedRate = raw
		m.dampLast = now
		return basisPrice
	}
	next := raw
	if halfLife > 0 {
		dt := now.Sub(m.dampLast)
		if dt < 0 {
			dt = 0
		}
		alpha := 1 - math.Exp2(-float64(dt)/float64(halfLife))
		next = prev + alpha*(raw-prev)
	}
	if maxStep > 0 {
		lim := prev * maxStep
		if next > prev+lim {
			next = prev + lim
		} else if next < prev-lim {
			next = prev - lim
		}
	}
	m.dampedRate = next
	m.dampLast = now
	out := uint64(math.Round(next))
	if out == 0 {
		out = 1
	}
	out = steppedRate(out, m.rateStep.Load())
	if out != basisPrice {
		m.log.Debugf("oracle damping: basis %s -> center %s (half-life %s, max step %.2f%%)",
			m.fmtRate(basisPrice), m.fmtRate(out), halfLife, maxStep*100)
	}
	return out
}

// oracleOffsets returns the bid and ask center offsets. A side without its
// own offset falls back to OracleOffset.
func (m *basicMarketMaker) oracleOffsets() (bid, ask float64) {
	return m.cfg().oracleOffsets()
}

// oracleOffsets returns the effective bid and ask center offsets. A side
// without its own offset falls back to OracleOffset.
func (cfg *BasicMarketMakingConfig) oracleOffsets() (bid, ask float64) {
	if cfg == nil {
		return 0, 0
	}
	if cfg.OracleOffset != nil {
		bid, ask = *cfg.OracleOffset, *cfg.OracleOffset
	}
	if cfg.OracleBidOffset != nil {
		bid = *cfg.OracleBidOffset
	}
	if cfg.OracleAskOffset != nil {
		ask = *cfg.OracleAskOffset
	}
	return
}

// offsetRate moves a rate by a fractional offset (0.02 = +2%).
func (m *basicMarketMaker) offsetRate(rate uint64, off float64) uint64 {
	if rate == 0 || off == 0 {
		return rate
	}
	out := uint64(math.Round(float64(rate) * (1 + off)))
	if out == 0 {
		out = 1
	}
	return steppedRate(out, m.rateStep.Load())
}

func placementQty(ps []*OrderPlacement, lotSize uint64) uint64 {
	var lots uint64
	for _, p := range ps {
		lots += p.Lots
	}
	return lots * lotSize
}

func (m *basicMarketMaker) quotedInventoryDenom(diff float64) uint64 {
	lotSize := m.lotSize.Load()
	if lotSize == 0 {
		return 0
	}
	cfg := m.cfg()
	var denom uint64
	if cfg != nil {
		if diff < 0 {
			denom = placementQty(cfg.SellPlacements, lotSize)
		} else if diff > 0 {
			denom = placementQty(cfg.BuyPlacements, lotSize)
		}
	}
	if denom == 0 {
		denom = lotSize
	}
	return denom
}

// applyInventorySkew slides the oracle basis toward selling or buying base
// depending on how far the bot's base inventory is from its allocated target.
// Deviation is scaled by lots currently quoted, not the warehouse allocation.
// Ghost matches with no swap do not change DEXBalance, so they do not skew.
func (m *basicMarketMaker) applyInventorySkew(basisPrice uint64) uint64 {
	if basisPrice == 0 {
		return 0
	}
	skew, cap := m.inventorySkewParams()
	if skew == 0 || cap == 0 {
		return basisPrice
	}
	target := m.targetBaseInventory()
	if target == 0 {
		return basisPrice
	}
	current := m.currentBaseInventory()
	diff := float64(current) - float64(target)
	if diff == 0 {
		return basisPrice
	}
	denom := m.quotedInventoryDenom(diff)
	if denom == 0 {
		return basisPrice
	}
	n := diff / float64(denom)
	shift := -n * skew * cap
	if shift > cap {
		shift = cap
	} else if shift < -cap {
		shift = -cap
	}
	skewed := uint64(math.Round(float64(basisPrice) * (1 + shift)))
	out := steppedRate(skewed, m.rateStep.Load())
	m.log.Debugf("inventory skew: target=%s current=%s denom=%s n=%.4f shift=%.2f%% basis=%s skewed=%s",
		m.fmtBase(target), m.fmtBase(current), m.fmtBase(denom), n, shift*100, m.fmtRate(basisPrice), m.fmtRate(out))
	return out
}

func lerpRate(from, to uint64, t float64) uint64 {
	if t <= 0 {
		return from
	}
	if t >= 1 {
		return to
	}
	return uint64(math.Round(float64(from) + (float64(to)-float64(from))*t))
}

func clampAnchor(anchor, invBasis uint64, sellSide bool) uint64 {
	if invBasis == 0 || anchor == 0 {
		return anchor
	}
	if sellSide {
		var maxA uint64
		if invBasis > math.MaxUint64/uint64(anchorCollarMult) {
			maxA = math.MaxUint64
		} else {
			maxA = uint64(float64(invBasis) * anchorCollarMult)
		}
		if anchor > maxA {
			return maxA
		}
		return anchor
	}
	minA := invBasis / uint64(anchorCollarMult)
	if minA == 0 {
		minA = 1
	}
	if anchor < minA {
		return minA
	}
	return anchor
}

func (m *basicMarketMaker) ownOrderIDs() map[order.OrderID]struct{} {
	ids := make(map[order.OrderID]struct{})
	if m.unifiedExchangeAdaptor == nil {
		return ids
	}
	m.balancesMtx.RLock()
	defer m.balancesMtx.RUnlock()
	for oid := range m.pendingDEXOrders {
		ids[oid] = struct{}{}
	}
	return ids
}

func (m *basicMarketMaker) bookedLots(sell bool) uint64 {
	if m.unifiedExchangeAdaptor == nil {
		return 0
	}
	lotSize := m.lotSize.Load()
	if lotSize == 0 {
		return 0
	}
	var lots uint64
	m.balancesMtx.RLock()
	defer m.balancesMtx.RUnlock()
	for _, po := range m.pendingDEXOrders {
		st := po.currentState()
		if st == nil || st.order == nil {
			continue
		}
		o := st.order
		if o.Sell != sell || o.Status > order.OrderStatusBooked {
			continue
		}
		if o.Qty > o.Filled {
			lots += (o.Qty - o.Filled) / lotSize
		}
	}
	return lots
}

// foreignBest returns the best booked rate on the requested side that is not
// one of our own orders. sell=true means asks.
func (m *basicMarketMaker) foreignBest(sell bool) (uint64, bool) {
	if m.book == nil {
		return 0, false
	}
	orders, _, err := m.book.BestNOrders(32, sell)
	if err != nil || len(orders) == 0 {
		return 0, false
	}
	own := m.ownOrderIDs()
	for _, o := range orders {
		if o == nil || o.Rate == 0 {
			continue
		}
		if _, mine := own[o.OrderID]; mine {
			continue
		}
		return o.Rate, true
	}
	return 0, false
}

func (m *basicMarketMaker) liftMatchRate(makerSell bool, threshold uint64, now time.Time) (uint64, bool) {
	if m.book == nil || threshold == 0 {
		return 0, false
	}
	matches := m.book.RecentMatches()
	if len(matches) == 0 {
		return 0, false
	}
	nowMS := now.UnixMilli()
	var best uint64
	found := false
	for i, ms := range matches {
		if i >= 8 {
			break
		}
		if ms == nil || ms.Rate == 0 {
			continue
		}
		if ms.Stamp != 0 && nowMS > int64(ms.Stamp) && time.Duration(nowMS-int64(ms.Stamp))*time.Millisecond > recentMatchWindow {
			continue
		}
		if makerSell {
			if ms.Rate >= threshold && ms.Rate >= best {
				best = ms.Rate
				found = true
			}
			continue
		}
		if ms.Rate <= threshold && (!found || ms.Rate < best) {
			best = ms.Rate
			found = true
		}
	}
	return best, found
}

// invAsk and invBid are the sell and buy ladder centers. Anchors are
// collared relative to the center of their own side.
func (m *basicMarketMaker) updateAnchors(now time.Time, invAsk, invBid, intendedAsk0, intendedBid0 uint64) {
	foreignBid, hasBid := m.foreignBest(false)
	foreignAsk, hasAsk := m.foreignBest(true)
	sellLots := m.bookedLots(true)
	buyLots := m.bookedLots(false)

	m.anchorMtx.Lock()
	defer m.anchorMtx.Unlock()

	protect := m.doNotCrossEnabled()
	aggressiveBid := protect && hasBid && intendedAsk0 > 0 && foreignBid >= intendedAsk0
	if aggressiveBid {
		clamped := clampAnchor(foreignBid, invAsk, true)
		if clamped > m.sellAnchor {
			m.sellAnchor = clamped
		}
		m.sellFadeStart = time.Time{}
	} else {
		if m.prevSellBookedLots > sellLots {
			if rate, ok := m.liftMatchRate(true, intendedAsk0, now); ok {
				clamped := clampAnchor(rate, invAsk, true)
				if clamped > m.sellAnchor {
					m.sellAnchor = clamped
				}
			}
		}
		if m.sellAnchor > 0 && m.sellFadeStart.IsZero() {
			m.sellFadeStart = now
		}
	}
	m.prevSellBookedLots = sellLots

	aggressiveAsk := protect && hasAsk && intendedBid0 > 0 && foreignAsk <= intendedBid0
	if aggressiveAsk {
		clamped := clampAnchor(foreignAsk, invBid, false)
		if m.buyAnchor == 0 || clamped < m.buyAnchor {
			m.buyAnchor = clamped
		}
		m.buyFadeStart = time.Time{}
	} else {
		if m.prevBuyBookedLots > buyLots {
			if rate, ok := m.liftMatchRate(false, intendedBid0, now); ok {
				clamped := clampAnchor(rate, invBid, false)
				if m.buyAnchor == 0 || clamped < m.buyAnchor {
					m.buyAnchor = clamped
				}
			}
		}
		if m.buyAnchor > 0 && m.buyFadeStart.IsZero() {
			m.buyFadeStart = now
		}
	}
	m.prevBuyBookedLots = buyLots

	d := m.fadeDuration()
	if m.sellAnchor > 0 && !m.sellFadeStart.IsZero() && (d <= 0 || !now.Before(m.sellFadeStart.Add(d))) {
		m.log.Debugf("book protection: sell anchor fade complete (was %s)", m.fmtRate(m.sellAnchor))
		m.sellAnchor = 0
		m.sellFadeStart = time.Time{}
	}
	if m.buyAnchor > 0 && !m.buyFadeStart.IsZero() && (d <= 0 || !now.Before(m.buyFadeStart.Add(d))) {
		m.log.Debugf("book protection: buy anchor fade complete (was %s)", m.fmtRate(m.buyAnchor))
		m.buyAnchor = 0
		m.buyFadeStart = time.Time{}
	}
}

func (m *basicMarketMaker) fadedRate(anchor uint64, fadeStart time.Time, now time.Time, invBasis uint64) uint64 {
	if anchor == 0 {
		return 0
	}
	if fadeStart.IsZero() {
		return anchor
	}
	d := m.fadeDuration()
	if d <= 0 {
		return 0
	}
	elapsed := now.Sub(fadeStart)
	if elapsed >= d {
		return 0
	}
	if elapsed < 0 {
		elapsed = 0
	}
	t := float64(elapsed) / float64(d)
	return steppedRate(lerpRate(anchor, invBasis, t), m.rateStep.Load())
}

func (m *basicMarketMaker) askFloor(now time.Time, invBasis uint64) uint64 {
	m.anchorMtx.Lock()
	defer m.anchorMtx.Unlock()
	return m.fadedRate(m.sellAnchor, m.sellFadeStart, now, invBasis)
}

func (m *basicMarketMaker) bidCeil(now time.Time, invBasis uint64) uint64 {
	m.anchorMtx.Lock()
	defer m.anchorMtx.Unlock()
	return m.fadedRate(m.buyAnchor, m.buyFadeStart, now, invBasis)
}

func (m *basicMarketMaker) applyDoNotCross(buys, sells []*TradePlacement) {
	step := m.rateStep.Load()
	if step == 0 {
		step = 1
	}
	// Only lift asks that would take a live bid. Do not re-ladder the rest
	// of the book (that walked every level up by one rateStep each epoch
	// and caused cancel/replace storms).
	if bid, ok := m.foreignBest(false); ok {
		minAsk := bid + step
		for _, p := range sells {
			if p.Rate != 0 && p.Rate <= bid {
				m.log.Debugf("do-not-cross: raising sell %s -> %s (foreign bid %s)",
					m.fmtRate(p.Rate), m.fmtRate(minAsk), m.fmtRate(bid))
				p.Rate = minAsk
			}
		}
	}
	if ask, ok := m.foreignBest(true); ok {
		if ask <= step {
			for _, p := range buys {
				p.Rate = 0
				p.Lots = 0
			}
			return
		}
		maxBid := ask - step
		for _, p := range buys {
			if p.Rate != 0 && p.Rate >= ask {
				m.log.Debugf("do-not-cross: lowering buy %s -> %s (foreign ask %s)",
					m.fmtRate(p.Rate), m.fmtRate(maxBid), m.fmtRate(ask))
				p.Rate = maxBid
			}
			if p.Rate == 0 {
				p.Lots = 0
			}
		}
	}
}

func (m *basicMarketMaker) ordersToPlace() (buyOrders, sellOrders []*TradePlacement, err error) {
	basisPrice, err := m.calculator.basisPrice()
	if err != nil {
		return nil, nil, err
	}
	now := m.now()
	center := m.dampedBasis(basisPrice, now)
	bidOff, askOff := m.oracleOffsets()
	invBid := m.applyInventorySkew(m.offsetRate(center, bidOff))
	invAsk := m.applyInventorySkew(m.offsetRate(center, askOff))
	// The fee gap is estimated at the middle of the two centers.
	invBasis := invBid
	if invAsk != invBid {
		invBasis = m.applyInventorySkew(m.offsetRate(center, (bidOff+askOff)/2))
	}
	if bidOff != 0 || askOff != 0 {
		m.log.Tracef("oracle offsets: center %s, bid %+.2f%% -> %s, ask %+.2f%% -> %s",
			m.fmtRate(center), bidOff*100, m.fmtRate(invBid), askOff*100, m.fmtRate(invAsk))
	}

	feeGap, err := m.calculator.feeGapStats(invBasis)
	if err != nil {
		return nil, nil, fmt.Errorf("error calculating fee gap stats: %w", err)
	}

	m.registerFeeGap(feeGap)
	var feeAdj uint64
	if needBreakEvenHalfSpread(m.cfg().GapStrategy) {
		feeAdj = feeGap.FeeGap / 2
	}

	if m.log.Level() == dex.LevelTrace {
		m.log.Tracef("ordersToPlace %s, basis price = %s, break-even fee adjustment = %s",
			m.name, m.fmtRate(invBasis), m.fmtRate(feeAdj))
	}

	cfg := m.cfg()
	var intendedAsk0, intendedBid0 uint64
	if len(cfg.SellPlacements) > 0 {
		intendedAsk0 = m.orderPrice(invAsk, feeAdj, true, cfg.SellPlacements[0].GapFactor)
	}
	if len(cfg.BuyPlacements) > 0 {
		intendedBid0 = m.orderPrice(invBid, feeAdj, false, cfg.BuyPlacements[0].GapFactor)
	}

	m.updateAnchors(now, invAsk, invBid, intendedAsk0, intendedBid0)

	// Asks may lift to the aggressive-bid floor. Bids stay on invBid
	// unless a dump (aggressive ask) pulls them down. Raising bids with a
	// pump would buy the pump.
	askBasis := invAsk
	if floor := m.askFloor(now, invAsk); floor > askBasis {
		askBasis = floor
		m.log.Debugf("book protection: ask basis %s (inv %s, floor %s)",
			m.fmtRate(askBasis), m.fmtRate(invAsk), m.fmtRate(floor))
	}
	bidBasis := invBid
	if ceil := m.bidCeil(now, invBid); ceil > 0 && ceil < bidBasis {
		bidBasis = ceil
		m.log.Debugf("book protection: bid basis %s (inv %s, ceil %s)",
			m.fmtRate(bidBasis), m.fmtRate(invBid), m.fmtRate(ceil))
	}
	// Never bid above the raw (undamped) oracle moved by the bid offset.
	maxBid := m.offsetRate(basisPrice, bidOff)
	if bidBasis > maxBid {
		m.log.Debugf("bid cap: bid basis %s -> %s (raw oracle %s, bid offset %+.2f%%)",
			m.fmtRate(bidBasis), m.fmtRate(maxBid), m.fmtRate(basisPrice), bidOff*100)
		bidBasis = maxBid
	}

	orders := func(orderPlacements []*OrderPlacement, sell bool, basis uint64) []*TradePlacement {
		placements := make([]*TradePlacement, 0, len(orderPlacements))
		for i, p := range orderPlacements {
			rate := m.orderPrice(basis, feeAdj, sell, p.GapFactor)

			if m.log.Level() == dex.LevelTrace {
				m.log.Tracef("ordersToPlace.orders: %s placement # %d, gap factor = %f, rate = %s, %+v",
					sellStr(sell), i, p.GapFactor, m.fmtRate(rate), rate)
			}

			lots := p.Lots
			if rate == 0 {
				lots = 0
			}
			placements = append(placements, &TradePlacement{
				Rate: rate,
				Lots: lots,
			})
		}
		return placements
	}

	// Never sell below the raw (undamped) oracle moved by the ask offset.
	minAsk := m.offsetRate(basisPrice, askOff)
	if askBasis < minAsk {
		m.log.Debugf("ask floor: ask basis %s -> %s (raw oracle %s, ask offset %+.2f%%)",
			m.fmtRate(askBasis), m.fmtRate(minAsk), m.fmtRate(basisPrice), askOff*100)
		askBasis = minAsk
	}

	buyOrders = orders(cfg.BuyPlacements, false, bidBasis)
	for _, p := range buyOrders {
		if p.Rate > maxBid {
			p.Rate = maxBid
		}
	}
	sellOrders = orders(cfg.SellPlacements, true, askBasis)
	for _, p := range sellOrders {
		if p.Rate != 0 && p.Rate < minAsk {
			p.Rate = minAsk
		}
	}
	if m.doNotCrossEnabled() {
		m.applyDoNotCross(buyOrders, sellOrders)
	}
	return buyOrders, sellOrders, nil
}

func (m *basicMarketMaker) rebalance(newEpoch uint64) {
	if !m.rebalanceRunning.CompareAndSwap(false, true) {
		return
	}
	defer m.rebalanceRunning.Store(false)

	m.log.Tracef("rebalance: epoch %d", newEpoch)

	if !m.checkBotHealth(newEpoch) {
		m.tryCancelOrders(m.ctx, &newEpoch, false)
		return
	}

	var buysReport, sellsReport *OrderReport
	buyOrders, sellOrders, determinePlacementsErr := m.ordersToPlace()
	if determinePlacementsErr != nil {
		m.tryCancelOrders(m.ctx, &newEpoch, false)
	} else {
		// FundMultiOrder will happily fund an entire side (many sells,
		// zero buys) before the other side is attempted. Place at most
		// one new order per call and interleave buy, sell, buy, sell so
		// both sides are on the book in the first epoch.
		placeOne := func(orders []*TradePlacement, sell bool) *OrderReport {
			_, rep := m.multiTradeN(orders, sell, m.cfg().DriftTolerance, newEpoch, 1)
			return rep
		}
		orderedLots := func(rep *OrderReport) uint64 {
			if rep == nil {
				return 0
			}
			var n uint64
			for _, p := range rep.Placements {
				n += p.OrderedLots
			}
			return n
		}
		const pairsPerEpoch = 2
		for pass := 0; pass < pairsPerEpoch; pass++ {
			m.log.Debugf("rebalance: interleaved place buy then sell (pass %d/%d, 1 new order per side)", pass+1, pairsPerEpoch)
			buyRep := placeOne(buyOrders, false)
			sellRep := placeOne(sellOrders, true)
			buysReport = mergeOrderReport(buysReport, buyRep)
			sellsReport = mergeOrderReport(sellsReport, sellRep)
			if orderedLots(buyRep) == 0 && orderedLots(sellRep) == 0 {
				break
			}
		}
	}

	m.maybeAutoTopUp(buysReport)
	m.maybeAutoTopUp(sellsReport)
	m.maybeSaveSkewState(false)

	epochReport := &EpochReport{
		BuysReport:  buysReport,
		SellsReport: sellsReport,
		EpochNum:    newEpoch,
	}
	epochReport.setPreOrderProblems(determinePlacementsErr)
	m.updateEpochReport(epochReport)
}

// maybeAutoTopUp requests an auto top-up for each asset the report marks
// as insufficient, if the config opts in for that asset.
func (m *basicMarketMaker) maybeAutoTopUp(rep *OrderReport) {
	caps := m.cfg().AutoTopUp
	if len(caps) == 0 || rep == nil || rep.Error == nil {
		return
	}
	for assetID, short := range rep.Error.InsufficientBalance {
		capAmt := caps[assetID]
		if !short || capAmt == 0 {
			continue
		}
		var have uint64
		if b := rep.AvailableDEXBals[assetID]; b != nil {
			have = b.Available
		}
		need := rep.RequiredDEXBals[assetID]
		if need <= have {
			continue
		}
		m.requestTopUp(assetID, need-have, capAmt)
	}
}

// requestTopUp moves up to want atoms of assetID from the wallet's
// unallocated balance into the bot's allocation, never exceeding capAmt in
// total for this run. It runs asynchronously: internalTransfer takes the
// MarketMaker's startUpdateMtx, which a config update can hold while it
// waits for the bot loop (and so this rebalance) to stop.
func (m *basicMarketMaker) requestTopUp(assetID uint32, want, capAmt uint64) {
	if m.internalTransfer == nil {
		return
	}
	m.topUpMtx.Lock()
	if m.topUpAdded == nil {
		m.topUpAdded = make(map[uint32]uint64)
		m.topUpLast = make(map[uint32]time.Time)
		m.topUpInFlight = make(map[uint32]bool)
		m.topUpCapLogged = make(map[uint32]bool)
	}
	if m.topUpInFlight[assetID] || time.Since(m.topUpLast[assetID]) < autoTopUpInterval {
		m.topUpMtx.Unlock()
		return
	}
	added := m.topUpAdded[assetID]
	if added >= capAmt {
		if !m.topUpCapLogged[assetID] {
			m.topUpCapLogged[assetID] = true
			m.log.Warnf("auto top-up: %s cap of %s reached, not adding more", dex.BipIDSymbol(assetID), fmtAssetAmt(assetID, capAmt))
		}
		m.topUpMtx.Unlock()
		return
	}
	if want > capAmt-added {
		want = capAmt - added
	}
	m.topUpInFlight[assetID] = true
	m.topUpLast[assetID] = time.Now()
	m.topUpMtx.Unlock()

	go func() {
		var moved uint64
		defer func() {
			m.topUpMtx.Lock()
			m.topUpInFlight[assetID] = false
			m.topUpAdded[assetID] += moved
			total := m.topUpAdded[assetID]
			m.topUpMtx.Unlock()
			if moved > 0 {
				m.log.Infof("auto top-up: allocated %s more to the bot (%s of %s cap used this run)",
					fmtAssetAmt(assetID, moved), fmtAssetAmt(assetID, total), fmtAssetAmt(assetID, capAmt))
			}
		}()
		if m.ctx == nil || m.ctx.Err() != nil {
			return
		}
		err := m.internalTransfer(m.mwh, func(dexAvail, _ map[uint32]uint64) error {
			amt := min(want, dexAvail[assetID])
			if amt == 0 {
				return fmt.Errorf("no unallocated %s in the wallet", dex.BipIDSymbol(assetID))
			}
			m.updateInventory(&BotInventoryDiffs{DEX: map[uint32]int64{assetID: int64(amt)}})
			moved = amt
			return nil
		})
		if err != nil {
			m.log.Warnf("auto top-up of %s failed: %v", dex.BipIDSymbol(assetID), err)
		}
	}()
}

// mergeOrderReport folds next into acc. OrderedLots and used balances
// accumulate; remaining balances and standing lots come from next.
func mergeOrderReport(acc, next *OrderReport) *OrderReport {
	if next == nil {
		return acc
	}
	if acc == nil {
		return next
	}
	n := len(acc.Placements)
	if len(next.Placements) < n {
		n = len(next.Placements)
	}
	for i := 0; i < n; i++ {
		next.Placements[i].OrderedLots += acc.Placements[i].OrderedLots
		if next.Placements[i].Error == nil {
			next.Placements[i].Error = acc.Placements[i].Error
		}
	}
	if next.UsedDEXBals == nil {
		next.UsedDEXBals = make(map[uint32]uint64)
	}
	for assetID, v := range acc.UsedDEXBals {
		next.UsedDEXBals[assetID] += v
	}
	next.UsedCEXBal += acc.UsedCEXBal
	if next.Error == nil {
		next.Error = acc.Error
	}
	return next
}

func (m *basicMarketMaker) botLoop(ctx context.Context) (*sync.WaitGroup, error) {
	ob, bookFeed, err := m.core.SyncBook(m.host, m.dexBaseID, m.dexQuoteID)
	if err != nil {
		return nil, fmt.Errorf("failed to sync book: %v", err)
	}
	m.book = ob

	m.calculator = &basicMMCalculatorImpl{
		market: m.market,
		oracle: m.oracle,
		core:   m.core,
		cfg:    m.cfg(),
		log:    m.log,
	}

	m.restoreSkewState()

	// Process book updates. Rebalance runs in its own goroutine so a
	// slow FundMultiOrder cannot block the feed (bookie then drops
	// updates: "feed N is blocking and book update was thrown away").
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer bookFeed.Close()
		var rbWg sync.WaitGroup
		defer m.maybeSaveSkewState(true)
		defer rbWg.Wait()
		for {
			select {
			case ni, ok := <-bookFeed.Next():
				if !ok {
					m.log.Error("Stopping bot due to nil book feed.")
					m.kill()
					return
				}
				switch epoch := ni.Payload.(type) {
				case *core.ResolvedEpoch:
					ep := epoch.Current
					rbWg.Add(1)
					go func() {
						defer rbWg.Done()
						m.rebalance(ep)
					}()
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return &wg, nil
}

// RunBasicMarketMaker starts a basic market maker bot.
func newBasicMarketMaker(cfg *BotConfig, adaptorCfg *exchangeAdaptorCfg, oracle oracle, log dex.Logger) (*basicMarketMaker, error) {
	if cfg.BasicMMConfig == nil {
		// implies bug in caller
		return nil, errors.New("no market making config provided")
	}

	adaptor, err := newUnifiedExchangeAdaptor(adaptorCfg)
	if err != nil {
		return nil, fmt.Errorf("error constructing exchange adaptor: %w", err)
	}

	err = cfg.BasicMMConfig.validate()
	if err != nil {
		return nil, fmt.Errorf("invalid market making config: %v", err)
	}

	for assetID := range cfg.BasicMMConfig.AutoTopUp {
		if assetID != cfg.BaseID && assetID != cfg.QuoteID &&
			assetID != feeAssetID(cfg.BaseID) && assetID != feeAssetID(cfg.QuoteID) {
			return nil, fmt.Errorf("auto top-up configured for asset %d, which this market does not use", assetID)
		}
	}

	basicMM := &basicMarketMaker{
		unifiedExchangeAdaptor: adaptor,
		core:                   adaptor,
		oracle:                 oracle,
		skewStatePath:          skewStatePathForDir(adaptorCfg.mmStateDir),
	}
	adaptor.setBotLoop(basicMM.botLoop)
	return basicMM, nil
}
