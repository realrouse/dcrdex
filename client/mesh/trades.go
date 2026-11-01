// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/candles"
	"decred.org/dcrdex/dex/msgjson"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Trade history (price history) for mesh markets.
//
// When a swap completes, each side signs a TradeSummary (market, rate, qty,
// time, match ID and the two participants' mesh peer IDs; no addresses, IPs or
// coins) and publishes it on the market's order book topic. Every client keeps
// the summaries it sees, deduplicated by match ID, in a local file, and builds
// candles from them. A client that subscribes to a market asks for recent
// history, and peers answer by re-publishing the summaries they hold (they are
// signed by a participant, so anyone can relay them).
//
// Limits: a summary proves that one of the two named peers claims the trade.
// It can't prove the trade happened on chain, and two colluding peers can
// report trades between themselves, as on any exchange without a server.
const (
	tradeSigDomain = "tatanka-mesh-trade-v1"

	msgTypeTrade   = "trade"
	msgTypeHistory = "history"

	// TradeRetention is how long trades are kept locally.
	TradeRetention = 30 * 24 * time.Hour
	// TradeBackfillWindow is how far back peers re-publish trades when asked.
	TradeBackfillWindow = 7 * 24 * time.Hour
	// maxTradesPerMarket caps the trades kept per market (oldest dropped).
	maxTradesPerMarket = 20_000
	// maxHistoryReply caps the trades re-published per history reply.
	maxHistoryReply = 200
	// historyReplyInterval is the shortest time between our history replies
	// on one market.
	historyReplyInterval = 30 * time.Second
	// historyRequestInterval is how often a market with no known trades asks
	// again.
	historyRequestInterval = 5 * time.Minute
	// maxTradeValue bounds rate and qty (atoms and message-rate units).
	maxTradeValue = 1 << 56
)

// TradeSummary is a signed report of a completed mesh swap.
type TradeSummary struct {
	Market  Market  `json:"market"`
	MatchID MatchID `json:"id"`
	Rate    uint64  `json:"rate"`
	Qty     uint64  `json:"qty"`
	Stamp   int64   `json:"stamp"` // unix ms
	Maker   string  `json:"maker"`
	Taker   string  `json:"taker"`
	ByTaker bool    `json:"byTaker,omitempty"` // signer is the taker, else the maker
	Sig     []byte  `json:"sig"`
}

