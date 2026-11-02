// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// MarketDefaults are the user interface defaults for a mesh market: the lot
// size in base atoms and a suggested rate (message-rate encoding) for
// simnet test chains.
type MarketDefaults struct {
	Name     string `json:"name"`
	LotSize  uint64 `json:"lotSize"`
	TestRate uint64 `json:"testRate"`
}

// DefaultMarkets are the markets a mesh client subscribes to on connect.
var DefaultMarkets = []MarketDefaults{
	// Lot sizes per PLAN.md release-1 decisions (LBC-BTC 100,000 LBC,
	// LBC-DCR 1,000 LBC, LBC-USDC 5,000 LBC). Test rates are real-world
	// prices of 2026-11-02 (CoinGecko: DCR $12.95, BTC $76,883,
	// LBC $0.000504, USDC $0.9995). LBC-BTC is below the rate resolution
	// (1 sat per LBC = $0.00077); 1 is the smallest representable rate.
	{Name: "dcr_btc", LotSize: 1e8, TestRate: 16_844},
	{Name: "lbc_btc", LotSize: 100_000e8, TestRate: 1},
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
