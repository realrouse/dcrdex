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
}

const (
	defaultInventorySkew      = 1.0
	defaultInventorySkewCap   = 0.03
	defaultDoNotCross         = true
	defaultBidAnchorFadeHours = 4.0
	maxBidAnchorFadeHours     = 24.0
	anchorCollarMult          = 2.0
	recentMatchWindow         = 30 * time.Second
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
	if c.DoNotCross != nil {
		v := *c.DoNotCross
		cfg.DoNotCross = &v
	}
	if c.BidAnchorFadeHours != nil {
		v := *c.BidAnchorFadeHours
		cfg.BidAnchorFadeHours = &v
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

func (m *basicMarketMaker) updateAnchors(now time.Time, invBasis, intendedAsk0, intendedBid0 uint64) {
	foreignBid, hasBid := m.foreignBest(false)
	foreignAsk, hasAsk := m.foreignBest(true)
	sellLots := m.bookedLots(true)
	buyLots := m.bookedLots(false)

	m.anchorMtx.Lock()
	defer m.anchorMtx.Unlock()

	protect := m.doNotCrossEnabled()
	aggressiveBid := protect && hasBid && intendedAsk0 > 0 && foreignBid >= intendedAsk0
	if aggressiveBid {
		clamped := clampAnchor(foreignBid, invBasis, true)
		if clamped > m.sellAnchor {
			m.sellAnchor = clamped
		}
		m.sellFadeStart = time.Time{}
	} else {
		if m.prevSellBookedLots > sellLots {
			if rate, ok := m.liftMatchRate(true, intendedAsk0, now); ok {
				clamped := clampAnchor(rate, invBasis, true)
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
		clamped := clampAnchor(foreignAsk, invBasis, false)
		if m.buyAnchor == 0 || clamped < m.buyAnchor {
			m.buyAnchor = clamped
		}
		m.buyFadeStart = time.Time{}
	} else {
		if m.prevBuyBookedLots > buyLots {
			if rate, ok := m.liftMatchRate(false, intendedBid0, now); ok {
				clamped := clampAnchor(rate, invBasis, false)
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
	if bid, ok := m.foreignBest(false); ok {
		minAsk := bid + step
		for _, p := range sells {
			if p.Rate != 0 && p.Rate < minAsk {
				m.log.Debugf("do-not-cross: raising sell %s -> %s (foreign bid %s)",
					m.fmtRate(p.Rate), m.fmtRate(minAsk), m.fmtRate(bid))
				p.Rate = minAsk
			}
			if p.Rate >= minAsk {
				minAsk = p.Rate + step
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
			if p.Rate != 0 && p.Rate > maxBid {
				m.log.Debugf("do-not-cross: lowering buy %s -> %s (foreign ask %s)",
					m.fmtRate(p.Rate), m.fmtRate(maxBid), m.fmtRate(ask))
				p.Rate = maxBid
			}
			if p.Rate == 0 {
				p.Lots = 0
				continue
			}
			if p.Rate > step {
				maxBid = p.Rate - step
			} else {
				maxBid = 0
			}
		}
	}
}

func (m *basicMarketMaker) ordersToPlace() (buyOrders, sellOrders []*TradePlacement, err error) {
	basisPrice, err := m.calculator.basisPrice()
	if err != nil {
		return nil, nil, err
	}
	invBasis := m.applyInventorySkew(basisPrice)

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
	now := m.now()
	var intendedAsk0, intendedBid0 uint64
	if len(cfg.SellPlacements) > 0 {
		intendedAsk0 = m.orderPrice(invBasis, feeAdj, true, cfg.SellPlacements[0].GapFactor)
	}
	if len(cfg.BuyPlacements) > 0 {
		intendedBid0 = m.orderPrice(invBasis, feeAdj, false, cfg.BuyPlacements[0].GapFactor)
	}

	m.updateAnchors(now, invBasis, intendedAsk0, intendedBid0)

	// Asks may lift to the aggressive-bid floor. Bids stay on invBasis
	// unless a dump (aggressive ask) pulls them down. Raising bids with a
	// pump would buy the pump.
	askBasis := invBasis
	if floor := m.askFloor(now, invBasis); floor > askBasis {
		askBasis = floor
		m.log.Debugf("book protection: ask basis %s (inv %s, floor %s)",
			m.fmtRate(askBasis), m.fmtRate(invBasis), m.fmtRate(floor))
	}
	bidBasis := invBasis
	if ceil := m.bidCeil(now, invBasis); ceil > 0 && ceil < bidBasis {
		bidBasis = ceil
		m.log.Debugf("book protection: bid basis %s (inv %s, ceil %s)",
			m.fmtRate(bidBasis), m.fmtRate(invBasis), m.fmtRate(ceil))
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

	buyOrders = orders(cfg.BuyPlacements, false, bidBasis)
	sellOrders = orders(cfg.SellPlacements, true, askBasis)
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
		_, buysReport = m.multiTrade(buyOrders, false, m.cfg().DriftTolerance, newEpoch)
		_, sellsReport = m.multiTrade(sellOrders, true, m.cfg().DriftTolerance, newEpoch)
	}

	epochReport := &EpochReport{
		BuysReport:  buysReport,
		SellsReport: sellsReport,
		EpochNum:    newEpoch,
	}
	epochReport.setPreOrderProblems(determinePlacementsErr)
	m.updateEpochReport(epochReport)
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

	// Process book updates
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer bookFeed.Close()
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
					m.rebalance(epoch.Current)
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

	basicMM := &basicMarketMaker{
		unifiedExchangeAdaptor: adaptor,
		core:                   adaptor,
		oracle:                 oracle,
	}
	adaptor.setBotLoop(basicMM.botLoop)
	return basicMM, nil
}