func (t *TradeSummary) serialize() []byte {
	b := new(bytes.Buffer)
	b.WriteString(tradeSigDomain)
	b.Write(t.MatchID[:])
	_ = binary.Write(b, binary.BigEndian, t.Market.Base)
	_ = binary.Write(b, binary.BigEndian, t.Market.Quote)
	_ = binary.Write(b, binary.BigEndian, t.Rate)
	_ = binary.Write(b, binary.BigEndian, t.Qty)
	_ = binary.Write(b, binary.BigEndian, t.Stamp)
	writeStr(b, t.Maker)
	writeStr(b, t.Taker)
	if t.ByTaker {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// Signer is the peer ID of the participant that signed the summary.
func (t *TradeSummary) Signer() string {
	if t.ByTaker {
		return t.Taker
	}
	return t.Maker
}

// Sign signs the summary with priv, which must be the maker's or the taker's.
func (t *TradeSummary) Sign(priv libp2pcrypto.PrivKey) error {
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return err
	}
	switch pid.String() {
	case t.Taker:
		t.ByTaker = true
	case t.Maker:
		t.ByTaker = false
	default:
		return errors.New("signer is not a participant of the trade")
	}
	t.Sig, err = priv.Sign(t.serialize())
	return err
}

// Validate bounds-checks the summary and checks that it is signed by one of
// its participants.
func (t *TradeSummary) Validate(now time.Time) error {
	if _, err := t.Market.Name(); err != nil {
		return err
	}
	switch {
	case t.Rate == 0 || t.Rate > maxTradeValue:
		return errors.New("rate out of range")
	case t.Qty == 0 || t.Qty > maxTradeValue:
		return errors.New("qty out of range")
	case t.Stamp > now.Add(MaxClockSkew).UnixMilli():
		return errors.New("trade stamp is in the future")
	case t.Stamp < now.Add(-TradeRetention).UnixMilli():
		return errors.New("trade is too old")
	case t.Maker == t.Taker:
		return errors.New("maker is the taker")
	case len(t.Sig) == 0 || len(t.Sig) > 128:
		return errors.New("bad signature length")
	}
	for _, p := range []string{t.Maker, t.Taker} {
		if _, err := peer.Decode(p); err != nil {
			return fmt.Errorf("bad participant ID: %w", err)
		}
	}
	pid, _ := peer.Decode(t.Signer())
	pub, err := pid.ExtractPublicKey()
	if err != nil {
		return fmt.Errorf("no public key in signer ID: %w", err)
	}
	ok, err := pub.Verify(t.serialize(), t.Sig)
	if err != nil || !ok {
		return errors.New("bad signature")
	}
	return nil
}

// TradeHistory holds trade summaries, deduplicated by match ID, optionally
// persisted to a file.
type TradeHistory struct {
	mtx    sync.Mutex
	path   string
	trades map[MatchID]*TradeSummary
	dirty  bool
}

func newTradeHistory() *TradeHistory {
	return &TradeHistory{trades: make(map[MatchID]*TradeSummary)}
}

// Add adds a summary. It returns false if the match is already known.
func (h *TradeHistory) Add(t *TradeSummary) bool {
	h.mtx.Lock()
	defer h.mtx.Unlock()
	if _, found := h.trades[t.MatchID]; found {
		return false
	}
	h.trades[t.MatchID] = t
	h.dirty = true
	return true
}

// Has is true if the match is known.
func (h *TradeHistory) Has(id MatchID) bool {
	h.mtx.Lock()
	defer h.mtx.Unlock()
	_, found := h.trades[id]
	return found
}

// Trades returns the market's trades since sinceMs (unix ms), oldest first.
func (h *TradeHistory) Trades(m Market, sinceMs int64) []*TradeSummary {
	h.mtx.Lock()
	out := make([]*TradeSummary, 0)
	for _, t := range h.trades {
		if t.Market == m && t.Stamp >= sinceMs {
			out = append(out, t)
		}
	}
	h.mtx.Unlock()
	sortTrades(out)
	return out
}

func sortTrades(ts []*TradeSummary) {
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Stamp != ts[j].Stamp {
			return ts[i].Stamp < ts[j].Stamp
		}
		return bytes.Compare(ts[i].MatchID[:], ts[j].MatchID[:]) < 0
	})
}

// prune drops trades older than the retention and caps each market.
func (h *TradeHistory) prune(now time.Time) {
	cutoff := now.Add(-TradeRetention).UnixMilli()
	h.mtx.Lock()
	defer h.mtx.Unlock()
	perMkt := make(map[Market][]*TradeSummary)
	for id, t := range h.trades {
		if t.Stamp < cutoff {
			delete(h.trades, id)
			h.dirty = true
			continue
		}
		perMkt[t.Market] = append(perMkt[t.Market], t)
	}
	for _, ts := range perMkt {
		if len(ts) <= maxTradesPerMarket {
			continue
		}
		sortTrades(ts)
		for _, t := range ts[:len(ts)-maxTradesPerMarket] {
			delete(h.trades, t.MatchID)
		}
		h.dirty = true
	}
}

// load reads the history file, keeping the summaries that still validate.
func (h *TradeHistory) load(path string, now time.Time) (int, error) {
	h.mtx.Lock()
	h.path = path
	h.mtx.Unlock()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var ts []*TradeSummary
	if err := json.Unmarshal(b, &ts); err != nil {
		return 0, fmt.Errorf("bad trade history file: %w", err)
	}
	var n int
	for _, t := range ts {
		if t.Validate(now) == nil && h.Add(t) {
			n++
		}
	}
	h.mtx.Lock()
	h.dirty = false
	h.mtx.Unlock()
	return n, nil
}

