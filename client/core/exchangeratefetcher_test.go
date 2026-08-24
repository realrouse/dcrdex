package core

import "testing"

func TestMEXCUSDTPrices(t *testing.T) {
	got := mexcUSDTPrices([]mexcTicker{
		{Symbol: "LBCUSDT", Price: "0.002288"},
		{Symbol: "DCRUSDT", Price: "13.50"},
		{Symbol: "LBCBTC", Price: "0.000001"},
		{Symbol: "BADUSDT", Price: "nope"},
	})
	if got["LBC"] != 0.002288 {
		t.Fatalf("LBC: got %v", got["LBC"])
	}
	if got["DCR"] != 13.50 {
		t.Fatalf("DCR: got %v", got["DCR"])
	}
	if _, ok := got["LBCBTC"]; ok {
		t.Fatal("non-USDT pair should be ignored")
	}
	if _, ok := got["BAD"]; ok {
		t.Fatal("unparseable price should be ignored")
	}
}
