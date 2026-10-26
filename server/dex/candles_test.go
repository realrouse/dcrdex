// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dex

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/server/comms"
	"github.com/go-chi/chi/v5"
)

func TestCandleParamsParser(t *testing.T) {
	var got *msgjson.CandlesRequest
	mux := chi.NewRouter()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = r.Context().Value(comms.CtxThing).(*msgjson.CandlesRequest)
	})
	mux.With(candleParamsParser).Get("/candles/{baseSymbol}/{quoteSymbol}/{binSize}", h)
	mux.With(candleParamsParser).Get("/candles/{baseSymbol}/{quoteSymbol}/{binSize}/{count}", h)

	for _, tt := range []struct {
		path       string
		wantCode   int
		wantCount  int
		wantBefore uint64
	}{
		{"/candles/dcr/btc/5m", http.StatusOK, 0, 0},
		{"/candles/dcr/btc/5m/100", http.StatusOK, 100, 0},
		{"/candles/dcr/btc/1h/100?before=1700000000000", http.StatusOK, 100, 1700000000000},
		{"/candles/dcr/btc/1h?before=1700000000000", http.StatusOK, 0, 1700000000000},
		{"/candles/dcr/btc/1h/100?before=abc", http.StatusBadRequest, 0, 0},
		{"/candles/dcr/btc/1h/100?before=-5", http.StatusBadRequest, 0, 0},
	} {
		got = nil
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
		if rec.Code != tt.wantCode {
			t.Fatalf("%s: wanted code %d, got %d", tt.path, tt.wantCode, rec.Code)
		}
		if tt.wantCode != http.StatusOK {
			continue
		}
		if got == nil {
			t.Fatalf("%s: no request parsed", tt.path)
		}
		if got.BaseID != 42 || got.QuoteID != 0 || got.NumCandles != tt.wantCount || got.Before != tt.wantBefore {
			t.Fatalf("%s: wrong request %+v", tt.path, got)
		}
	}
}
