// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	meshpkg "decred.org/dcrdex/client/mesh"
)

const (
	tBTC = 0
	tDCR = 42
	tLBC = 140
	tLot = 1e8
)

func tMkt(name string, base, quote uint32) *qsMarket {
	return &qsMarket{Source: QuickSwapMesh, Host: MeshHost, Name: name, Base: base, Quote: quote, LotSize: tLot}
}

func (m *qsMarket) tSell(id string, rate, lots uint64) *qsMarket {
	m.Sells = append(m.Sells, &qsOrder{ID: id, Owner: "maker-" + id, Rate: rate, Qty: lots * tLot, LotSize: tLot, Sell: true})
	m.sortBooks()
	return m
}

func (m *qsMarket) tBuy(id string, rate, lots uint64) *qsMarket {
	m.Buys = append(m.Buys, &qsOrder{ID: id, Owner: "maker-" + id, Rate: rate, Qty: lots * tLot, LotSize: tLot})
	m.sortBooks()
	return m
}

func TestQuickSwapRouteDirect(t *testing.T) {
	// lbc_dcr: asks at 0.01 (3 lots) and 0.0102 (5 lots) DCR per LBC.
	m := tMkt("lbc_dcr", tLBC, tDCR).tSell("a", 1_000_000, 3).tSell("b", 1_020_000, 5)
	// 0.05 DCR buys 3 LBC at 0.01 (0.03 DCR), then 1 LBC at 0.0102; the
	// rest (0.0098 DCR) is less than a lot.
	routes := qsFindRoutes([]*qsMarket{m}, tDCR, tLBC, 5_000_000, "", nil)
	if len(routes) != 1 {
		t.Fatalf("want 1 route, got %d", len(routes))
	}
	r := routes[0]
	if r.Hops() != 1 || r.Out != 4*tLot || r.In != 3_000_000+1_020_000 {
		t.Fatalf("bad route: hops %d out %d in %d", r.Hops(), r.Out, r.In)
	}
	st := r.Steps[0]
	if st.Sell || len(st.Fills) != 2 || st.Fills[0].Lots != 3 || st.Fills[1].Lots != 1 || st.WorstRate != 1_020_000 {
		t.Fatalf("bad step %+v", st)
	}
	// Selling LBC into the bids: best bid first.
	m.tBuy("c", 990_000, 2).tBuy("d", 995_000, 1)
	routes = qsFindRoutes([]*qsMarket{m}, tLBC, tDCR, 2*tLot+5, "", nil)
	r = routes[0]
	if !r.Steps[0].Sell || r.In != 2*tLot || r.Out != 995_000+990_000 || r.Steps[0].Fills[0].OrderID != "d" {
		t.Fatalf("bad sell route: in %d out %d fills %+v", r.In, r.Out, r.Steps[0].Fills)
	}
	// Own orders are skipped.
	if routes = qsFindRoutes([]*qsMarket{m}, tLBC, tDCR, 2*tLot, "maker-d", nil); routes[0].Steps[0].Fills[0].OrderID != "c" {
		t.Fatalf("own order not skipped")
	}
	// Nothing to buy: less than one lot.
	if routes = qsFindRoutes([]*qsMarket{m}, tDCR, tLBC, 999_999, "", nil); len(routes) != 0 {
		t.Fatalf("want no route for less than one lot, got %d", len(routes))
	}
}

