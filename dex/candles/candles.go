// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package candles

import (
	"time"

	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/utils"
)

const (
	// DefaultCandleRequest is the number of candles to return if the request
	// does not specify otherwise.
	DefaultCandleRequest = 50
	// CacheSize is the default cache size. It is used for any bin size not
	// listed in CacheSizes (e.g. the epoch-duration cache). It is also the
	// maximum number of candles that a server that does not advertise its
	// cache sizes in the 'config' response (i.e. upstream servers) will
	// return for a single request, so clients must not request more than this
	// from such a server.
	CacheSize = 1000
	// MaxCacheSize is a sanity limit on the cache size a client will use for a
	// server-advertised bin size.
	MaxCacheSize = 10_000
	// HistoryPageSize is the maximum number of candles a server will return
	// for one request for older candle history (msgjson.CandlesRequest with
	// Before set).
	HistoryPageSize = 1000
)

var (
	// BinSizes is the default bin sizes for candlestick data sets. Exported for
	// use in the 'config' response. Internally, we will parse these to uint64
	// milliseconds.
	BinSizes = []string{"24h", "1h", "5m"}

	// CacheSizes are the in-memory cache sizes for the BinSizes. These are
	// also the maximum number of candles that can be requested at once for the
	// bin size, and are advertised to clients in the 'config' response. A
	// candle is 64 bytes in memory, so 3000 candles are ~190 KB per market.
	CacheSizes = map[string]int{
		"24h": 1000, // ~2.7 years
		"1h":  3000, // 125 days
		"5m":  3000, // ~10.4 days
	}
)

// CacheSizeFor returns the cache size for the bin size, or CacheSize if the
// bin size has no specific size.
func CacheSizeFor(binSize string) int {
	if sz, ok := CacheSizes[binSize]; ok && sz > 0 {
		return sz
	}
	return CacheSize
}

// Candle is a report about the trading activity of a market over some specified
// period of time. Candles are managed with a Cache, which takes into account
// bin sizes and handles candle addition.
type Candle = msgjson.Candle

// Cache is a sized cache of candles. Cache provides methods for adding to the
// cache and reading cache data out. Candles is a typical slice until it reaches
// capacity, when it becomes a "circular array" to avoid re-allocations.
type Cache struct {
	Candles []Candle
	BinSize uint64
	cap     int
	// cursor will be the index of the last inserted candle.
	cursor int
}

// NewCache is a constructor for a Cache.
func NewCache(cap int, binSize uint64) *Cache {
	return &Cache{
		cap:     cap,
		BinSize: binSize,
	}
}

// Cap is the maximum number of candles the Cache will hold.
func (c *Cache) Cap() int {
	return c.cap
}

// Add adds a new candle TO THE END of the Cache. The caller is responsible to
// ensure that candles added with Add are always newer than the last candle
// added.
func (c *Cache) Add(candle *Candle) {
	sz := len(c.Candles)
	if sz == 0 {
		c.Candles = append(c.Candles, *candle)
		return
	}

	if c.combineCandles(c.Last(), candle) {
		return
	}
	if sz == c.cap { // circular mode
		c.cursor = (c.cursor + 1) % c.cap
		c.Candles[c.cursor] = *candle
		return
	}
	c.Candles = append(c.Candles, *candle)
	c.cursor = sz // len(c.Candles) - 1
}

func (c *Cache) Reset() {
	c.cursor = 0
	c.Candles = nil
}

// WireCandles encodes up to 'count' most recent candles as
// *msgjson.WireCandles. If the Cache contains fewer than 'count', only those
// available will be returned, with no indication of error.
func (c *Cache) WireCandles(count int) *msgjson.WireCandles {
	n := count
	sz := len(c.Candles)
	if sz < n {
		n = sz
	}
	wc := msgjson.NewWireCandles(n)
	for i := sz - n; i < sz; i++ {
		candle := &c.Candles[(c.cursor+1+i)%sz]
		wc.StartStamps = append(wc.StartStamps, candle.StartStamp)
		wc.EndStamps = append(wc.EndStamps, candle.EndStamp)
		wc.MatchVolumes = append(wc.MatchVolumes, candle.MatchVolume)
		wc.QuoteVolumes = append(wc.QuoteVolumes, candle.QuoteVolume)
		wc.HighRates = append(wc.HighRates, candle.HighRate)
		wc.LowRates = append(wc.LowRates, candle.LowRate)
		wc.StartRates = append(wc.StartRates, candle.StartRate)
		wc.EndRates = append(wc.EndRates, candle.EndRate)
	}

	return wc
}