// save writes the history file if it changed.
func (h *TradeHistory) save() error {
	h.mtx.Lock()
	if h.path == "" || !h.dirty {
		h.mtx.Unlock()
		return nil
	}
	ts := make([]*TradeSummary, 0, len(h.trades))
	for _, t := range h.trades {
		ts = append(ts, t)
	}
	path := h.path
	h.dirty = false
	h.mtx.Unlock()
	sortTrades(ts)
	b, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CandleBinSizes are the candle durations served for mesh markets, as for
// DEX servers.
var CandleBinSizes = candles.BinSizes

// BuildCandles builds candles of binSize from trades sorted oldest first.
func BuildCandles(trades []*TradeSummary, binSize time.Duration) *candles.Cache {
	binMs := uint64(binSize.Milliseconds())
	c := candles.NewCache(maxTradesPerMarket, binMs)
	for _, t := range trades {
		stamp := uint64(t.Stamp)
		// Candles are aligned to bins; a trade's candle spans its bin.
		start := stamp / binMs * binMs
		c.Add(&candles.Candle{
			StartStamp:  start,
			EndStamp:    stamp,
			MatchVolume: t.Qty,
			QuoteVolume: calc.BaseToQuote(t.Rate, t.Qty),
			HighRate:    t.Rate,
			LowRate:     t.Rate,
			StartRate:   t.Rate,
			EndRate:     t.Rate,
		})
	}
	return c
}

// MarketSpot is the 24 hour summary of a market's trades, or nil if the market
// has no trades.
func MarketSpot(m Market, trades []*TradeSummary, now time.Time) *msgjson.Spot {
	if len(trades) == 0 {
		return nil
	}
	last := trades[len(trades)-1]
	spot := &msgjson.Spot{Stamp: uint64(last.Stamp), BaseID: m.Base, QuoteID: m.Quote, Rate: last.Rate}
	cutoff := now.Add(-24 * time.Hour).UnixMilli()
	var first *TradeSummary
	for _, t := range trades {
		if t.Stamp < cutoff {
			continue
		}
		if first == nil {
			first = t
		}
		spot.Vol24 += t.Qty
		if t.Rate > spot.High24 {
			spot.High24 = t.Rate
		}
		if spot.Low24 == 0 || t.Rate < spot.Low24 {
			spot.Low24 = t.Rate
		}
	}
	if first != nil {
		spot.Change24 = (float64(last.Rate) - float64(first.Rate)) / float64(first.Rate)
	}
	return spot
}

// SetTradeStore loads trade history from path and keeps it saved there.
func (ob *OrderBooks) SetTradeStore(path string) (int, error) {
	return ob.trades.load(path, ob.now())
}

// Trades returns a market's trade history since sinceMs, oldest first.
func (ob *OrderBooks) Trades(m Market, sinceMs int64) []*TradeSummary {
	return ob.trades.Trades(m, sinceMs)
}

// handleTrade validates and records a trade summary from the market topic.
func (ob *OrderBooks) handleTrade(m Market, t *TradeSummary) {
	if t.Market != m {
		ob.log.Debugf("Trade summary for %v on the %v topic", t.Market, m)
		return
	}
	if err := t.Validate(ob.now()); err != nil {
		ob.log.Debugf("Rejected trade summary %s: %v", t.MatchID, err)
		return
	}
	if !ob.trades.Add(t) {
		return
	}
	ob.log.Debugf("Mesh trade %s on %s: %d @ %d", t.MatchID, marketName(m), t.Qty, t.Rate)
	ob.emitTrade(t)
}

func (ob *OrderBooks) emitTrade(t *TradeSummary) {
	ob.emit(&Event{Kind: EventTrade, Market: marketName(t.Market), MatchID: t.MatchID.String(),
		Qty: t.Qty, Rate: t.Rate, Maker: t.Maker, Taker: t.Taker})
}

// signTrade makes our signed summary of a completed match.
func (ob *OrderBooks) signTrade(m *Match, stamp int64) (*TradeSummary, error) {
	t := &TradeSummary{Market: m.Market, MatchID: m.ID, Rate: m.Rate, Qty: m.Qty, Stamp: stamp,
		Maker: m.Maker, Taker: m.Taker}
	if err := t.Sign(ob.priv); err != nil {
		return nil, err
	}
	return t, nil
}

// publishTrade records and publishes our summary of a completed swap. It does
// not block.
func (ob *OrderBooks) publishTrade(m *Match) {
	if ob.trades.Has(m.ID) {
		return
	}
	t, err := ob.signTrade(m, ob.now().UnixMilli())
	if err != nil {
		ob.log.Errorf("Error signing trade summary for %s: %v", m.ID, err)
		return
	}
	ob.trades.Add(t)
	if err := ob.trades.save(); err != nil {
		ob.log.Errorf("Error saving mesh trade history: %v", err)
	}
	ob.emitTrade(t)
	topic, err := m.Market.Topic()
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ob.broadcast(ctx, topic, &bookMsg{Type: msgTypeTrade, Trade: t})
	}()
}

