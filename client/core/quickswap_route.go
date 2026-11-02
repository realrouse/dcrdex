// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

// Quick Swap route finding: swap any coin for any other over the available
// markets (Tatanka mesh and DEX servers), directly or through one
// intermediate coin. Everything in this file is pure (no Core, no network)
// so it can be unit tested.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
)

// Quick Swap market sources.
const (
	QuickSwapMesh   = "mesh"
	QuickSwapServer = "server"
)

// QuickSwapMaxDriftPct is the highest drift tolerance a user may set.
const QuickSwapMaxDriftPct = 10.0

// qsOrder is a book order as Quick Swap sees it. Rate is in the message-rate
// encoding (quote atoms per 1e8 base atoms), Qty is the remaining quantity in
// base atoms, LotSize in base atoms.
type qsOrder struct {
	ID      string `json:"id"`
	Owner   string `json:"owner,omitempty"`
	Rate    uint64 `json:"rate"`
	Qty     uint64 `json:"qty"`
	LotSize uint64 `json:"lotSize"`
	Sell    bool   `json:"sell"`
}

// qsMarket is a snapshot of one market's book. Sells are sorted best (lowest
// rate) first, Buys best (highest rate) first.
type qsMarket struct {
	Source  string
	Host    string // DEX host, or MeshHost for mesh markets
	Name    string // e.g. lbc_dcr
	Base    uint32
	Quote   uint32
	LotSize uint64
	// RateStep is the server market's rate step (0 for mesh markets).
	RateStep uint64
	Sells    []*qsOrder
	Buys     []*qsOrder
}

// MeshHost is the host name the UI uses for mesh markets.
const MeshHost = "tatanka-mesh"

func (m *qsMarket) key() string { return m.Source + "|" + m.Host + "|" + m.Name }

func (m *qsMarket) has(a uint32) bool { return m.Base == a || m.Quote == a }

func (m *qsMarket) other(a uint32) uint32 {
	if m.Base == a {
		return m.Quote
	}
	return m.Base
}

func (m *qsMarket) sortBooks() {
	sort.SliceStable(m.Sells, func(i, j int) bool { return m.Sells[i].Rate < m.Sells[j].Rate })
	sort.SliceStable(m.Buys, func(i, j int) bool { return m.Buys[i].Rate > m.Buys[j].Rate })
}

// QuickSwapFill is one order a step takes: lots of the maker's order at its
// rate. In is what we send, Out what we receive (atoms).
type QuickSwapFill struct {
	OrderID string `json:"orderID"`
	Owner   string `json:"owner,omitempty"`
	Rate    uint64 `json:"rate"`
	Lots    uint64 `json:"lots"`
	LotSize uint64 `json:"lotSize"`
	Qty     uint64 `json:"qty"` // base atoms
	In      uint64 `json:"in"`
	Out     uint64 `json:"out"`
}

// QuickSwapStepPlan is one swap of a route: send FromAsset, receive ToAsset
// on one market. Sell is true when we sell the market's base asset.
type QuickSwapStepPlan struct {
	Source      string           `json:"source"`
	Host        string           `json:"host"`
	Market      string           `json:"market"`
	Base        uint32           `json:"base"`
	Quote       uint32           `json:"quote"`
	FromAsset   uint32           `json:"fromAsset"`
	ToAsset     uint32           `json:"toAsset"`
	FromSymbol  string           `json:"fromSymbol"`
	ToSymbol    string           `json:"toSymbol"`
	Sell        bool             `json:"sell"`
	LotSize     uint64           `json:"lotSize"`
	In          uint64           `json:"in"`
	Out         uint64           `json:"out"`
	AvgRate     uint64           `json:"avgRate"`
	WorstRate   uint64           `json:"worstRate"`
	BookOrders  int              `json:"bookOrders"`
	Fills       []*QuickSwapFill `json:"fills"`
	marketShape *qsMarket
}

// QuickSwapRoute is a route with its expected amounts. Amount is what the
// user offered; In is what the first step spends (whole lots only), Out what
// the last step delivers. Leftover is the unspent intermediate coin.
type QuickSwapRoute struct {
	From       uint32               `json:"from"`
	To         uint32               `json:"to"`
	FromSymbol string               `json:"fromSymbol"`
	ToSymbol   string               `json:"toSymbol"`
	Amount     uint64               `json:"amount"`
	In         uint64               `json:"in"`
	Out        uint64               `json:"out"`
	Leftover   uint64               `json:"leftover"`
	Steps      []*QuickSwapStepPlan `json:"steps"`
	Label      string               `json:"label"`
}

// Hops is the number of swaps.
func (r *QuickSwapRoute) Hops() int { return len(r.Steps) }