func TestQuickSwapRouteTwoHop(t *testing.T) {
	// BTC -> LBC. Direct lbc_btc asks at 0.000101 BTC/LBC. Via DCR: buy DCR
	// at 0.01 BTC, then LBC at 0.01 DCR: 0.0001 BTC/LBC, cheaper.
	direct := tMkt("lbc_btc", tLBC, tBTC).tSell("x", 10_100, 50)
	m1 := tMkt("dcr_btc", tDCR, tBTC).tSell("p", 1_000_000, 10)
	m2 := tMkt("lbc_dcr", tLBC, tDCR).tSell("q", 1_000_000, 300)
	amount := uint64(3_000_000) // 0.03 BTC
	routes := qsFindRoutes([]*qsMarket{direct, m1, m2}, tBTC, tLBC, amount, "", nil)
	if len(routes) != 2 {
		t.Fatalf("want 2 routes, got %d", len(routes))
	}
	best := routes[0]
	// 0.03 BTC -> 3 DCR -> 300 LBC. Direct: 0.03/0.000101 -> 50 lots max
	// (book depth) = 50 LBC.
	if best.Hops() != 2 || best.Out != 300*tLot || best.Steps[0].Out != 3*tLot || best.Steps[1].In != 3*tLot {
		t.Fatalf("bad best route %s: out %d", best.Label, best.Out)
	}
	if routes[1].Hops() != 1 || routes[1].Out != 50*tLot {
		t.Fatalf("bad direct route: %s out %d", routes[1].Label, routes[1].Out)
	}
	if !strings.Contains(best.Label, "BTC → DCR → LBC") {
		t.Fatalf("label %q", best.Label)
	}
	// Step 1 shrinks to what step 2 can use: only 100 LBC on the DCR book
	// means 1 DCR is enough.
	m2.Sells[0].Qty = 100 * tLot
	routes = qsFindRoutes([]*qsMarket{direct, m1, m2}, tBTC, tLBC, amount, "", nil)
	best = routes[0]
	if best.Out != 100*tLot || best.Steps[0].In != 1_000_000 || best.Steps[0].Out != tLot || best.Leftover != 0 {
		t.Fatalf("step 1 not shrunk: %s in %d out %d leftover %d", best.Label, best.Steps[0].In, best.Steps[0].Out, best.Leftover)
	}
	// A fee reserve on the intermediate coin is kept back.
	reserve := func(a uint32) uint64 {
		if a == tDCR {
			return 100_000
		}
		return 0
	}
	routes = qsFindRoutes([]*qsMarket{direct, m1, m2}, tBTC, tLBC, amount, "", reserve)
	best = routes[0]
	// 2 DCR bought, 1.999 DCR usable -> 100 LBC (book limit), shrink keeps
	// 1 DCR + reserve -> needs 2 DCR lots.
	if best.Steps[1].In != tLot || best.Steps[0].Out != 2*tLot || best.Leftover != tLot {
		t.Fatalf("reserve not applied: step1 out %d step2 in %d leftover %d", best.Steps[0].Out, best.Steps[1].In, best.Leftover)
	}
	// Equal outputs: direct wins.
	eq := tMkt("lbc_btc", tLBC, tBTC).tSell("y", 10_000, 1000)
	routes = qsFindRoutes([]*qsMarket{eq, m1, tMkt("lbc_dcr", tLBC, tDCR).tSell("q", 1_000_000, 1000)}, tBTC, tLBC, amount, "", nil)
	if routes[0].Hops() != 1 || routes[0].Out != routes[1].Out {
		t.Fatalf("tie should prefer direct: %s %d vs %s %d", routes[0].Label, routes[0].Out, routes[1].Label, routes[1].Out)
	}
	// No path.
	if routes = qsFindRoutes([]*qsMarket{m1}, tBTC, tLBC, amount, "", nil); len(routes) != 0 {
		t.Fatalf("want no route")
	}
}

func TestQuickSwapMinOut(t *testing.T) {
	if v := qsMinOut(1000_0000, 1); v != 990_0000 {
		t.Fatalf("minOut %d", v)
	}
	if v := qsMinOut(1000, 0); v != 1000 {
		t.Fatalf("minOut %d", v)
	}
	if qsCheckDrift(-1) == nil || qsCheckDrift(11) == nil || qsCheckDrift(math.NaN()) == nil || qsCheckDrift(1) != nil {
		t.Fatal("drift check")
	}
	if p := qsRateWorsePct(false, 1_000_000, 1_003_000); math.Abs(p-0.3) > 1e-9 {
		t.Fatalf("buy worse %f", p)
	}
	if p := qsRateWorsePct(true, 1_000_000, 997_000); math.Abs(p-0.3) > 1e-9 {
		t.Fatalf("sell worse %f", p)
	}
}

