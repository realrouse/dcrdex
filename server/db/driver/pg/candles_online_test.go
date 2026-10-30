//go:build pgonline

package pg

import (
	"testing"

	"decred.org/dcrdex/dex/candles"
)

func TestCandles(t *testing.T) {
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}

	var baseID, quoteID uint32 = 42, 0
	var candleDur uint64 = 5 * 60 * 1000

	lastCandle, err := archie.LastCandleEndStamp(baseID, quoteID, candleDur)
	if err != nil {
		t.Fatalf("Initial LastCandleEndStamp error: %v", err)
	}

	cands := []*candles.Candle{
		{EndStamp: candleDur},
		{EndStamp: candleDur * 2},
	}

	if err = archie.InsertCandles(baseID, quoteID, candleDur, cands); err != nil {
		t.Fatalf("InsertCandles error: %v", err)
	}

	lastCandle, err = archie.LastCandleEndStamp(baseID, quoteID, candleDur)
	if err != nil {
		t.Fatalf("LastCandleEndStamp error: %v", err)
	}

	if lastCandle != candleDur*2 {
		t.Fatalf("Wrong last candle. Wanted 2, got %d", lastCandle)
	}

	// Updating is fine
	cands[1].MatchVolume = 1
	if err = archie.InsertCandles(baseID, quoteID, candleDur, []*candles.Candle{cands[1]}); err != nil {
		t.Fatalf("InsertCandles (overwrite) error: %v", err)
	}

	cache := candles.NewCache(5, candleDur)
	if err = archie.LoadEpochStats(baseID, quoteID, []*candles.Cache{cache}); err != nil {
		t.Fatalf("LoadEpochStats error: %v", err)
	}

	if len(cache.Candles) != 2 {
		t.Fatalf("Expected 2 candles, got %d", len(cache.Candles))
	}

	if cache.Last().MatchVolume != 1 {
		t.Fatalf("Overwrite failed")
	}

	// With more candles stored than fit in the cache, the newest candles
	// should be loaded, not the oldest.
	if err := cleanTables(archie.db); err != nil {
		t.Fatalf("cleanTables: %v", err)
	}
	const numStored = 20
	cands = make([]*candles.Candle, 0, numStored)
	for i := uint64(1); i <= numStored; i++ {
		cands = append(cands, &candles.Candle{EndStamp: candleDur * i, MatchVolume: i})
	}
	if err = archie.InsertCandles(baseID, quoteID, candleDur, cands); err != nil {
		t.Fatalf("InsertCandles error: %v", err)
	}
	cache = candles.NewCache(5, candleDur)
	if err = archie.LoadEpochStats(baseID, quoteID, []*candles.Cache{cache}); err != nil {
		t.Fatalf("LoadEpochStats error: %v", err)
	}
	cs := cache.CandlesCopy()
	if len(cs) != 5 {
		t.Fatalf("Expected 5 candles, got %d", len(cs))
	}
	for i, c := range cs {
		if want := candleDur * (numStored - 4 + uint64(i)); c.EndStamp != want {
			t.Fatalf("candle %d: wanted end stamp %d, got %d", i, want, c.EndStamp)
		}
	}

	// CandlesBefore pages back through the stored history.
	page, err := archie.CandlesBefore(baseID, quoteID, candleDur, candleDur*11, 4)
	if err != nil {
		t.Fatalf("CandlesBefore error: %v", err)
	}
	if len(page) != 4 {
		t.Fatalf("expected 4 candles, got %d", len(page))
	}
	for i, c := range page { // end stamps 7, 8, 9, 10
		if want := candleDur * (7 + uint64(i)); c.EndStamp != want || c.MatchVolume != 7+uint64(i) || c.StartStamp != want-candleDur {
			t.Fatalf("page candle %d: wrong candle %+v", i, c)
		}
	}
	// Next page from the oldest end stamp. Only 3 left.
	page, err = archie.CandlesBefore(baseID, quoteID, candleDur, page[0].EndStamp, 4)
	if err != nil {
		t.Fatalf("CandlesBefore error: %v", err)
	}
	if len(page) != 4 || page[0].EndStamp != candleDur*3 || page[3].EndStamp != candleDur*6 {
		t.Fatalf("wrong second page: %+v", page)
	}
	page, err = archie.CandlesBefore(baseID, quoteID, candleDur, candleDur*3, 4)
	if err != nil {
		t.Fatalf("CandlesBefore error: %v", err)
	}
	if len(page) != 2 || page[0].EndStamp != candleDur || page[1].EndStamp != candleDur*2 {
		t.Fatalf("wrong last page: %+v", page)
	}
	// Nothing before the first candle.
	page, err = archie.CandlesBefore(baseID, quoteID, candleDur, candleDur, 4)
	if err != nil {
		t.Fatalf("CandlesBefore error: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("expected no candles, got %d", len(page))
	}
	// Other bin sizes have their own tables.
	page, err = archie.CandlesBefore(baseID, quoteID, 60*60*1000, candleDur*100, 4)
	if err != nil {
		t.Fatalf("CandlesBefore (1h) error: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("expected no 1h candles, got %d", len(page))
	}
}