// recordCompletedSwaps adds our signed summaries of completed swaps from the
// swap store (e.g. swaps from before trade history existed).
func (ob *OrderBooks) recordCompletedSwaps(recs []*SwapRecord) {
	cutoff := ob.now().Add(-TradeRetention).UnixMilli()
	var n int
	for _, r := range recs {
		if r.Status != SwapComplete || r.Updated < cutoff || ob.trades.Has(r.Match.ID) {
			continue
		}
		stamp := r.Updated
		if stamp > ob.now().UnixMilli() {
			stamp = ob.now().UnixMilli()
		}
		t, err := ob.signTrade(&r.Match, stamp)
		if err != nil {
			continue
		}
		if ob.trades.Add(t) {
			n++
		}
	}
	if n > 0 {
		ob.log.Infof("Added %d completed mesh swaps to the trade history", n)
		if err := ob.trades.save(); err != nil {
			ob.log.Errorf("Error saving mesh trade history: %v", err)
		}
	}
}

// requestHistory asks the market's peers for recent trades and publishes our
// own recent ones.
func (ob *OrderBooks) requestHistory(ctx context.Context, m Market) {
	topic, err := m.Topic()
	if err != nil {
		return
	}
	ob.mtx.Lock()
	ob.histReq[m] = ob.now()
	ob.mtx.Unlock()
	ob.broadcast(ctx, topic, &bookMsg{Type: msgTypeHistory})
	self := ob.self.String()
	var own []*TradeSummary
	for _, t := range ob.trades.Trades(m, ob.now().Add(-TradeBackfillWindow).UnixMilli()) {
		if t.Signer() == self {
			own = append(own, t)
		}
	}
	ob.sendTrades(ctx, topic, own)
}

// replyHistory re-publishes recent trades on the market when a peer asks or
// joins, at most once per historyReplyInterval and after a random delay.
func (ob *OrderBooks) replyHistory(ctx context.Context, m Market) {
	topic, err := m.Topic()
	if err != nil {
		return
	}
	now := ob.now()
	ob.mtx.Lock()
	if now.Sub(ob.histReply[m]) < historyReplyInterval {
		ob.mtx.Unlock()
		return
	}
	ob.histReply[m] = now
	ob.mtx.Unlock()
	ts := ob.trades.Trades(m, now.Add(-TradeBackfillWindow).UnixMilli())
	if len(ts) == 0 {
		return
	}
	go func() {
		select {
		case <-time.After(time.Duration(rand.Int63n(int64(2 * time.Second)))):
		case <-ctx.Done():
			return
		}
		ob.sendTrades(ctx, topic, ts)
	}()
}

// sendTrades publishes up to maxHistoryReply of the newest trades, paced.
func (ob *OrderBooks) sendTrades(ctx context.Context, topic string, ts []*TradeSummary) {
	if len(ts) > maxHistoryReply {
		ts = ts[len(ts)-maxHistoryReply:]
	}
	for _, t := range ts {
		ob.broadcast(ctx, topic, &bookMsg{Type: msgTypeTrade, Trade: t})
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

// maintainTrades prunes and saves the history and re-asks for history on
// markets where we know no trades.
func (ob *OrderBooks) maintainTrades(ctx context.Context) {
	now := ob.now()
	ob.trades.prune(now)
	if err := ob.trades.save(); err != nil {
		ob.log.Errorf("Error saving mesh trade history: %v", err)
	}
	ob.mtx.RLock()
	var ask []Market
	for m := range ob.books {
		if now.Sub(ob.histReq[m]) >= historyRequestInterval && len(ob.trades.Trades(m, 0)) == 0 {
			ask = append(ask, m)
		}
	}
	ob.mtx.RUnlock()
	for _, m := range ask {
		ob.requestHistory(ctx, m)
	}
}
