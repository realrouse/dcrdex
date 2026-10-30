// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package apidata

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"decred.org/dcrdex/dex/candles"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/server/comms"
	"decred.org/dcrdex/server/matcher"
)

var dummyErr = fmt.Errorf("dummy error")

type TMarketSource struct {
	base, quote uint32
}

func (m *TMarketSource) EpochDuration() uint64 { return 1000 }
func (m *TMarketSource) Base() uint32          { return m.base }
func (m *TMarketSource) Quote() uint32         { return m.quote }

type historyCall struct {
	dur, before uint64
	n           int
}

type TDBSource struct {
	loadEpochErr error
	history      []candles.Candle
	historyErr   error
	historyCalls []historyCall
	historyWait  chan struct{} // if set, CandlesBefore blocks until closed
	historyMtx   sync.Mutex
}

func (db *TDBSource) CandlesBefore(base, quote uint32, dur, before uint64, n int) ([]candles.Candle, error) {
	db.historyMtx.Lock()
	db.historyCalls = append(db.historyCalls, historyCall{dur, before, n})
	wait := db.historyWait
	db.historyMtx.Unlock()
	if wait != nil {
		<-wait
	}
	return db.history, db.historyErr
}

func (db *TDBSource) LoadEpochStats(base, quote uint32, caches []*candles.Cache) error {
	return db.loadEpochErr
}

func (db *TDBSource) LastCandleEndStamp(base, quote uint32, candleDur uint64) (uint64, error) {
	return 0, nil
}

func (db *TDBSource) InsertCandles(base, quote uint32, dur uint64, cs []*candles.Candle) error {
	return nil
}

type TBookSource struct {
	book *msgjson.OrderBook
}

func (bs *TBookSource) Book(mktName string) (*msgjson.OrderBook, error) {
	return bs.book, nil
}

type testRig struct {
	db  *TDBSource
	api *DataAPI
}

func newTestRig() *testRig {
	db := new(TDBSource)
	return &testRig{
		db:  db,
		api: NewDataAPI(db, func(route string, handler comms.HTTPHandler) {}),
	}
}

func TestAddMarketSource(t *testing.T) {
	rig := newTestRig()
	// initial success
	err := rig.api.AddMarketSource(&TMarketSource{42, 0})
	if err != nil {
		t.Fatalf("AddMarketSource error: %v", err)
	}
	// unknown asset
	err = rig.api.AddMarketSource(&TMarketSource{42, 54321})
	if err == nil {
		t.Fatalf("no error for unknown asset")
	}
	// DB error
	rig.db.loadEpochErr = dummyErr
	err = rig.api.AddMarketSource(&TMarketSource{42, 0})
	if err == nil {
		t.Fatalf("no error for DB error")
	}
	rig.db.loadEpochErr = nil
	// success again
	err = rig.api.AddMarketSource(&TMarketSource{42, 0})
	if err != nil {
		t.Fatalf("AddMarketSource error after: %v", err)
	}
}

