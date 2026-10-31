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