// tBackend is a fake mesh: takes resolve per the order's behavior.
type tBackend struct {
	mtx     sync.Mutex
	mkts    []*qsMarket
	behave  map[string]string // order ID -> agree (default), vanish, timeout, refund
	matches map[string]*tMatch
	taken   []string
	n       int
	onTake  func(orderID string)
}

type tMatch struct {
	order  string
	status string
	reason string
	qty    uint64
	quote  uint64
	swap   string
	polls  int
}

func (b *tBackend) markets() ([]*qsMarket, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	out := make([]*qsMarket, 0, len(b.mkts))
	for _, m := range b.mkts {
		c := *m
		c.Sells = append([]*qsOrder(nil), m.Sells...)
		c.Buys = append([]*qsOrder(nil), m.Buys...)
		out = append(out, &c)
	}
	return out, nil
}

func (b *tBackend) self() string              { return "me" }
func (b *tBackend) peerName(id string) string { return id }
func (b *tBackend) available(uint32) uint64   { return 1e12 }
func (b *tBackend) reserve(uint32) uint64     { return 0 }

func (b *tBackend) find(id string) (*qsMarket, *qsOrder) {
	for _, m := range b.mkts {
		for _, o := range append(append([]*qsOrder(nil), m.Sells...), m.Buys...) {
			if o.ID == id {
				return m, o
			}
		}
	}
	return nil, nil
}

func (b *tBackend) remove(id string) {
	for _, m := range b.mkts {
		for i, o := range m.Sells {
			if o.ID == id {
				m.Sells = append(m.Sells[:i:i], m.Sells[i+1:]...)
			}
		}
		for i, o := range m.Buys {
			if o.ID == id {
				m.Buys = append(m.Buys[:i:i], m.Buys[i+1:]...)
			}
		}
	}
}

func (b *tBackend) meshTake(market, orderID string, lots uint64) (string, error) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if b.onTake != nil {
		b.onTake(orderID)
	}
	_, o := b.find(orderID)
	if o == nil {
		return "", fmt.Errorf("order is no longer on the book")
	}
	b.n++
	id := fmt.Sprintf("m%d", b.n)
	qty := lots * o.LotSize
	b.matches[id] = &tMatch{order: orderID, status: meshpkg.MatchProposed, qty: qty, quote: qty / tLot * o.Rate}
	b.taken = append(b.taken, orderID)
	return id, nil
}

func (b *tBackend) meshMatch(id string) (string, string, uint64, uint64, bool) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	m := b.matches[id]
	if m == nil {
		return "", "", 0, 0, false
	}
	if m.status == meshpkg.MatchProposed {
		m.polls++
		if m.polls < 2 {
			return m.status, "", m.qty, m.quote, true
		}
		switch b.behave[m.order] {
		case "vanish":
			m.status, m.reason = meshpkg.MatchRejected, "maker: unknown, filled or cancelled order"
			b.remove(m.order)
		case "timeout":
			m.status, m.reason = meshpkg.MatchTimedOut, "no answer from maker"
		default:
			m.status, m.swap = meshpkg.MatchAgreed, meshpkg.SwapNew
			_, o := b.find(m.order)
			if o != nil {
				o.Qty -= min(o.Qty, m.qty)
			}
		}
	}
	return m.status, m.reason, m.qty, m.quote, true
}

