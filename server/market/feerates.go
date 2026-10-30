// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"context"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/dex"
)

const (
	// feeRateRefreshInterval is how often the background refresher fetches
	// fresh swap fee rates for a running market.
	feeRateRefreshInterval = 20 * time.Second
	// feeRateFetchTimeout bounds a single background fee rate fetch. It can
	// be generous since nothing waits on it.
	feeRateFetchTimeout = 10 * time.Second
	// feeRateCacheMaxAge is the oldest cached rate getFeeRate will use
	// before falling back to a (time limited) direct fetch.
	feeRateCacheMaxAge = 90 * time.Second
)

// cachedFeeRate is an unscaled swap fee rate fetched in the background.
type cachedFeeRate struct {
	rate  atomic.Uint64
	stamp atomic.Int64 // unix nano, 0 if never fetched
	// fetching is set while a fetch is still running, even one that timed
	// out, so a hung backend does not accumulate fetch goroutines.
	fetching atomic.Bool
}

func (c *cachedFeeRate) set(rate uint64, now time.Time) {
	c.rate.Store(rate)
	c.stamp.Store(now.UnixNano())
}

// get returns the cached rate if it is non-zero and no older than maxAge.
func (c *cachedFeeRate) get(maxAge time.Duration, now time.Time) (uint64, bool) {
	stamp := c.stamp.Load()
	if stamp == 0 || now.Sub(time.Unix(0, stamp)) > maxAge {
		return 0, false
	}
	r := c.rate.Load()
	return r, r != 0
}

// feeRateCache returns the cache for the fetcher, or nil if f is not one of
// the market's own fetchers.
func (m *Market) feeRateCache(assetID uint32, f FeeFetcher) *cachedFeeRate {
	switch {
	case assetID == m.marketInfo.Base && f == m.baseFeeFetcher:
		return &m.feeCacheBase
	case assetID == m.marketInfo.Quote && f == m.quoteFeeFetcher:
		return &m.feeCacheQuote
	}
	return nil
}

// fetchSwapFeeRate fetches the unscaled swap fee rate. It returns 0 if the
// fetch timed out or failed. The timeout is enforced here because not every
// backend honors the context.
// If inFlight is non-nil and a previous fetch is still running, it returns 0
// without starting another.
func fetchSwapFeeRate(f FeeFetcher, timeout time.Duration, inFlight *atomic.Bool) uint64 {
	if inFlight != nil && !inFlight.CompareAndSwap(false, true) {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res := make(chan uint64, 1)
	go func() {
		res <- f.SwapFeeRate(ctx)
		if inFlight != nil {
			inFlight.Store(false)
		}
	}()
	select {
	case rate := <-res:
		if ctx.Err() != nil {
			return 0
		}
		return rate
	case <-ctx.Done():
		return 0
	}
}

// refreshFeeRates fetches both swap fee rates into the cache.
func (m *Market) refreshFeeRates() {
	for _, a := range []struct {
		assetID uint32
		f       FeeFetcher
	}{{m.Base(), m.baseFeeFetcher}, {m.Quote(), m.quoteFeeFetcher}} {
		if a.f == nil {
			continue
		}
		c := m.feeRateCache(a.assetID, a.f)
		if rate := fetchSwapFeeRate(a.f, feeRateFetchTimeout, &c.fetching); rate != 0 {
			c.set(rate, time.Now())
		} else {
			log.Warnf("Background fee rate refresh for %s failed. Keeping the last cached rate.",
				dex.BipIDSymbol(a.assetID))
		}
	}
}

// runFeeRateRefresher keeps the swap fee rate cache warm until ctx is done,
// so that epoch processing and cancel matching never wait on a fee rate
// request to an asset backend.
func (m *Market) runFeeRateRefresher(ctx context.Context) {
	m.refreshFeeRates()
	ticker := time.NewTicker(feeRateRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.refreshFeeRates()
		case <-ctx.Done():
			return
		}
	}
}

// unscaledSwapFeeRate returns the cached swap fee rate if fresh, otherwise it
// fetches it with a short timeout, falling back to the fetcher's last rate.
func (m *Market) unscaledSwapFeeRate(assetID uint32, f FeeFetcher) uint64 {
	c := m.feeRateCache(assetID, f)
	if c != nil {
		if rate, ok := c.get(feeRateCacheMaxAge, time.Now()); ok {
			return rate
		}
	}
	// Do not block indefinitely waiting for fetcher.
	var inFlight *atomic.Bool
	if c != nil {
		inFlight = &c.fetching
	}
	rate := fetchSwapFeeRate(f, time.Second, inFlight)
	if rate == 0 { // timeout, try last known rate
		rate = f.LastRate()
		log.Warnf("Failed to get latest fee rate for %v. Using last known rate %d.",
			dex.BipIDSymbol(assetID), rate)
	} else if c != nil {
		c.set(rate, time.Now())
	}
	return rate
}