// CandlesCopy returns a deep copy of Candles with the oldest candle at the
// first index.
func (c *Cache) CandlesCopy() []msgjson.Candle {
	sz := len(c.Candles)
	candles := make([]msgjson.Candle, sz)
	switch {
	case sz == 0:
	case sz == c.cap: // circular mode
		oldIdx := (c.cursor + 1) % c.cap
		copy(candles, c.Candles[oldIdx:])
		copy(candles[sz-oldIdx:], c.Candles)
	default:
		copy(candles, c.Candles)
	}
	return candles
}

// Delta calculates the change in rate, as a percentage, and total volume over
// the specified period going backwards from now. Because the first candle does
// not necessarily align with the cutoff, the rate and volume contribution from
// that candle is linearly interpreted between the endpoints. The caller is
// responsible for making sure that dur >> binSize, otherwise the results will
// be of little value.
func (c *Cache) Delta(since time.Time) (changePct float64, vol, high, low uint64) {
	cutoff := uint64(since.UnixMilli())
	sz := len(c.Candles)
	if sz == 0 {
		return 0, 0, 0, 0
	}
	var startRate, endRate uint64
	for i := 0; i < sz; i++ {
		candle := &c.Candles[(c.cursor+sz-i)%sz]
		if candle.EndStamp <= cutoff {
			break
		}

		if endRate == 0 {
			endRate = candle.EndRate
			if endRate == 0 {
				endRate = candle.StartRate
			}
		}

		if low == 0 || (candle.LowRate > 0 && candle.LowRate < low) {
			low = candle.LowRate
		}
		if candle.HighRate > high {
			high = candle.HighRate
		}

		if candle.StartStamp <= cutoff && candle.StartRate != 0 {
			// Interpret the point linearly between the start and end stamps
			cut := float64(cutoff-candle.StartStamp) / float64(candle.EndStamp-candle.StartStamp)
			rateDelta := float64(candle.EndRate) - float64(candle.StartRate)
			r := candle.StartRate + uint64(cut*rateDelta)
			if r > 0 {
				startRate = r
			}
			vol += uint64((1 - cut) * float64(candle.MatchVolume))

			break
		} else if candle.StartRate != 0 {
			startRate = candle.StartRate
		} else if candle.EndRate != 0 {
			startRate = candle.EndRate
		}

		vol += candle.MatchVolume
	}
	if startRate == 0 {
		return 0, vol, high, low
	}
	return (float64(endRate) - float64(startRate)) / float64(startRate), vol, high, low
}

// Last gets the most recent candle in the cache.
func (c *Cache) Last() *Candle {
	return &c.Candles[c.cursor]
}

// CompletedCandlesSince returns any candles that fall into an epoch after the
// epoch of the provided timestamp, and before the current epoch.
func (c *Cache) CompletedCandlesSince(lastStoredEndStamp uint64) (cs []*Candle) {
	currentIdx := uint64(time.Now().UnixMilli()) / c.BinSize
	lastStoredIdx := lastStoredEndStamp / c.BinSize

	sz := len(c.Candles)
	for i := 0; i < sz; i++ {
		// iterate backwards
		candle := &c.Candles[(c.cursor+sz-i)%sz]
		epochIdx := candle.EndStamp / c.BinSize
		if epochIdx >= currentIdx {
			continue
		}
		if epochIdx <= lastStoredIdx {
			break
		}
		cs = append(cs, candle)
	}
	utils.ReverseSlice(cs)
	return
}

// combineCandles attempts to add the candidate candle to the target candle
// in-place, if they're in the same bin, otherwise returns false.
func (c *Cache) combineCandles(target, candidate *Candle) bool {
	if target.EndStamp/c.BinSize != candidate.EndStamp/c.BinSize {
		// The candidate candle cannot be added.
		return false
	}
	target.EndStamp = candidate.EndStamp
	target.EndRate = candidate.EndRate
	if candidate.HighRate > target.HighRate {
		target.HighRate = candidate.HighRate
	}
	if candidate.LowRate < target.LowRate || target.LowRate == 0 {
		target.LowRate = candidate.LowRate
	}
	target.MatchVolume += candidate.MatchVolume
	target.QuoteVolume += candidate.QuoteVolume
	return true
}
