// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DebugServer is a SIMNET-ONLY, loopback-only, unauthenticated HTTP API for
// driving mesh order books from test scripts. Endpoints:
//
//	GET  /mesh/id                     -> {"peerID": "..."}
//	POST /mesh/subscribe {"market":"dcr_btc"}
//	POST /mesh/order {"market":"dcr_btc","sell":true,"qty":1,"lotSize":1,"rate":1,"lifetimeSec":60}
//	POST /mesh/cancel {"id":"<order id>"}
//	GET  /mesh/book?market=dcr_btc    -> {"orders":[...]}
//	GET  /mesh/own                    -> {"orders":[...]}
//	POST /mesh/take {"market":"dcr_btc","sell":false,"qty":1,"rate":1,"lotSize":0,"standing":false,"lifetimeSec":60}
//	POST /mesh/propose {"market":"dcr_btc","orderID":"...","qty":1,"rate":0}  (raw, no compat checks; rate 0 = order rate)
//	GET  /mesh/matches                -> {"matches":[...]}
type DebugServer struct {
	ob     *OrderBooks
	peerID func() string
}

// NewDebugServer creates a debug server for ob.
func NewDebugServer(ob *OrderBooks, peerID func() string) *DebugServer {
	return &DebugServer{ob: ob, peerID: peerID}
}

type debugOrder struct {
	ID string `json:"id"`
	*BookOrder
}

func toDebugOrders(ords []*BookOrder) []*debugOrder {
	out := make([]*debugOrder, 0, len(ords))
	for _, o := range ords {
		out = append(out, &debugOrder{ID: o.ID().String(), BookOrder: o})
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, thing any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(thing)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// Handler returns the HTTP handler.
func (s *DebugServer) Handler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mesh/id", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"peerID": s.peerID()})
	})
	mux.HandleFunc("POST /mesh/subscribe", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Market string `json:"market"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		m, err := ParseMarket(req.Market)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.ob.SubscribeMarket(ctx, m); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /mesh/order", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Market      string `json:"market"`
			Sell        bool   `json:"sell"`
			Qty         uint64 `json:"qty"`
			LotSize     uint64 `json:"lotSize"`
			Rate        uint64 `json:"rate"`
			LifetimeSec int64  `json:"lifetimeSec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		m, err := ParseMarket(req.Market)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		ord, err := s.ob.PostOrder(ctx, m, req.Sell, req.Qty, req.LotSize, req.Rate, time.Duration(req.LifetimeSec)*time.Second)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, &debugOrder{ID: ord.ID().String(), BookOrder: &BookOrder{MeshOrder: ord, Remaining: ord.Qty}})
	})
	mux.HandleFunc("POST /mesh/cancel", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		id, err := ParseOrderID(req.ID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.ob.CancelOrder(ctx, id); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /mesh/book", func(w http.ResponseWriter, r *http.Request) {
		m, err := ParseMarket(r.URL.Query().Get("market"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		ords, err := s.ob.Book(m)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"orders": toDebugOrders(ords)})
	})
	mux.HandleFunc("GET /mesh/own", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"orders": toDebugOrders(s.ob.OwnOrders())})
	})
	mux.HandleFunc("POST /mesh/take", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Market      string `json:"market"`
			Sell        bool   `json:"sell"`
			Qty         uint64 `json:"qty"`
			Rate        uint64 `json:"rate"`
			LotSize     uint64 `json:"lotSize"`
			Standing    bool   `json:"standing"`
			LifetimeSec int64  `json:"lifetimeSec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		m, err := ParseMarket(req.Market)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if req.LifetimeSec == 0 {
			req.LifetimeSec = 300
		}
		res, err := s.ob.Take(ctx, m, req.Sell, req.Qty, req.Rate, req.LotSize, req.Standing, time.Duration(req.LifetimeSec)*time.Second)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /mesh/propose", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Market  string `json:"market"`
			OrderID string `json:"orderID"`
			Qty     uint64 `json:"qty"`
			Rate    uint64 `json:"rate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		m, err := ParseMarket(req.Market)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		id, err := ParseOrderID(req.OrderID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		s.ob.mtx.RLock()
		book := s.ob.books[m]
		s.ob.mtx.RUnlock()
		if book == nil {
			writeErr(w, http.StatusBadRequest, errors.New("not subscribed to market"))
			return
		}
		bo, found := book.Order(id, s.ob.now())
		if !found {
			writeErr(w, http.StatusBadRequest, errors.New("order not on book"))
			return
		}
		if req.Rate == 0 {
			req.Rate = bo.Rate
		}
		mid, _, err := s.ob.Propose(ctx, bo, req.Qty, req.Rate)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"matchID": mid.String()})
	})
	mux.HandleFunc("GET /mesh/matches", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"matches": s.ob.Matches()})
	})
	return mux
}

// ListenAndServe serves on addr, which must be a loopback address, until ctx
// is done.
func (s *DebugServer) ListenAndServe(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("mesh debug server address %q is not a loopback IP", addr)
	}
	srv := &http.Server{Addr: addr, Handler: s.Handler(ctx), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
