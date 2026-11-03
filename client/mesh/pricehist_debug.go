// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S7 debug API (simnet): verified price history and adversary reports.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func (s *DebugServer) priceHandlers(ctx context.Context, mux *http.ServeMux) {
	eng := func(w http.ResponseWriter) *EpochEngine {
		e := s.ob.Epochs()
		if e == nil {
			writeErr(w, http.StatusBadRequest, errors.New("epoch mode is off (no release)"))
		}
		return e
	}
	// Verified trades and candles (bin: Go duration, default 1m), plus the
	// candles the UI would show (legacy signed summaries when S7 is off).
	mux.HandleFunc("GET /mesh/epochs/prices", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		bin := time.Minute
		if b := r.URL.Query().Get("bin"); b != "" {
			d, err := time.ParseDuration(b)
			if err != nil || d <= 0 {
				writeErr(w, http.StatusBadRequest, errors.New("bad bin"))
				return
			}
			bin = d
		}
		st := e.PriceHistoryStatus(bin)
		legacy := make(map[string]int)
		for _, d := range DefaultMarkets {
			if m, err := ParseMarket(d.Name); err == nil {
				legacy[d.Name] = len(s.ob.Trades(m, 0))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": st, "signedSummaries": legacy})
	})
	// Submit our half of a trade proof for one of our swaps. force: whatever
	// its status; fake: invented contracts (adversary tests).
	mux.HandleFunc("POST /mesh/epochs/tradeproof", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		var req struct {
			Match string `json:"match"`
			Force bool   `json:"force"`
			Fake  bool   `json:"fake"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		id, err := ParseOrderID(req.Match)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		rec, ok := s.ob.SwapRecord(id)
		if !ok {
			writeErr(w, http.StatusBadRequest, errors.New("no such swap"))
			return
		}
		p, err := e.SubmitTradeProof(ctx, rec, req.Force, req.Fake)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if p == nil {
			writeErr(w, http.StatusBadRequest, errors.New("release has no price history rules"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"key": p.Key(), "content": p.contentKey(), "status": rec.Status,
			"makerTx": coinTxID(p.MakerCoin), "takerTx": coinTxID(p.TakerCoin)})
	})
	// Forge a trade proof for epoch match (epoch, index), which need not
	// exist, with invented contracts, signed by us as maker and taker.
	mux.HandleFunc("POST /mesh/epochs/tradeproof/forge", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		var req struct {
			Market string `json:"market"`
			Epoch  uint64 `json:"epoch"`
			Index  int    `json:"index"`
			Qty    uint64 `json:"qty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		mkt, err := ParseMarket(req.Market)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		m := &Match{Market: mkt, MakerSell: true, Stamp: e.p.EpochEnd(req.Epoch)}
		_, _ = rand.Read(m.ID[:])
		m.MakerLockTime, m.TakerLockTime = m.Stamp/1000+7200, m.Stamp/1000+3600
		p := &TradeProof{Epoch: req.Epoch, Index: req.Index, Qty: req.Qty}
		if p.MakerCoin, p.MakerContract, err = fakeTradeContract(m, true); err == nil {
			p.TakerCoin, p.TakerContract, err = fakeTradeContract(m, false)
		}
		if err == nil {
			if err = e.SignTradeProof(p, true); err == nil {
				err = e.SignTradeProof(p, false)
			}
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		e.SubmitRawTradeProof(ctx, p)
		writeJSON(w, http.StatusOK, map[string]any{"key": p.Key(), "content": p.contentKey()})
	})
	// Publish a fake signed trade summary (the pre-S7 history path) with us
	// as maker and a random taker.
	mux.HandleFunc("POST /mesh/trade/fakesummary", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Market string `json:"market"`
			Rate   uint64 `json:"rate"`
			Qty    uint64 `json:"qty"`
			Taker  string `json:"taker"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		mkt, err := ParseMarket(req.Market)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		m := &Match{Market: mkt, Maker: s.ob.self.String(), Taker: req.Taker, Rate: req.Rate, Qty: req.Qty}
		_, _ = rand.Read(m.ID[:])
		s.ob.publishTrade(m)
		writeJSON(w, http.StatusOK, map[string]any{"match": m.ID.String()})
	})
}
