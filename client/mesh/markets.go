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
	{Name: "dcr_btc", LotSize: 1e8, TestRate: 1_000_000},
	{Name: "lbc_btc", LotSize: 1e8, TestRate: 10_000},
	{Name: "lbc_dcr", LotSize: 1e8, TestRate: 1_000_000},
	// USDC on Polygon (6 decimals): 20 USDC per DCR; LBC-USDC lot 5,000 LBC at
	// 0.01 USDC per LBC (PLAN.md lot sizes).
	{Name: "dcr_usdc.polygon", LotSize: 1e8, TestRate: 20_000_000},
	{Name: "lbc_usdc.polygon", LotSize: 5_000e8, TestRate: 10_000},
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
