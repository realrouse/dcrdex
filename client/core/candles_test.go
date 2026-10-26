// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"errors"
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
		if gotReq.Before != 0 {
			t.Fatalf("live candle request should not set Before")
		}
		if booky.candleCaches["5m"].Cap() != tt.want {
			t.Fatalf("wrong cache size %d", booky.candleCaches["5m"].Cap())
		}
	}
}

func TestCandleHistory(t *testing.T) {
	rig := newTestRig()
	tCore := rig.core
	base, quote := tUTXOAssetA.ID, tUTXOAssetB.ID
	const before = 1_000_000

	setPageSize := func(sz int) {
		rig.dc.cfgMtx.Lock()
		rig.dc.cfg.CandlePageSize = sz
		rig.dc.cfgMtx.Unlock()
	}

	// A server that doesn't advertise candle history (e.g. upstream servers)
	// gets no request.
	setPageSize(0)
	if _, err := tCore.CandleHistory(tDexHost, base, quote, "1h", before, 10); !errors.Is(err, ErrCandleHistoryUnsupported) {
		t.Fatalf("wanted ErrCandleHistoryUnsupported, got %v", err)
	}
	if ex := tCore.exchangeInfo(rig.dc); ex.CandlePageSize != 0 {
		t.Fatalf("exchange shows candle page size %d for unsupported server", ex.CandlePageSize)
	}

	setPageSize(1000)
	if ex := tCore.exchangeInfo(rig.dc); ex.CandlePageSize != 1000 {
		t.Fatalf("wrong exchange candle page size %d", ex.CandlePageSize)
	}

	var gotReq msgjson.CandlesRequest
	queue := func(wc *msgjson.WireCandles) {
		rig.ws.queueResponse(msgjson.CandlesRoute, func(msg *msgjson.Message, f msgFunc) error {
			gotReq = msgjson.CandlesRequest{}
			if err := msg.Unmarshal(&gotReq); err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}
			resp, _ := msgjson.NewResponse(msg.ID, wc, nil)
			f(resp)
			return nil
		})
	}
	goodCandles := func() *msgjson.WireCandles {
		return &msgjson.WireCandles{
			StartStamps:  []uint64{100, 200},
			EndStamps:    []uint64{200, 300},
			MatchVolumes: []uint64{1, 2},
			QuoteVolumes: []uint64{1, 2},
			HighRates:    []uint64{3, 4},
			LowRates:     []uint64{1, 2},
			StartRates:   []uint64{1, 2},
			EndRates:     []uint64{3, 4},
		}
	}

	queue(goodCandles())
	cs, err := tCore.CandleHistory(tDexHost, base, quote, "1h", before, 10)
	if err != nil {
		t.Fatalf("CandleHistory error: %v", err)
	}
	if gotReq.Before != before || gotReq.NumCandles != 10 || gotReq.BinSize != "1h" || gotReq.BaseID != base || gotReq.QuoteID != quote {
		t.Fatalf("wrong request %+v", gotReq)
	}
	if len(cs) != 2 || cs[0].EndStamp != 200 || cs[1].EndStamp != 300 || cs[1].HighRate != 4 {
		t.Fatalf("wrong candles %+v", cs)
	}

	// n is limited to the page size, and 0 means a full page.
	for _, n := range []int{0, 5000} {
		queue(goodCandles())
		if _, err = tCore.CandleHistory(tDexHost, base, quote, "1h", before, n); err != nil {
			t.Fatalf("CandleHistory error: %v", err)
		}
		if gotReq.NumCandles != 1000 {
			t.Fatalf("n = %d: wanted request for 1000 candles, got %d", n, gotReq.NumCandles)
		}
	}

	// Bad inputs, no request sent.
	if _, err = tCore.CandleHistory(tDexHost, base, quote, "5m", before, 10); err == nil {
		t.Fatalf("no error for bin size the server doesn't have")
	}
	if _, err = tCore.CandleHistory(tDexHost, base, quote, "1h", 0, 10); err == nil {
		t.Fatalf("no error for zero before")
	}
	if _, err = tCore.CandleHistory("unknown.dex", base, quote, "1h", before, 10); err == nil {
		t.Fatalf("no error for unknown dex")
	}
	if _, err = tCore.CandleHistory(tDexHost, base, 12345, "1h", before, 10); err == nil {
		t.Fatalf("no error for unknown market")
	}

	// Bad server responses.
	for name, mod := range map[string]func(*msgjson.WireCandles){
		"not before": func(wc *msgjson.WireCandles) { wc.EndStamps[1] = before },
		"unordered":  func(wc *msgjson.WireCandles) { wc.EndStamps[0] = 300 },
		"malformed":  func(wc *msgjson.WireCandles) { wc.HighRates = wc.HighRates[:1] },
		"too many":   func(wc *msgjson.WireCandles) {},
	} {
		wc := goodCandles()
		mod(wc)
		queue(wc)
		n := 10
		if name == "too many" {
			n = 1
		}
		if _, err = tCore.CandleHistory(tDexHost, base, quote, "1h", before, n); err == nil {
			t.Fatalf("%s: no error", name)
		}
	}
}