// qsWalk walks a market's book for a step from asset `from`, spending at most
// budget atoms of it, skipping excluded order IDs and orders of self. It
// returns the fills, best first. Only whole lots of each order are taken.
func qsWalk(m *qsMarket, from uint32, budget uint64, exclude map[string]bool, self string) (fills []*QuickSwapFill, in, out uint64) {
	sell := from == m.Base
	book := m.Sells // we buy base: take sells
	if sell {
		book = m.Buys // we sell base: take buys
	}
	left := budget
	for _, o := range book {
		if exclude[o.ID] || (self != "" && o.Owner == self) || o.Sell == sell {
			continue
		}
		lot := o.LotSize
		if lot == 0 {
			lot = m.LotSize
		}
		if lot == 0 || o.Rate == 0 {
			continue
		}
		avail := o.Qty / lot
		var lots, fin, fout uint64
		if sell {
			lots = min(avail, left/lot)
			fin = lots * lot
			fout = calc.BaseToQuote(o.Rate, fin)
		} else {
			lotCost := calc.BaseToQuote(o.Rate, lot)
			if lotCost == 0 {
				continue
			}
			lots = min(avail, left/lotCost)
			fin = lots * lotCost
			fout = lots * lot
		}
		if lots == 0 {
			// Nothing left on this order, or one of its lots is more than
			// what's left. Worse orders can have smaller lots, so go on.
			continue
		}
		fills = append(fills, &QuickSwapFill{OrderID: o.ID, Owner: o.Owner, Rate: o.Rate, Lots: lots, LotSize: lot,
			Qty: lots * lot, In: fin, Out: fout})
		in += fin
		out += fout
		left -= fin
		if left == 0 {
			break
		}
	}
	return fills, in, out
}

func qsStep(m *qsMarket, from uint32, budget uint64, exclude map[string]bool, self string) *QuickSwapStepPlan {
	fills, in, out := qsWalk(m, from, budget, exclude, self)
	to := m.other(from)
	st := &QuickSwapStepPlan{Source: m.Source, Host: m.Host, Market: m.Name, Base: m.Base, Quote: m.Quote,
		FromAsset: from, ToAsset: to, FromSymbol: dex.BipIDSymbol(from), ToSymbol: dex.BipIDSymbol(to),
		Sell: from == m.Base, LotSize: m.LotSize, In: in, Out: out, Fills: fills, marketShape: m}
	if st.Sell {
		st.BookOrders = len(m.Buys)
	} else {
		st.BookOrders = len(m.Sells)
	}
	var qty, quote uint64
	for _, f := range fills {
		qty += f.Qty
		if st.Sell {
			quote += f.Out
		} else {
			quote += f.In
		}
		if st.WorstRate == 0 || (st.Sell && f.Rate < st.WorstRate) || (!st.Sell && f.Rate > st.WorstRate) {
			st.WorstRate = f.Rate
		}
	}
	if qty > 0 {
		st.AvgRate = calc.BaseQuoteToRate(qty, quote)
	}
	return st
}

func qsLabel(steps []*QuickSwapStepPlan) string {
	if len(steps) == 0 {
		return ""
	}
	s := strings.ToUpper(steps[0].FromSymbol)
	for _, st := range steps {
		s += " → " + strings.ToUpper(st.ToSymbol)
	}
	s += " ("
	for i, st := range steps {
		if i > 0 {
			s += ", "
		}
		s += st.Source
	}
	return s + ")"
}

// qsFindRoutes returns every direct and two-hop route from `from` to `to`
// for amount atoms, best (highest output) first; ties go to fewer hops, then
// to the route that spends less. Routes that deliver nothing are dropped.
// reserve(asset) is kept back from the intermediate amount before the second
// step, for the second swap's network fees (nil for none).
func qsFindRoutes(mkts []*qsMarket, from, to uint32, amount uint64, self string, reserve func(asset uint32) uint64) []*QuickSwapRoute {
	var routes []*QuickSwapRoute
	add := func(steps ...*QuickSwapStepPlan) {
		last := steps[len(steps)-1]
		if last.Out == 0 {
			return
		}
		r := &QuickSwapRoute{From: from, To: to, FromSymbol: dex.BipIDSymbol(from), ToSymbol: dex.BipIDSymbol(to),
			Amount: amount, In: steps[0].In, Out: last.Out, Steps: steps, Label: qsLabel(steps)}
		if len(steps) == 2 {
			r.Leftover = steps[0].Out - steps[1].In
		}
		routes = append(routes, r)
	}
	for _, m := range mkts {
		if m.has(from) && m.has(to) {
			if st := qsStep(m, from, amount, nil, self); st.Out > 0 {
				add(st)
			}
		}
	}
	for _, m1 := range mkts {
		if !m1.has(from) || m1.has(to) {
			continue
		}
		mid := m1.other(from)
		st1 := qsStep(m1, from, amount, nil, self)
		if st1.Out == 0 {
			continue
		}
		budget2 := st1.Out
		if reserve != nil {
			if r := reserve(mid); r < budget2 {
				budget2 -= r
			} else {
				budget2 = 0
			}
		}
		for _, m2 := range mkts {
			if m2 == m1 || !m2.has(mid) || !m2.has(to) {
				continue
			}
			st2 := qsStep(m2, mid, budget2, nil, self)
			if st2.Out == 0 {
				continue
			}
			// Step 1 only needs to buy what step 2 can use. Shrinking it
			// keeps less of the intermediate coin and spends less.
			st1b := st1
			if st2.In < st1.Out {
				st1b = qsShrinkStep1(m1, from, st2.In+(st1.Out-budget2), self, st1)
			}
			add(st1b, st2)
		}
	}
	sort.SliceStable(routes, func(i, j int) bool {
		a, b := routes[i], routes[j]
		if a.Out != b.Out {
			return a.Out > b.Out
		}
		if a.Hops() != b.Hops() {
			return a.Hops() < b.Hops()
		}
		return a.In < b.In
	})
	return routes
}