func (b *tBackend) meshSwap(id string) (string, bool, bool) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	m := b.matches[id]
	if m == nil || m.swap == "" {
		return "", false, false
	}
	m.polls++
	if m.polls > 3 && m.swap == meshpkg.SwapNew {
		if b.behave[m.order] == "refund" {
			m.swap = meshpkg.SwapRefunded
		} else {
			m.swap = meshpkg.SwapComplete
		}
	}
	return m.swap, m.swap != meshpkg.SwapNew, true
}

func (b *tBackend) serverTrade(*QuickSwapStepPlan, uint64, uint64) (string, error) {
	return "", fmt.Errorf("no server")
}

func (b *tBackend) serverOrder(string) (bool, uint64, uint64, string, error) {
	return false, 0, 0, "", fmt.Errorf("no server")
}

var tTiming = qsTiming{poll: time.Millisecond, proposalTimeout: 50 * time.Millisecond, stepTimeout: 10 * time.Second}

func tRun(t *testing.T, be *tBackend, form *QuickSwapForm) *QuickSwapRun {
	t.Helper()
	qs := &quickSwapper{runs: make(map[string]*qsRun)}
	r, err := qs.start(be, form, tTiming)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	return r.snapshot()
}

func tLogHas(s *QuickSwapRun, sub string) bool {
	for _, l := range s.Log {
		if strings.Contains(l.Msg, sub) {
			return true
		}
	}
	return false
}

func TestQuickSwapDirectRun(t *testing.T) {
	be := &tBackend{behave: map[string]string{}, matches: map[string]*tMatch{},
		mkts: []*qsMarket{tMkt("lbc_dcr", tLBC, tDCR).tSell("a", 1_000_000, 2).tSell("b", 1_003_000, 5)}}
	s := tRun(t, be, &QuickSwapForm{From: tDCR, To: tLBC, Amount: 2_000_000, DriftPct: 1})
	if s.Status != QuickSwapComplete || s.Received != 2*tLot {
		t.Fatalf("status %s received %d: %s", s.Status, s.Received, s.Message)
	}
}

// An order vanishes when taken; the replacement is 0.3% worse, within 1%.
func TestQuickSwapReplaceWithinTolerance(t *testing.T) {
	be := &tBackend{behave: map[string]string{"a": "vanish"}, matches: map[string]*tMatch{},
		mkts: []*qsMarket{tMkt("lbc_dcr", tLBC, tDCR).tSell("a", 1_000_000, 4).tSell("b", 1_003_000, 10)}}
	// 0.0401 DCR: 4 LBC at 0.01 (+ a little spare to pay the replacement).
	s := tRun(t, be, &QuickSwapForm{From: tDCR, To: tLBC, Amount: 4_012_000, DriftPct: 1})
	if s.Status != QuickSwapComplete || s.Received != 4*tLot {
		t.Fatalf("status %s received %d: %s", s.Status, s.Received, s.Message)
	}
	st := s.Steps[0]
	if len(st.Takes) != 2 || st.Takes[0].Status != qsTakeFailed || !st.Takes[1].Replacement || st.Takes[1].OrderID != "b" {
		t.Fatalf("takes %+v", st.Takes)
	}
	if math.Abs(st.Takes[1].WorsePct-0.3) > 1e-6 || !strings.Contains(st.Note, "0.30% worse") {
		t.Fatalf("worse %f note %q", st.Takes[1].WorsePct, st.Note)
	}
	if !tLogHas(s, "vanished") || !tLogHas(s, "Replacement") {
		t.Fatalf("log missing lines: %+v", s.Log)
	}
}