func TestReportEpoch(t *testing.T) {
	rig := newTestRig()
	mktSrc := &TMarketSource{42, 0}
	err := rig.api.AddMarketSource(mktSrc)
	if err != nil {
		t.Fatalf("AddMarketSource error: %v", err)
	}
	epoch := uint64(time.Now().UnixMilli()) / mktSrc.EpochDuration()
	epochsPerDay := uint64(time.Hour*24/time.Millisecond) / mktSrc.EpochDuration()
	epochYesterday := epoch - epochsPerDay - 1
	// initial success
	stats := &matcher.MatchCycleStats{
		MatchVolume: 123,
		QuoteVolume: 2,
		HighRate:    10,
		LowRate:     1,
		StartRate:   4,
		EndRate:     5,
	}
	spot, err := rig.api.ReportEpoch(42, 0, epochYesterday, stats)
	if err != nil {
		t.Fatalf("ReportEpoch yesterday error: %v", err)
	}
	if spot.Vol24 != 0 {
		t.Fatalf("wrong spot Vol24. wanted 0, got %d", spot.Vol24)
	}

	spot, err = rig.api.ReportEpoch(42, 0, epoch-1, stats)
	if err != nil {
		t.Fatalf("ReportEpoch last epoch error: %v", err)
	}
	if spot.Vol24 != 123 {
		t.Fatalf("wrong spot Vol24. wanted 123, got %d", spot.Vol24)
	}

	stats.EndRate = 555

	spot, err = rig.api.ReportEpoch(42, 0, epoch, stats)
	if err != nil {
		t.Fatalf("ReportEpoch error: %v", err)
	}
	if spot.Vol24 != 246 {
		t.Fatalf("wrong spot Vol24. wanted 246, got %d", spot.Vol24)
	}
	if spot.Rate != 555 {
		t.Fatalf("wrong spot Rate. wanted 555, got %d", spot.Rate)
	}

	// handleSpots should return one spot for the one market.
	spotsI, err := rig.api.handleSpots(nil)
	if err != nil {
		t.Fatalf("handleSpots error: %v", err)
	}
	spotsEnc := spotsI.([]json.RawMessage)
	if spotsEnc == nil {
		t.Fatalf("failed to decode []json.RawMessage. spotsI is %T", spotsI)
	}
	if len(spotsEnc) != 1 {
		t.Fatalf("expected 1 spot, got %d", len(spotsEnc))
	}
	reSpot := new(msgjson.Spot)
	err = json.Unmarshal(spotsEnc[0], reSpot)
	if err != nil {
		t.Fatalf("error encoding spot: %v", err)
	}
	if reSpot.Vol24 != spot.Vol24 {
		t.Fatalf("reSpot Vol24 mismatch, wanted %d, got %d", spot.Vol24, reSpot.Vol24)
	}

	// handleCandles should return three candles.
	candlesI, err := rig.api.handleCandles(&msgjson.CandlesRequest{
		BaseID:     42,
		QuoteID:    0,
		BinSize:    "1s", // Epoch duration, smallest candle size
		NumCandles: candles.CacheSize,
	})
	if err != nil {
		t.Fatalf("handleCandles error: %v", err)
	}
	wireCandles := candlesI.(*msgjson.WireCandles)
	if wireCandles == nil {
		t.Fatalf("failed to decode *msgjson.WireCandles. candlesI is %T", candlesI)
	}
	if len(wireCandles.StartRates) != 3 {
		t.Fatalf("wrong number of candles. expected 3, got %d", len(wireCandles.StartRates))
	}
}

func TestOrderBook(t *testing.T) {
	rig := newTestRig()
	book := new(msgjson.OrderBook)
	rig.api.SetBookSource(&TBookSource{book})
	bookI, err := rig.api.handleOrderBook(&msgjson.OrderBookSubscription{
		Base:  42,
		Quote: 0,
	})
	if err != nil {
		t.Fatalf("handleOrderBook error: %v", err)
	}
	reBook := bookI.(*msgjson.OrderBook)
	if reBook == nil {
		t.Fatalf("failed to decode *msgjson.OrderBook. bookI is %T", bookI)
	}
	if reBook != book {
		t.Fatalf("where did this book come from?")
	}
}

func TestCandleRequestLimits(t *testing.T) {
	rig := newTestRig()
	if err := rig.api.AddMarketSource(&TMarketSource{42, 0}); err != nil {
		t.Fatalf("AddMarketSource error: %v", err)
	}

	// Each bin size's cache should have its configured size, and the epoch
	// duration cache the default size.
	caches := rig.api.marketCaches["dcr_btc"]
	for binStr, wantCap := range map[string]int{"5m": 3000, "1h": 3000, "24h": 1000, "1s": candles.CacheSize} {
		dur, _ := time.ParseDuration(binStr)
		cache := caches[uint64(dur.Milliseconds())]
		if cache == nil {
			t.Fatalf("no %s cache", binStr)
		}
		if cache.Cap() != wantCap {
			t.Fatalf("wrong %s cache size. wanted %d, got %d", binStr, wantCap, cache.Cap())
		}
	}

	for _, tt := range []struct {
		binSize string
		n       int
		wantErr bool
	}{
		{"5m", 0, false},
		{"5m", candles.CacheSize, false}, // what old clients request
		{"5m", 3000, false},
		{"5m", 3001, true},
		{"1h", 3000, false},
		{"1h", 3001, true},
		{"24h", 1000, false},
		{"24h", 1001, true},
		{"1s", 1000, false},
		{"1s", 1001, true},
		{"5m", -1, true},
		{"15m", 10, true}, // no such bin size
	} {
		_, err := rig.api.handleCandles(&msgjson.CandlesRequest{
			BaseID:     42,
			QuoteID:    0,
			BinSize:    tt.binSize,
			NumCandles: tt.n,
		})
		if (err != nil) != tt.wantErr {
			t.Fatalf("%s x %d: wanted error = %t, got %v", tt.binSize, tt.n, tt.wantErr, err)
		}
	}
}

