// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"testing"

	"decred.org/dcrdex/dex/candles"
	"decred.org/dcrdex/dex/msgjson"
)

func TestClientCacheSize(t *testing.T) {
	for _, tt := range []struct {
		name  string
		sizes map[string]int
		bin   string
		want  int
	}{
		{"server without sizes", nil, "5m", candles.CacheSize},
		{"bin not advertised", map[string]int{"1h": 3000}, "5m", candles.CacheSize},
		{"advertised", map[string]int{"5m": 3000}, "5m", 3000},
		{"advertised smaller", map[string]int{"5m": 500}, "5m", 500},
		{"zero", map[string]int{"5m": 0}, "5m", candles.CacheSize},
		{"negative", map[string]int{"5m": -5}, "5m", candles.CacheSize},
		{"too large", map[string]int{"5m": 1e9}, "5m", candles.MaxCacheSize},
	} {
		if got := clientCacheSize(tt.sizes, tt.bin); got != tt.want {
			t.Fatalf("%s: wanted %d, got %d", tt.name, tt.want, got)
		}
	}
}

// TestCandlesRequestSize checks that the bookie requests the server-advertised
// number of candles, and the legacy 1000 from servers that advertise nothing.
func TestCandlesRequestSize(t *testing.T) {
	rig := newTestRig()

	for _, tt := range []struct {
		sizes map[string]int
		want  int
	}{
		{nil, candles.CacheSize},
		{map[string]int{"5m": 3000}, 3000},
	} {
		booky := newBookie(rig.dc, tUTXOAssetA.ID, tUTXOAssetB.ID, []string{"5m"}, tt.sizes, tLogger)
		var gotReq msgjson.CandlesRequest
		rig.ws.queueResponse(msgjson.CandlesRoute, func(msg *msgjson.Message, f msgFunc) error {
			if err := msg.Unmarshal(&gotReq); err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}
			resp, _ := msgjson.NewResponse(msg.ID, &msgjson.WireCandles{}, nil)
			f(resp)
			return nil
		})
		if err := booky.candles("5m", 0); err != nil {
			t.Fatalf("candles error: %v", err)
		}
		if gotReq.NumCandles != tt.want {
			t.Fatalf("wanted request for %d candles, got %d", tt.want, gotReq.NumCandles)
		}
		if booky.candleCaches["5m"].Cap() != tt.want {
			t.Fatalf("wrong cache size %d", booky.candleCaches["5m"].Cap())
		}
	}
}