// Timeouts are replaced the same way, but only up to the attempt limit.
func TestQuickSwapReplaceTimeouts(t *testing.T) {
	be := &tBackend{behave: map[string]string{"a": "timeout", "b": "timeout", "c": "timeout", "d": "timeout", "e": "agree"},
		matches: map[string]*tMatch{}, mkts: []*qsMarket{tMkt("lbc_dcr", tLBC, tDCR).
				tSell("a", 1_000_000, 1).tSell("b", 1_000_000, 1).tSell("c", 1_000_000, 1).tSell("d", 1_000_000, 1).tSell("e", 1_000_000, 1)}}
	s := tRun(t, be, &QuickSwapForm{From: tDCR, To: tLBC, Amount: 1_000_000, DriftPct: 1})
	// 1 + 3 replacement rounds: a, b, c, d all time out; e is never tried.
	if s.Status != QuickSwapStopped || s.Received != 0 || len(s.Steps[0].Takes) != 4 {
		t.Fatalf("status %s received %d takes %d: %s", s.Status, s.Received, len(s.Steps[0].Takes), s.Message)
	}
	if len(s.Kept) != 1 || s.Kept[0].Asset != tDCR || s.Kept[0].Amount != 1_000_000 {
		t.Fatalf("kept %+v", s.Kept)
	}
}

// Two hops; in step 2 the order vanishes and the next best is 5% worse, more
// than the 1% tolerance: the run stops and keeps the intermediate coin.
func TestQuickSwapReplaceBeyondTolerance(t *testing.T) {
	be := &tBackend{behave: map[string]string{"q": "vanish"}, matches: map[string]*tMatch{},
		mkts: []*qsMarket{
			tMkt("dcr_btc", tDCR, tBTC).tSell("p", 1_000_000, 10),
			tMkt("lbc_dcr", tLBC, tDCR).tSell("q", 1_000_000, 100).tSell("r", 1_050_000, 100),
		}}
	s := tRun(t, be, &QuickSwapForm{From: tBTC, To: tLBC, Amount: 1_000_000, DriftPct: 1})
	if s.Status != QuickSwapStopped || s.Steps[0].Status != QuickSwapComplete || s.Steps[0].Received != tLot {
		t.Fatalf("status %s step1 %s/%d: %s", s.Status, s.Steps[0].Status, s.Steps[0].Received, s.Message)
	}
	if s.Received != 0 || len(s.Kept) != 1 || s.Kept[0].Asset != tDCR || s.Kept[0].Amount != tLot {
		t.Fatalf("received %d kept %+v", s.Received, s.Kept)
	}
	if !strings.Contains(s.Message, "guaranteed minimum") || !strings.Contains(s.Message, "1 DCR") {
		t.Fatalf("message %q", s.Message)
	}
	for _, tk := range s.Steps[1].Takes {
		if tk.OrderID == "r" {
			t.Fatal("took the order beyond tolerance")
		}
	}
	// Same, but the next order is only 0.5% worse: replaced, completes.
	be = &tBackend{behave: map[string]string{"q": "vanish"}, matches: map[string]*tMatch{},
		mkts: []*qsMarket{
			tMkt("dcr_btc", tDCR, tBTC).tSell("p", 1_000_000, 10),
			tMkt("lbc_dcr", tLBC, tDCR).tSell("q", 1_000_000, 100).tSell("r", 1_005_000, 100),
		}}
	// Step 2 spends 1 DCR: 100 LBC at 0.01, or 99 at 0.01005 (1% less).
	s = tRun(t, be, &QuickSwapForm{From: tBTC, To: tLBC, Amount: 1_000_000, DriftPct: 1})
	if s.Status != QuickSwapComplete || s.Received != 99*tLot || !tLogHas(s, "Replacement") {
		t.Fatalf("status %s received %d: %s", s.Status, s.Received, s.Message)
	}
	if s.Steps[1].Takes[1].OrderID != "r" || math.Abs(s.Steps[1].Takes[1].WorsePct-0.5) > 1e-6 {
		t.Fatalf("replacement %+v", s.Steps[1].Takes[1])
	}
}

