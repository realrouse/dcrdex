package market

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// tSlowFeeFetcher returns rate from SwapFeeRate, after blocking until the
// gate is closed if one is set. It ignores the context like some backends.
type tSlowFeeFetcher struct {
	rate  atomic.Uint64
	last  uint64
	max   uint64
	gate  chan struct{}
	calls atomic.Int32
}

func (f *tSlowFeeFetcher) FeeRate(ctx context.Context) uint64 { return f.SwapFeeRate(ctx) }
func (f *tSlowFeeFetcher) LastRate() uint64                   { return f.last }
func (f *tSlowFeeFetcher) MaxFeeRate() uint64                 { return f.max }
func (f *tSlowFeeFetcher) SwapFeeRate(ctx context.Context) uint64 {
	f.calls.Add(1)
	if f.gate != nil {
		<-f.gate
	}
	return f.rate.Load()
}

func TestFeeRateCache(t *testing.T) {
	mkt, _, _, cleanup, err := newTestMarket()
	if err != nil {
		t.Fatalf("newTestMarket failure: %v", err)
	}
	defer cleanup()

	base := &tSlowFeeFetcher{last: 7, max: 1000}
	base.rate.Store(10)
	quote := &tSlowFeeFetcher{last: 8, max: 1000}
	quote.rate.Store(20)
	mkt.baseFeeFetcher, mkt.quoteFeeFetcher = base, quote

	// Background refresh fills the cache.
	mkt.refreshFeeRates()
	if r := mkt.getFeeRate(mkt.Base(), base); r != 10 {
		t.Fatalf("wrong base rate %d", r)
	}
	if r := mkt.getFeeRate(mkt.Quote(), quote); r != 20 {
		t.Fatalf("wrong quote rate %d", r)
	}

	// A failed refresh keeps the last good cached value.
	base.rate.Store(0)
	quote.rate.Store(0)
	mkt.refreshFeeRates()
	if r := mkt.getFeeRate(mkt.Quote(), quote); r != 20 {
		t.Fatalf("failed refresh replaced cached rate, got %d", r)
	}
	base.rate.Store(10)
	mkt.refreshFeeRates()

	// With a fresh cache, a hung backend is never consulted.
	base.gate = make(chan struct{})
	defer close(base.gate)
	calls := base.calls.Load()
	start := time.Now()
	if r := mkt.getFeeRate(mkt.Base(), base); r != 10 {
		t.Fatalf("wrong cached base rate %d", r)
	}
	if time.Since(start) > 100*time.Millisecond || base.calls.Load() != calls {
		t.Fatalf("getFeeRate hit the backend despite a fresh cache")
	}

	// A stale cache falls back to a bounded direct fetch, then LastRate.
	mkt.feeCacheBase.stamp.Store(time.Now().Add(-2 * feeRateCacheMaxAge).UnixNano())
	start = time.Now()
	if r := mkt.getFeeRate(mkt.Base(), base); r != 7 {
		t.Fatalf("expected last rate 7, got %d", r)
	}
	if el := time.Since(start); el < time.Second || el > 3*time.Second {
		t.Fatalf("direct fetch not bounded as expected: %v", el)
	}
	// The hung fetch is still in flight, so another is not started.
	calls = base.calls.Load()
	if r := mkt.getFeeRate(mkt.Base(), base); r != 7 || base.calls.Load() != calls {
		t.Fatalf("started a second fetch while one is hung (rate %d)", r)
	}

	// A fetcher that is not one of the market's own is not cached.
	other := &tSlowFeeFetcher{max: 1000}
	other.rate.Store(33)
	if r := mkt.getFeeRate(mkt.Base(), other); r != 33 {
		t.Fatalf("wrong rate from foreign fetcher %d", r)
	}
}
