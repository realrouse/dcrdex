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
//	POST /mesh/order {"market":"dcr_btc","sell":true,"qty":1,"rate":1,"lifetimeSec":60}
//	POST /mesh/cancel {"id":"<order id>"}
//	GET  /mesh/book?market=dcr_btc    -> {"orders":[...]}
//	GET  /mesh/own                    -> {"orders":[...]}
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
	*MeshOrder
}

func toDebugOrders(ords []*MeshOrder) []*debugOrder {
	out := make([]*debugOrder, 0, len(ords))
	for _, o := range ords {
		out = append(out, &debugOrder{ID: o.ID().String(), MeshOrder: o})
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
		ord, err := s.ob.PostOrder(ctx, m, req.Sell, req.Qty, req.Rate, time.Duration(req.LifetimeSec)*time.Second)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, &debugOrder{ID: ord.ID().String(), MeshOrder: ord})
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
