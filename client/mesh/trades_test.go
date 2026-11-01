// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"path/filepath"
	"testing"
	"time"
)

func testMatch(t *testing.T, maker, taker *OrderBooks, rate, qty uint64, n byte) *Match {
	t.Helper()
	m := &Match{Market: dcrBTC, Maker: maker.self.String(), Taker: taker.self.String(), Rate: rate, Qty: qty}
	m.ID[0], m.ID[1] = n, 0xab
	return m
}

func TestTradeSummarySignValidate(t *testing.T) {
	m := newFakeMesh()
	a, b, c := newTestBooks(t, m, nil), newTestBooks(t, m, nil), newTestBooks(t, m, nil)
	now := time.Now()
	mt := testMatch(t, a, b, 1e6, 3e8, 1)
	for _, ob := range []*OrderBooks{a, b} {
		ts, err := ob.signTrade(mt, now.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if ts.Signer() != ob.self.String() {
			t.Fatal("wrong signer")
		}
		if err := ts.Validate(now); err != nil {
			t.Fatalf("valid summary rejected: %v", err)
		}
		// Fits a mesh publish with the largest values.
		big := *ts
		big.Rate, big.Qty, big.Stamp = maxTradeValue, maxTradeValue, 1<<62
		enc, _ := encodeBookMsg(&bookMsg{Type: msgTypeTrade, Trade: &big})
		if n := len(enc) + len("orderbook.lbc_dcr"); n > 1024-200 {
			t.Fatalf("trade message is %d bytes", n)
		}
	}
	if _, err := c.signTrade(mt, now.UnixMilli()); err == nil {
		t.Fatal("non-participant signed")
	}
	good, _ := a.signTrade(mt, now.UnixMilli())
	for name, mod := range map[string]func(ts *TradeSummary){
		"rate":     func(ts *TradeSummary) { ts.Rate++ },
		"qty":      func(ts *TradeSummary) { ts.Qty = 0 },
		"huge":     func(ts *TradeSummary) { ts.Rate = maxTradeValue + 1 },
		"future":   func(ts *TradeSummary) { ts.Stamp = now.Add(time.Hour).UnixMilli() },
		"old":      func(ts *TradeSummary) { ts.Stamp = now.Add(-TradeRetention - time.Hour).UnixMilli() },
		"byTaker":  func(ts *TradeSummary) { ts.ByTaker = true },
		"taker":    func(ts *TradeSummary) { ts.Taker = c.self.String() },
		"self":     func(ts *TradeSummary) { ts.Taker = ts.Maker },
		"market":   func(ts *TradeSummary) { ts.Market = Market{Base: 42, Quote: 42} },
		"nosig":    func(ts *TradeSummary) { ts.Sig = nil },
		"matchID":  func(ts *TradeSummary) { ts.MatchID[5] ^= 1 },
		"badMaker": func(ts *TradeSummary) { ts.Maker = "nope" },
	} {
		ts := *good
		mod(&ts)
		if err := ts.Validate(now); err == nil {
			t.Fatalf("%s: tampered summary accepted", name)
		}
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTradeHistoryGossipAndBackfill(t *testing.T) {
	m := newFakeMesh()
	a, b := newTestBooks(t, m, nil), newTestBooks(t, m, nil)
	dir := t.TempDir()
	if _, err := a.SetTradeStore(filepath.Join(dir, "a.json")); err != nil {
		t.Fatal(err)
	}
	for _, ob := range []*OrderBooks{a, b} {
		if err := ob.SubscribeMarket(tCtx, dcrBTC); err != nil {
			t.Fatal(err)
		}
	}
	mt := testMatch(t, a, b, 1e6, 2e8, 1)
	a.publishTrade(mt)
	waitFor(t, "b to see the trade", func() bool { return len(b.Trades(dcrBTC, 0)) == 1 })
	// The other side's summary of the same match is a duplicate.
	b.publishTrade(mt)
	time.Sleep(100 * time.Millisecond)
	if n := len(a.Trades(dcrBTC, 0)); n != 1 {
		t.Fatalf("a has %d trades", n)
	}
	a.publishTrade(testMatch(t, a, b, 1.1e6, 1e8, 2))
	waitFor(t, "b to see trade 2", func() bool { return len(b.Trades(dcrBTC, 0)) == 2 })

	// A late joiner asks for history and gets both trades.
	c := newTestBooks(t, m, nil)
	if err := c.SubscribeMarket(tCtx, dcrBTC); err != nil {
		t.Fatal(err)
	}
	c.requestHistory(tCtx, dcrBTC)
	waitFor(t, "c backfill", func() bool { return len(c.Trades(dcrBTC, 0)) == 2 })

	// a's history survives a restart.
	a2 := newTestBooks(t, m, nil)
	n, err := a2.SetTradeStore(filepath.Join(dir, "a.json"))
	if err != nil || n != 2 {
		t.Fatalf("loaded %d trades, err %v", n, err)
	}
}

func TestBuildCandlesAndSpot(t *testing.T) {
	m := newFakeMesh()
	a, b := newTestBooks(t, m, nil), newTestBooks(t, m, nil)
	now := time.Now().Truncate(time.Hour)
	mk := func(n byte, at time.Time, rate, qty uint64) *TradeSummary {
		ts, err := a.signTrade(testMatch(t, a, b, rate, qty, n), at.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	trades := []*TradeSummary{
		mk(1, now.Add(-25*time.Hour), 9e5, 1e8), // outside 24h
		mk(2, now.Add(-2*time.Hour), 1e6, 1e8),
		mk(3, now.Add(-2*time.Hour+time.Minute), 1.2e6, 2e8),
		mk(4, now.Add(-time.Hour), 1.1e6, 1e8),
	}
	c5 := BuildCandles(trades, 5*time.Minute).CandlesCopy()
	if len(c5) != 3 {
		t.Fatalf("%d 5m candles", len(c5))
	}
	c := c5[1]
	if c.StartRate != 1e6 || c.EndRate != 1.2e6 || c.HighRate != 1.2e6 || c.LowRate != 1e6 || c.MatchVolume != 3e8 {
		t.Fatalf("bad candle %+v", c)
	}
	if c.QuoteVolume != 1e6*1e8/1e8+1.2e6*2e8/1e8 {
		t.Fatalf("bad quote volume %d", c.QuoteVolume)
	}
	if c.StartStamp%uint64((5*time.Minute).Milliseconds()) != 0 {
		t.Fatal("candle not aligned")
	}
	if n := len(BuildCandles(trades, 24*time.Hour).CandlesCopy()); n < 1 || n > 2 {
		t.Fatalf("%d 24h candles", n)
	}
	spot := MarketSpot(dcrBTC, trades, now)
	if spot.Rate != 1.1e6 || spot.Vol24 != 4e8 || spot.High24 != 1.2e6 || spot.Low24 != 1e6 {
		t.Fatalf("bad spot %+v", spot)
	}
	if spot.Change24 < 0.0999 || spot.Change24 > 0.1001 {
		t.Fatalf("bad change %v", spot.Change24)
	}
	if MarketSpot(dcrBTC, nil, now) != nil {
		t.Fatal("spot without trades")
	}
}
