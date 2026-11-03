// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

type epochRecordSummary struct {
	Epoch       uint64        `json:"epoch"`
	Prev        string        `json:"prev"`
	Fingerprint string        `json:"fingerprint"`
	StateHash   string        `json:"stateHash"`
	Proposer    string        `json:"proposer"`
	Active      int           `json:"active"`
	ActiveSet   []string      `json:"activeSet"`
	Credits     []string      `json:"credits"` // signers of the previous record (S3)
	Signers     []string      `json:"signers"`
	Orders      int           `json:"orders"`
	Cancels     int           `json:"cancels"`
	Matches     []*EpochMatch `json:"matches"`
	Proposal    any           `json:"proposal,omitempty"`
}

func (s *DebugServer) epochHandlers(ctx context.Context, mux *http.ServeMux) {
	eng := func(w http.ResponseWriter) *EpochEngine {
		e := s.ob.Epochs()
		if e == nil {
			writeErr(w, http.StatusBadRequest, errors.New("epoch mode is off (no release)"))
		}
		return e
	}
	mux.HandleFunc("GET /mesh/epochs/status", func(w http.ResponseWriter, r *http.Request) {
		if e := eng(w); e != nil {
			writeJSON(w, http.StatusOK, e.Status())
		}
	})
	mux.HandleFunc("GET /mesh/epochs/records", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		from, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 1000
		}
		full := r.URL.Query().Get("full") == "1"
		var out []*epochRecordSummary
		for _, rec := range e.Records(from, limit) {
			sum := &epochRecordSummary{Epoch: rec.Epoch, Prev: rec.Prev.String(), Fingerprint: rec.Fingerprint.String(),
				StateHash: rec.StateHash.String(), Proposer: rec.Proposer, Active: len(rec.Active), ActiveSet: rec.Active, Matches: rec.Matches}
			for _, a := range rec.Sigs {
				sum.Signers = append(sum.Signers, a.Validator)
			}
			if rec.Proposal != nil {
				sum.Orders, sum.Cancels = len(rec.Proposal.Orders), len(rec.Proposal.Cancels)
				for _, c := range rec.Proposal.Credits {
					sum.Credits = append(sum.Credits, c.Validator)
				}
				if full {
					sum.Proposal = rec.Proposal
				}
			}
			out = append(out, sum)
		}
		writeJSON(w, http.StatusOK, map[string]any{"records": out})
	})
	// S3: the active set of an epoch computed from our chain up to record
	// asOf (all peers must agree), and a replay of every record's set.
	mux.HandleFunc("GET /mesh/epochs/active", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		n, _ := strconv.ParseUint(r.URL.Query().Get("epoch"), 10, 64)
		asOf, _ := strconv.ParseUint(r.URL.Query().Get("asof"), 10, 64)
		as, err := e.ActiveFor(n, asOf)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, as)
	})
	mux.HandleFunc("GET /mesh/epochs/activecheck", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		from, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
		checked, bad := e.ActiveCheck(from)
		writeJSON(w, http.StatusOK, map[string]any{"checked": checked, "mismatches": bad})
	})
	// S4: bond switch, bonds and the validator set.
	mux.HandleFunc("POST /mesh/epochs/bondswitch", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		if err := e.SignalBondSwitch(); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /mesh/epochs/bonds", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		n, _ := strconv.ParseUint(r.URL.Query().Get("epoch"), 10, 64)
		asOf, _ := strconv.ParseUint(r.URL.Query().Get("asof"), 10, 64)
		writeJSON(w, http.StatusOK, e.BondsStatus(n, asOf))
	})
	mux.HandleFunc("POST /mesh/epochs/bond", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		var req struct {
			AssetID  uint32 `json:"assetID"`
			Amount   uint64 `json:"amount"`
			LockSecs int64  `json:"lockSecs"`
			Fake     bool   `json:"fake"` // simnet: valid-looking tx, never broadcast
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		c, err := e.PostBond(ctx, req.AssetID, req.Amount, req.LockSecs, req.Fake)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"key": c.Key(), "coinID": c.CoinID, "validator": c.Validator})
	})
	mux.HandleFunc("POST /mesh/epochs/peername", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		var req struct {
			PeerID string `json:"peerID"`
			Name   string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		e.SetPeerName(req.PeerID, req.Name)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /mesh/epochs/order", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		id, err := ParseOrderID(r.URL.Query().Get("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": e.OrderStatus(id)})
	})
	mux.HandleFunc("POST /mesh/epochs/omit", func(w http.ResponseWriter, r *http.Request) {
		e := eng(w)
		if e == nil {
			return
		}
		var req struct {
			Owner string `json:"owner"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		e.SetOmitOwner(req.Owner)
		s.ob.log.Warnf("ADVERSARY: proposals will leave out orders of %q", req.Owner)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /mesh/epochs/forge", func(w http.ResponseWriter, r *http.Request) {
		if eng(w) == nil {
			return
		}
		var req struct {
			Market  string `json:"market"`
			OrderID string `json:"orderID"`
			Qty     uint64 `json:"qty"`
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
		mid, _, err := s.ob.ForgeEpochMatch(ctx, bo, req.Qty)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"matchID": mid.String()})
	})
}