// qsShrinkStep1 finds the smallest step-1 spend that still delivers at least
// need atoms of the intermediate coin, using the same book walk. It returns
// full if no smaller spend works.
func qsShrinkStep1(m *qsMarket, from uint32, need uint64, self string, full *QuickSwapStepPlan) *QuickSwapStepPlan {
	if need >= full.Out {
		return full
	}
	lo, hi := uint64(0), full.In
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		if st := qsStep(m, from, mid, nil, self); st.Out >= need {
			hi = mid
		} else {
			lo = mid
		}
	}
	st := qsStep(m, from, hi, nil, self)
	if st.Out < need {
		return full
	}
	return st
}

// qsMinOut is the guaranteed minimum for an expected output and a drift
// tolerance in percent.
func qsMinOut(expected uint64, driftPct float64) uint64 {
	if driftPct < 0 {
		driftPct = 0
	}
	return uint64(math.Floor(float64(expected) * (1 - driftPct/100)))
}

func qsCheckDrift(driftPct float64) error {
	if math.IsNaN(driftPct) || driftPct < 0 || driftPct > QuickSwapMaxDriftPct {
		return fmt.Errorf("drift tolerance must be between 0 and %.0f%%", QuickSwapMaxDriftPct)
	}
	return nil
}

// qsRateWorsePct is how much worse rate b is than rate a for a step, in
// percent (positive is worse). Buying base, a higher rate is worse; selling
// base, a lower one.
func qsRateWorsePct(sell bool, a, b uint64) float64 {
	if a == 0 {
		return 0
	}
	if sell {
		return (float64(a) - float64(b)) / float64(a) * 100
	}
	return (float64(b) - float64(a)) / float64(a) * 100
}

// qsTrim keeps the fills (best first) needed to deliver need atoms, cutting
// the last one to the fewest lots that reach it.
func qsTrim(fills []*QuickSwapFill, sell bool, need uint64) (kept []*QuickSwapFill, in, out uint64) {
	for _, f := range fills {
		if out >= need {
			break
		}
		perLot := f.LotSize // buying base: each lot delivers a lot
		if sell {
			perLot = calc.BaseToQuote(f.Rate, f.LotSize)
		}
		if perLot == 0 {
			continue
		}
		lots := min(f.Lots, (need-out+perLot-1)/perLot)
		g := *f
		g.Lots, g.Qty = lots, lots*f.LotSize
		if sell {
			g.In, g.Out = g.Qty, calc.BaseToQuote(f.Rate, g.Qty)
		} else {
			g.In, g.Out = calc.BaseToQuote(f.Rate, f.LotSize)*lots, g.Qty
		}
		kept = append(kept, &g)
		in += g.In
		out += g.Out
	}
	return kept, in, out
}

// qsProject estimates the route's final output when step idx delivers
// idxOut atoms in total and the later steps run on the current books. It is
// the drift check's input.
func qsProject(steps []*QuickSwapStepPlan, mkts map[string]*qsMarket, idx int, idxOut uint64, exclude map[string]bool, self string, reserve func(uint32) uint64) uint64 {
	out := idxOut
	for i := idx + 1; i < len(steps); i++ {
		m := mkts[steps[i].marketShape.key()]
		if m == nil {
			return 0
		}
		b := out
		if reserve != nil {
			if r := reserve(steps[i].FromAsset); r < b {
				b -= r
			} else {
				b = 0
			}
		}
		fills, _, _ := qsWalk(m, steps[i].FromAsset, b, exclude, self)
		_, _, out = qsTrim(fills, steps[i].Sell, steps[i].Out)
	}
	return out
}

var errNoRoute = errors.New("no route: no market or orders connect these coins (at most 2 swaps)")