// A refunded swap counts as a failed take and is replaced.
func TestQuickSwapRefundReplaced(t *testing.T) {
	be := &tBackend{behave: map[string]string{"a": "refund"}, matches: map[string]*tMatch{},
		mkts: []*qsMarket{tMkt("lbc_dcr", tLBC, tDCR).tSell("a", 1_000_000, 1).tSell("b", 1_000_000, 1)}}
	s := tRun(t, be, &QuickSwapForm{From: tDCR, To: tLBC, Amount: 1_000_000, DriftPct: 1})
	if s.Status != QuickSwapComplete || s.Received != tLot || s.Steps[0].Takes[1].OrderID != "b" {
		t.Fatalf("status %s received %d: %s", s.Status, s.Received, s.Message)
	}
}

// Stop during step 1 of 2: the agreed swap finishes, step 2 never starts, the
// user keeps the intermediate coin.
func TestQuickSwapStop(t *testing.T) {
	be := &tBackend{behave: map[string]string{}, matches: map[string]*tMatch{},
		mkts: []*qsMarket{
			tMkt("dcr_btc", tDCR, tBTC).tSell("p", 1_000_000, 10),
			tMkt("lbc_dcr", tLBC, tDCR).tSell("q", 1_000_000, 100),
		}}
	qs := &quickSwapper{runs: make(map[string]*qsRun)}
	var r *qsRun
	be.onTake = func(id string) {
		if id == "p" {
			go r.requestStop()
		}
	}
	q, _, _ := qsQuote(be, &QuickSwapForm{From: tBTC, To: tLBC, Amount: 2_000_000, DriftPct: 1})
	r = newQSRun(be, q.Route, 1, q.MinOut, tTiming)
	qs.runs[r.st.ID] = r
	go r.run()
	<-r.done
	s := r.snapshot()
	if s.Status != QuickSwapStopped || s.Steps[0].Received != tLot || s.Steps[1].Status != QuickSwapPending {
		t.Fatalf("status %s, step1 %d, step2 %s: %s", s.Status, s.Steps[0].Received, s.Steps[1].Status, s.Message)
	}
	if len(s.Kept) != 1 || s.Kept[0].Asset != tDCR || s.Kept[0].Amount != tLot {
		t.Fatalf("kept %+v", s.Kept)
	}
}

// Start refuses when the books no longer give the minimum the user saw.
func TestQuickSwapStartPricesMoved(t *testing.T) {
	be := &tBackend{behave: map[string]string{}, matches: map[string]*tMatch{},
		mkts: []*qsMarket{tMkt("lbc_dcr", tLBC, tDCR).tSell("a", 1_000_000, 1)}}
	qs := &quickSwapper{runs: make(map[string]*qsRun)}
	if _, err := qs.start(be, &QuickSwapForm{From: tDCR, To: tLBC, Amount: 1_000_000, DriftPct: 1, MinOut: 2 * tLot}, tTiming); err == nil ||
		!strings.Contains(err.Error(), "prices moved") {
		t.Fatalf("want prices moved error, got %v", err)
	}
}

// The last step loses an order and the book has nothing left: the run stops
// below the minimum with what was filled, and keeps the rest of the
// intermediate coin.
func TestQuickSwapNoReplacementLeft(t *testing.T) {
	be := &tBackend{behave: map[string]string{"q": "timeout"}, matches: map[string]*tMatch{},
		mkts: []*qsMarket{
			tMkt("lbc_dcr", tLBC, tDCR).tSell("p", 1_000_000, 10),
			tMkt("lbc_btc", tLBC, tBTC).tBuy("q", 10_030, 4).tBuy("r", 10_000, 1),
		}}
	s := tRun(t, be, &QuickSwapForm{From: tDCR, To: tBTC, Amount: 5_000_000, DriftPct: 1})
	if s.Status != QuickSwapStopped || s.Received != 10_000 || !strings.Contains(s.Message, "no more orders") {
		t.Fatalf("status %s received %d: %s", s.Status, s.Received, s.Message)
	}
	if len(s.Kept) != 1 || s.Kept[0].Asset != tLBC || s.Kept[0].Amount != 4*tLot {
		t.Fatalf("kept %+v", s.Kept)
	}
}
