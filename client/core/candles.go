// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"errors"
	"fmt"
	"slices"

	"decred.org/dcrdex/dex/msgjson"
)

// ErrCandleHistoryUnsupported is returned by CandleHistory when the server
// does not support requests for older candle history. Upstream servers don't.
var ErrCandleHistoryUnsupported = errors.New("server does not support candle history")

// CandleHistory fetches a page of older candles for the market from the
// server: up to n candles of the bin size with end stamps before the before
// stamp (unix ms), oldest first. To page back, pass the EndStamp of the oldest
// candle already held. Fewer than n candles means there are no older candles.
// n <= 0 or larger than the server's page size requests a full page. The
// candles are not added to the live candle cache that feeds the market's
// BookFeed.
//
// If the server does not support candle history, ErrCandleHistoryUnsupported
// is returned and the caller should only show the candles from the live feed.
// Exchange.CandlePageSize can be checked beforehand.
func (c *Core) CandleHistory(host string, base, quote uint32, binSize string, before uint64, n int) ([]msgjson.Candle, error) {
	host, err := addrHost(host)
	if err != nil {
		return nil, newError(addressParseErr, "error parsing address: %w", err)
	}
	c.connMtx.RLock()
	dc, found := c.conns[host]
	c.connMtx.RUnlock()
	if !found {
		return nil, fmt.Errorf("unknown DEX %s", host)
	}

	cfg := dc.config()
	if cfg == nil {
		return nil, fmt.Errorf("no config for %s", host)
	}
	// Never send Before to a server that doesn't advertise support. It would
	// ignore the field and return the most recent candles instead.
	if cfg.CandlePageSize <= 0 {
		return nil, ErrCandleHistoryUnsupported
	}
	if !slices.Contains(cfg.BinSizes, binSize) {
		return nil, fmt.Errorf("bin size %q not supported by %s", binSize, host)
	}
	if before == 0 {
		return nil, errors.New("no before stamp")
	}
	mktID := marketName(base, quote)
	if dc.marketConfig(mktID) == nil {
		return nil, fmt.Errorf("unknown market %s", mktID)
	}
	if n <= 0 || n > cfg.CandlePageSize {
		n = cfg.CandlePageSize
	}

	req := &msgjson.CandlesRequest{
		BaseID:     base,
		QuoteID:    quote,
		BinSize:    binSize,
		NumCandles: n,
		Before:     before,
	}
	wc := new(msgjson.WireCandles)
	if err := sendRequest(dc.WsConn, msgjson.CandlesRoute, req, wc, DefaultResponseTimeout); err != nil {
		return nil, err
	}
	return validateCandleHistory(wc, before, n)
}

// validateCandleHistory checks a candle history response, so that a bad
// server response can't produce a corrupted chart.
func validateCandleHistory(wc *msgjson.WireCandles, before uint64, n int) ([]msgjson.Candle, error) {
	sz := len(wc.StartStamps)
	for _, s := range [][]uint64{wc.EndStamps, wc.MatchVolumes, wc.QuoteVolumes,
		wc.HighRates, wc.LowRates, wc.StartRates, wc.EndRates} {
		if len(s) != sz {
			return nil, errors.New("malformed candles response")
		}
	}
	if sz > n {
		return nil, fmt.Errorf("server returned %d candles, requested %d", sz, n)
	}
	cs := make([]msgjson.Candle, 0, sz)
	for _, c := range wc.Candles() {
		if c.EndStamp >= before {
			return nil, fmt.Errorf("server returned a candle ending at %d, not before %d", c.EndStamp, before)
		}
		if len(cs) > 0 && c.EndStamp <= cs[len(cs)-1].EndStamp {
			return nil, errors.New("server returned unordered candles")
		}
		cs = append(cs, *c)
	}
	return cs, nil
}