func TestCandleHistory(t *testing.T) {
	rig := newTestRig()
	if err := rig.api.AddMarketSource(&TMarketSource{42, 0}); err != nil {
		t.Fatalf("AddMarketSource error: %v", err)
	}
	const fiveMins = uint64(5 * 60 * 1000)
	rig.db.history = []candles.Candle{
		{StartStamp: 0, EndStamp: fiveMins, MatchVolume: 1, HighRate: 3},
		{StartStamp: fiveMins, EndStamp: 2 * fiveMins, MatchVolume: 2, HighRate: 4},
	}

	req := func(binSize string, n int, before uint64) (*msgjson.WireCandles, error) {
		rig.db.historyCalls = nil
		wcI, err := rig.api.handleCandles(&msgjson.CandlesRequest{
			BaseID:     42,
			QuoteID:    0,
			BinSize:    binSize,
			NumCandles: n,
			Before:     before,
		})
		if err != nil {
			return nil, err
		}
		return wcI.(*msgjson.WireCandles), nil
	}

	// Success.
	wc, err := req("5m", 100, 1e12)
	if err != nil {
		t.Fatalf("history request error: %v", err)
	}
	if len(rig.db.historyCalls) != 1 || rig.db.historyCalls[0] != (historyCall{fiveMins, 1e12, 100}) {
		t.Fatalf("wrong DB calls: %+v", rig.db.historyCalls)
	}
	cs := wc.Candles()
	if len(cs) != 2 || cs[1].EndStamp != 2*fiveMins || cs[1].MatchVolume != 2 || cs[0].HighRate != 3 {
		t.Fatalf("wrong candles: %+v", cs)
	}

	// Default page size.
	if _, err = req("1h", 0, 1e12); err != nil {
		t.Fatalf("history request error: %v", err)
	}
	if rig.db.historyCalls[0].n != candles.DefaultCandleRequest {
		t.Fatalf("wrong default page size %d", rig.db.historyCalls[0].n)
	}

	// Max page size, and too big.
	if _, err = req("24h", candles.HistoryPageSize, 1e12); err != nil {
		t.Fatalf("max page size error: %v", err)
	}
	if _, err = req("24h", candles.HistoryPageSize+1, 1e12); err == nil {
		t.Fatalf("no error for page too big")
	}
	// The page limit applies even if the bin's cache is bigger.
	if _, err = req("5m", 3000, 1e12); err == nil {
		t.Fatalf("no error for page bigger than the history page size")
	}

	// Epoch candles are not stored.
	if _, err = req("1s", 10, 1e12); err == nil {
		t.Fatalf("no error for epoch-size history")
	}
	// Unknown bin size.
	if _, err = req("15m", 10, 1e12); err == nil {
		t.Fatalf("no error for unknown bin size")
	}
	// Unknown market.
	if _, err = rig.api.handleCandles(&msgjson.CandlesRequest{BaseID: 42, QuoteID: 60, BinSize: "5m", Before: 1}); err == nil {
		t.Fatalf("no error for unknown market")
	}
	if len(rig.db.historyCalls) != 0 {
		t.Fatalf("DB was queried for invalid requests")
	}

	// DB error.
	rig.db.historyErr = dummyErr
	if _, err = req("5m", 10, 1e12); err == nil {
		t.Fatalf("no error for DB error")
	}
	rig.db.historyErr = nil

	// Requests without Before must not touch the DB.
	if _, err = req("5m", 10, 0); err != nil {
		t.Fatalf("live candles error: %v", err)
	}
	if len(rig.db.historyCalls) != 0 {
		t.Fatalf("DB queried for a request without before")
	}

	// Concurrency limit.
	rig.db.historyWait = make(chan struct{})
	errs := make(chan error, maxConcurrentHistoryRequests)
	for i := 0; i < maxConcurrentHistoryRequests; i++ {
		go func() {
			_, err := rig.api.handleCandles(&msgjson.CandlesRequest{BaseID: 42, BinSize: "5m", NumCandles: 1, Before: 1})
			errs <- err
		}()
	}
	// Wait for all slots to fill.
	for i := 0; len(rig.api.historySem) < maxConcurrentHistoryRequests; i++ {
		if i > 500 {
			t.Fatalf("history requests didn't start")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := rig.api.handleCandles(&msgjson.CandlesRequest{BaseID: 42, BinSize: "5m", NumCandles: 1, Before: 1}); err == nil {
		t.Fatalf("no error for too many concurrent history requests")
	}
	close(rig.db.historyWait)
	for i := 0; i < maxConcurrentHistoryRequests; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent request error: %v", err)
		}
	}
	// Slots are released.
	if _, err := rig.api.handleCandles(&msgjson.CandlesRequest{BaseID: 42, BinSize: "5m", NumCandles: 1, Before: 1}); err != nil {
		t.Fatalf("error after concurrent requests finished: %v", err)
	}
}
