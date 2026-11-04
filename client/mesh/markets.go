// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"fmt"
	"strings"
)

// MarketDefaults are the user interface defaults for a mesh market: the lot
// size in base atoms and a suggested rate (message-rate encoding) for
// simnet test chains.
type MarketDefaults struct {
	Name     string `json:"name"`
	LotSize  uint64 `json:"lotSize"`
	TestRate uint64 `json:"testRate"`
	// RateStep is the price step in message-rate units (quote atoms per
	// 1e8 base atoms). Order rates must be a whole, non-zero multiple of it,
	// so RateStep is also the minimum rate. Zero means 1.
	RateStep uint64 `json:"rateStep,omitempty"`
}

// DefaultMarkets are the markets a mesh client subscribes to on connect.
var DefaultMarkets = []MarketDefaults{
	// Lot sizes per PLAN.md release-1 decisions (LBC-BTC 100,000 LBC,
	// LBC-DCR 1,000 LBC, LBC-USDC 5,000 LBC). Test rates are real-world
	// prices of 2026-11-02 (CoinGecko: DCR $12.95, BTC $76,883,
	// LBC $0.000504, USDC $0.9995). LBC-BTC is below the rate resolution
	// (1 sat per LBC = $0.00077); 1 is the smallest representable rate.
	//
	// lbc_btc prices are whole satoshis per LBC: 1 sat is the minimum and
	// the step (the next price up is 2 sat). A satoshi isn't divisible, so
	// there is no sub-sat scale; while LBC trades below 1 sat it is in
	// practice a one-price market at 1 sat.
	{Name: "dcr_btc", LotSize: 1e8, TestRate: 16_844},
	{Name: "lbc_btc", LotSize: 100_000e8, TestRate: 1, RateStep: 1},
	{Name: "lbc_dcr", LotSize: 1_000e8, TestRate: 3_894},
	// USDC on Polygon has 6 decimals: rate = USDC atoms per base coin.
	{Name: "dcr_usdc.polygon", LotSize: 1e8, TestRate: 12_956_000},
	{Name: "lbc_usdc.polygon", LotSize: 5_000e8, TestRate: 504},
}

// MarketDefault returns the defaults for a market, if it has any.
func MarketDefault(name string) (MarketDefaults, bool) {
	for _, d := range DefaultMarkets {
		if d.Name == name {
			return d, true
		}
	}
	return MarketDefaults{}, false
}

// MarketRateStep is a market's price step in message-rate units (1 if the
// market has no defaults or sets none).
func MarketRateStep(name string) uint64 {
	if d, ok := MarketDefault(name); ok && d.RateStep > 0 {
		return d.RateStep
	}
	return 1
}

// CheckRate checks that rate is a valid price on the market: at least the
// rate step and a whole multiple of it.
func CheckRate(name string, rate uint64) error {
	step := MarketRateStep(name)
	if rate < step || rate%step != 0 {
		return fmt.Errorf("invalid rate %d on %s: prices must be whole multiples of the %s minimum price step",
			rate, name, rateStepText(name, step))
	}
	return nil
}

// rateStepText describes a rate step for errors, e.g. "1 sat per LBC".
func rateStepText(name string, step uint64) string {
	if base, ok := strings.CutSuffix(name, "_btc"); ok {
		return fmt.Sprintf("%d sat per %s", step, strings.ToUpper(base))
	}
	return fmt.Sprintf("%d (quote atoms per base coin)", step)
}
