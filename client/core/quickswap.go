// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

// Quick Swap execution. A run executes its route's steps in turn. A mesh step
// takes the planned book orders with the normal mesh take flow (one proposal
// per order, then the atomic swap). If a take fails (the order vanished, the
// maker didn't answer, the swap was refunded), the step re-walks the current
// book and takes the next best orders, but only while the projected result of
// the whole route stays at or above the guaranteed minimum (expected output
// minus the user's drift tolerance). Otherwise the run stops and the user
// keeps what they have (the intermediate coin after step 1). A server step
// places an immediate limit order at the worst rate the tolerance allows.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/comms"
	meshpkg "decred.org/dcrdex/client/mesh"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
)

// Run and step states.
const (
	QuickSwapRunning  = "running"
	QuickSwapComplete = "complete"
	QuickSwapStopped  = "stopped"
	QuickSwapFailed   = "failed"
	QuickSwapPending  = "pending"
)

// Take states.
const (
	qsTakeProposing = "proposing"
	qsTakeSwapping  = "swapping"
	qsTakeComplete  = "complete"
	qsTakeFailed    = "failed"
)

// QuickSwapMaxReplacements is how many times a step re-walks the book to
// replace failed takes.
const QuickSwapMaxReplacements = 3

// QuickSwapForm is a quote or start request. Amount is in atoms of From.
// MinOut (start only) is the guaranteed minimum the user was shown; the run
// refuses to start if the current books can't deliver it.
type QuickSwapForm struct {
	From     uint32  `json:"from"`
	To       uint32  `json:"to"`
	Amount   uint64  `json:"amount"`
	DriftPct float64 `json:"driftPct"`
	MinOut   uint64  `json:"minOut"`
}

// QuickSwapQuote is the best route and the others considered.
type QuickSwapQuote struct {
	Route    *QuickSwapRoute   `json:"route"`
	Others   []*QuickSwapRoute `json:"others"`
	Routes   int               `json:"routes"`
	DriftPct float64           `json:"driftPct"`
	MinOut   uint64            `json:"minOut"`
	Markets  []string          `json:"markets"`
}

// QuickSwapTake is one order taken (or tried) by a step.
type QuickSwapTake struct {
	OrderID     string  `json:"orderID"`
	Owner       string  `json:"owner,omitempty"`
	OwnerName   string  `json:"ownerName,omitempty"`
	MatchID     string  `json:"matchID,omitempty"`
	Rate        uint64  `json:"rate"`
	Lots        uint64  `json:"lots"`
	Qty         uint64  `json:"qty"`
	In          uint64  `json:"in"`
	Out         uint64  `json:"out"`
	Status      string  `json:"status"`
	SwapStatus  string  `json:"swapStatus,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	Replacement bool    `json:"replacement"`
	WorsePct    float64 `json:"worsePct"`
	Started     int64   `json:"started"`
	Deadline    int64   `json:"deadline,omitempty"`
}

// QuickSwapWait is something the step is waiting for, with a deadline for a
// countdown (0: none).
type QuickSwapWait struct {
	Until int64  `json:"until"`
	Msg   string `json:"msg"`
}

// QuickSwapStepRun is a step's live state.
type QuickSwapStepRun struct {
	Plan        *QuickSwapStepPlan `json:"plan"`
	Status      string             `json:"status"`
	Budget      uint64             `json:"budget"`
	Target      uint64             `json:"target"`
	Spent       uint64             `json:"spent"`
	Received    uint64             `json:"received"`
	Committed   uint64             `json:"committed"`
	Takes       []*QuickSwapTake   `json:"takes"`
	Round       int                `json:"round"`
	MaxRounds   int                `json:"maxRounds"`
	Wait        *QuickSwapWait     `json:"wait,omitempty"`
	Note        string             `json:"note,omitempty"`
	WorsePct    float64            `json:"worsePct"`
	ServerOrder string             `json:"serverOrder,omitempty"`
	Started     int64              `json:"started,omitempty"`
	Ended       int64              `json:"ended,omitempty"`
}

// QuickSwapLog is an activity line.
type QuickSwapLog struct {
	Time int64  `json:"time"`
	Msg  string `json:"msg"`
	Kind string `json:"kind,omitempty"` // "", good, warn, bad
}

// QuickSwapKept is an amount the user keeps when a run stops early.
type QuickSwapKept struct {
	Asset  uint32 `json:"asset"`
	Symbol string `json:"symbol"`
	Amount uint64 `json:"amount"`
}

// QuickSwapRun is a Quick Swap's state for the UI.
type QuickSwapRun struct {
	ID            string              `json:"id"`
	Status        string              `json:"status"`
	Message       string              `json:"message,omitempty"`
	Started       int64               `json:"started"`
	Ended         int64               `json:"ended,omitempty"`
	From          uint32              `json:"from"`
	To            uint32              `json:"to"`
	FromSymbol    string              `json:"fromSymbol"`
	ToSymbol      string              `json:"toSymbol"`
	Amount        uint64              `json:"amount"`
	DriftPct      float64             `json:"driftPct"`
	Expected      uint64              `json:"expected"`
	MinOut        uint64              `json:"minOut"`
	Projected     uint64              `json:"projected"`
	DriftUsedPct  float64             `json:"driftUsedPct"`
	Received      uint64              `json:"received"`
	Route         *QuickSwapRoute     `json:"route"`
	Step          int                 `json:"step"`
	Steps         []*QuickSwapStepRun `json:"steps"`
	Kept          []*QuickSwapKept    `json:"kept,omitempty"`
	StopRequested bool                `json:"stopRequested"`
	Log           []*QuickSwapLog     `json:"log"`
}

// qsBackend is what the executor needs from Core (faked in tests).
type qsBackend interface {
	// markets returns fresh book snapshots of every usable market.
	markets() ([]*qsMarket, error)
	self() string
	peerName(peerID string) string
	// meshTake proposes lots of a mesh book order, returning the match ID.
	meshTake(market, orderID string, lots uint64) (string, error)
	// meshMatch is a match's negotiation status.
	meshMatch(matchID string) (status, reason string, qty, quoteQty uint64, ok bool)
	// meshSwap is a swap's status, and whether it is final.
	meshSwap(matchID string) (status string, final, ok bool)
	// serverTrade places an immediate limit order, returning the order ID.
	serverTrade(st *QuickSwapStepPlan, qty, rate uint64) (string, error)
	// serverOrder reports a server order: done when it no longer trades
	// and all its swaps are final; base/quote are the completed amounts.
	serverOrder(oid string) (done bool, base, quote uint64, status string, err error)
	available(assetID uint32) uint64
	reserve(assetID uint32) uint64
}

// qsTiming are the executor's poll and timeout settings.
type qsTiming struct {
	poll            time.Duration
	proposalTimeout time.Duration
	stepTimeout     time.Duration
}

var qsDefaultTiming = qsTiming{poll: 700 * time.Millisecond, proposalTimeout: meshpkg.DefaultNegotiationConfig.ProposalTimeout,
	stepTimeout: 2 * time.Hour}

// quickSwapper keeps the runs.
type quickSwapper struct {
	mtx  sync.Mutex
	runs map[string]*qsRun
}

type qsRun struct {
	mtx     sync.Mutex
	st      *QuickSwapRun
	be      qsBackend
	timing  qsTiming
	now     func() time.Time
	stopped chan struct{}
	stopOne sync.Once
	done    chan struct{}
	// reserve is the quote's fee reserve for the drift projections.
	reserve func(uint32) uint64
}

func (r *qsRun) ms() int64 { return r.now().UnixMilli() }

func (r *qsRun) logf(kind, format string, args ...any) {
	r.st.Log = append(r.st.Log, &QuickSwapLog{Time: r.ms(), Msg: fmt.Sprintf(format, args...), Kind: kind})
}

// snapshot is a deep copy for the UI.
func (r *qsRun) snapshot() *QuickSwapRun {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	b, _ := json.Marshal(r.st)
	var c QuickSwapRun
	_ = json.Unmarshal(b, &c)
	// Newest activity first.
	for i, j := 0, len(c.Log)-1; i < j; i, j = i+1, j-1 {
		c.Log[i], c.Log[j] = c.Log[j], c.Log[i]
	}
	return &c
}

func (r *qsRun) stopRequested() bool {
	select {
	case <-r.stopped:
		return true
	default:
		return false
	}
}

func (r *qsRun) requestStop() {
	r.stopOne.Do(func() {
		r.mtx.Lock()
		r.st.StopRequested = true
		r.logf("warn", "Stop requested: no new orders will be taken; swaps already agreed finish (they are atomic)")
		r.mtx.Unlock()
		close(r.stopped)
	})
}

func (r *qsRun) sleep() bool {
	select {
	case <-time.After(r.timing.poll):
		return true
	case <-r.stopped:
		// Still sleep the poll interval so in-flight swaps are tracked.
		time.Sleep(r.timing.poll)
		return true
	}
}

func amtStr(assetID uint32, v uint64) string {
	ui, err := asset.UnitInfo(assetID)
	if err != nil || ui.Conventional.ConversionFactor == 0 || ui.Conventional.Unit == "" {
		return fmt.Sprintf("%s %s", strconv.FormatFloat(float64(v)/1e8, 'f', -1, 64), strings.ToUpper(dex.BipIDSymbol(assetID)))
	}
	return strconv.FormatFloat(float64(v)/float64(ui.Conventional.ConversionFactor), 'f', -1, 64) + " " + ui.Conventional.Unit
}

func rateStr(base, quote uint32, rate uint64) string {
	bui, err1 := asset.UnitInfo(base)
	qui, err2 := asset.UnitInfo(quote)
	if err1 != nil || err2 != nil || bui.Conventional.ConversionFactor == 0 || qui.Conventional.ConversionFactor == 0 ||
		bui.Conventional.Unit == "" || qui.Conventional.Unit == "" {
		return fmt.Sprintf("%.8g %s/%s", float64(rate)/1e8, strings.ToUpper(dex.BipIDSymbol(quote)), strings.ToUpper(dex.BipIDSymbol(base)))
	}
	conv := calc.ConventionalRate(rate, bui, qui)
	return fmt.Sprintf("%.8g %s/%s", conv, qui.Conventional.Unit, bui.Conventional.Unit)
}

// newQSRun builds a run for a route. MinOut must already be set.
func newQSRun(be qsBackend, route *QuickSwapRoute, driftPct float64, minOut uint64, timing qsTiming) *qsRun {
	st := &QuickSwapRun{
		ID: hex.EncodeToString(encode.RandomBytes(8)), Status: QuickSwapRunning,
		From: route.From, To: route.To, FromSymbol: route.FromSymbol, ToSymbol: route.ToSymbol,
		Amount: route.Amount, DriftPct: driftPct, Expected: route.Out, MinOut: minOut, Projected: route.Out,
		Route: route, Log: []*QuickSwapLog{},
	}
	for _, p := range route.Steps {
		st.Steps = append(st.Steps, &QuickSwapStepRun{Plan: p, Status: QuickSwapPending, Takes: []*QuickSwapTake{},
			MaxRounds: 1 + QuickSwapMaxReplacements})
	}
	r := &qsRun{st: st, be: be, timing: timing, now: time.Now, stopped: make(chan struct{}), done: make(chan struct{}),
		reserve: qsQuoteReserve(be)}
	st.Started = r.ms()
	return r
}

func (r *qsRun) finish(status, kind, format string, args ...any) {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	r.st.Status = status
	r.st.Message = fmt.Sprintf(format, args...)
	r.st.Ended = r.ms()
	r.logf(kind, "%s", r.st.Message)
}

// run executes the route. It returns when the run is final.
func (r *qsRun) run() {
	defer close(r.done)
	r.mtx.Lock()
	r.logf("", "Route chosen: %s, expected %s, at least %s (%.2g%% tolerance)", r.st.Route.Label,
		amtStr(r.st.To, r.st.Expected), amtStr(r.st.To, r.st.MinOut), r.st.DriftPct)
	// Step 1 may spend up to what the user offered (replacements can cost
	// more than the planned orders), but only to deliver its planned output.
	budget := r.st.Route.Amount
	r.mtx.Unlock()
	nSteps := len(r.st.Steps)
	for i := 0; i < nSteps; i++ {
		r.mtx.Lock()
		r.st.Step = i
		step := r.st.Steps[i]
		if i > 0 {
			// The second step spends what the first one delivered, keeping
			// some for network fees if the wallet has nothing extra.
			b := r.st.Steps[i-1].Received
			res := r.be.reserve(step.Plan.FromAsset)
			if avail := r.be.available(step.Plan.FromAsset); avail < b+res {
				if avail > res {
					b = avail - res
				} else {
					b = 0
				}
			}
			budget = b
		}
		step.Budget = budget
		step.Target = step.Plan.Out
		step.Status = QuickSwapRunning
		step.Started = r.ms()
		if step.Plan.Sell {
			r.logf("", "Step %d started: sell %s for %s on %s", i+1, strings.ToUpper(step.Plan.FromSymbol),
				strings.ToUpper(step.Plan.ToSymbol), qsVenue(step.Plan))
		} else {
			r.logf("", "Step %d started: buy %s with %s on %s", i+1, strings.ToUpper(step.Plan.ToSymbol),
				strings.ToUpper(step.Plan.FromSymbol), qsVenue(step.Plan))
		}
		r.mtx.Unlock()

		var stopMsg string
		if step.Plan.Source == QuickSwapServer {
			stopMsg = r.runServerStep(i)
		} else {
			stopMsg = r.runMeshStep(i)
		}

		r.mtx.Lock()
		step.Ended = r.ms()
		got := step.Received
		final := i == nSteps-1
		if got == 0 {
			step.Status = QuickSwapStopped
			if stopMsg == "" {
				stopMsg = "no order could be completed"
			}
		} else if stopMsg != "" {
			step.Status = QuickSwapStopped
		} else {
			step.Status = QuickSwapComplete
			r.logf("good", "Step %d complete: %s received", i+1, amtStr(step.Plan.ToAsset, got))
		}
		r.mtx.Unlock()

		if stopMsg != "" || got == 0 {
			r.mtx.Lock()
			r.keepLocked(i)
			kept := r.keptStrLocked()
			r.mtx.Unlock()
			if final && got > 0 {
				r.mtx.Lock()
				r.st.Received = got
				r.mtx.Unlock()
				r.finish(QuickSwapStopped, "warn", "Stopped in step %d: %s. Received %s; you keep %s", i+1, stopMsg,
					amtStr(step.Plan.ToAsset, got), kept)
			} else {
				r.finish(QuickSwapStopped, "warn", "Stopped in step %d: %s. Nothing is lost: you keep %s", i+1, stopMsg, kept)
			}
			return
		}
		if final {
			r.mtx.Lock()
			r.st.Received = got
			r.st.Projected = got
			r.keepLocked(-1)
			r.mtx.Unlock()
			r.finish(QuickSwapComplete, "good", "Swap complete: received %s (guaranteed at least %s)",
				amtStr(r.st.To, got), amtStr(r.st.To, r.st.MinOut))
			return
		}
		if r.stopRequested() {
			r.mtx.Lock()
			r.keepLocked(i + 1)
			kept := r.keptStrLocked()
			r.mtx.Unlock()
			r.finish(QuickSwapStopped, "warn", "Stopped after step %d as requested: you keep %s", i+1, kept)
			return
		}
	}
}

// keepLocked records what the user keeps of the coins in the route when the
// run ends in (or after) step stopIdx: the unspent first-step coin if it
// stopped in step 1, and any intermediate coin a later step didn't spend.
// stopIdx -1 is a completed run (only intermediate leftovers).
func (r *qsRun) keepLocked(stopIdx int) {
	r.st.Kept = nil
	add := func(a uint32, v uint64) {
		if v == 0 {
			return
		}
		for _, k := range r.st.Kept {
			if k.Asset == a {
				k.Amount += v
				return
			}
		}
		r.st.Kept = append(r.st.Kept, &QuickSwapKept{Asset: a, Symbol: dex.BipIDSymbol(a), Amount: v})
	}
	steps := r.st.Steps
	if stopIdx == 0 {
		add(steps[0].Plan.FromAsset, steps[0].Budget-min(steps[0].Spent, steps[0].Budget))
	}
	for i := 1; i < len(steps); i++ {
		prev := steps[i-1].Received
		add(steps[i].Plan.FromAsset, prev-min(steps[i].Spent, prev))
	}
}

func (r *qsRun) keptStrLocked() string {
	if len(r.st.Kept) == 0 {
		return "your coins (nothing was spent)"
	}
	s := ""
	for i, k := range r.st.Kept {
		if i > 0 {
			s += " and "
		}
		s += amtStr(k.Asset, k.Amount)
	}
	return s
}

func qsVenue(p *QuickSwapStepPlan) string {
	if p.Source == QuickSwapMesh {
		return "the Tatanka mesh (" + p.Market + ")"
	}
	return p.Host + " (" + p.Market + ")"
}

// marketsByKey snapshots the books.
func (r *qsRun) marketsByKey() (map[string]*qsMarket, error) {
	mkts, err := r.be.markets()
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*qsMarket, len(mkts))
	for _, m := range mkts {
		byKey[m.key()] = m
	}
	return byKey, nil
}

// project updates the projected final output and drift used.
func (r *qsRun) setProjectionLocked(p uint64) {
	r.st.Projected = p
	if r.st.Expected > 0 && p < r.st.Expected {
		r.st.DriftUsedPct = float64(r.st.Expected-p) / float64(r.st.Expected) * 100
	} else {
		r.st.DriftUsedPct = 0
	}
}

// runMeshStep runs step idx on the mesh. It returns a non-empty reason if
// the run must stop after this step (drift limit, user stop, nothing left).
func (r *qsRun) runMeshStep(idx int) string {
	r.mtx.Lock()
	step := r.st.Steps[idx]
	steps := r.st.Route.Steps
	r.mtx.Unlock()
	self := r.be.self()
	exclude := make(map[string]bool)
	needRound := true
	var failedRates []uint64 // rates of takes that failed since the last round
	var stopMsg string
	deadline := r.now().Add(r.timing.stepTimeout)

	for {
		// Update the takes.
		r.mtx.Lock()
		proposing, swapping := 0, 0
		var waitTake *QuickSwapTake
		for _, t := range step.Takes {
			switch t.Status {
			case qsTakeProposing:
				status, reason, qty, quoteQty, ok := r.be.meshMatch(t.MatchID)
				switch {
				case !ok:
					// Not known yet; keep waiting until the deadline.
				case status == meshpkg.MatchAgreed:
					// The maker may accept less than proposed.
					if qty > 0 && qty != t.Qty {
						lot := max(t.Qty/max(t.Lots, 1), 1)
						t.Qty, t.Lots = qty, qty/lot
						if step.Plan.Sell {
							t.In, t.Out = qty, quoteQty
						} else {
							t.In, t.Out = quoteQty, qty
						}
					}
					t.Status = qsTakeSwapping
					t.SwapStatus = meshpkg.SwapNew
					step.Committed += t.In
					r.logf("", "Order %s matched: %s @ %s with %s; swapping", short(t.OrderID), amtStr(step.Plan.Base, t.Qty),
						rateStr(step.Plan.Base, step.Plan.Quote, t.Rate), t.OwnerName)
				case status == meshpkg.MatchRejected || status == meshpkg.MatchTimedOut:
					t.Status, t.Reason = qsTakeFailed, reason
					exclude[t.OrderID] = true
					failedRates = append(failedRates, t.Rate)
					needRound = true
					r.logf("warn", "Order %s (%s @ %s, %s) %s: %s", short(t.OrderID), amtStr(step.Plan.Base, t.Qty),
						rateStr(step.Plan.Base, step.Plan.Quote, t.Rate), t.OwnerName, qsFailWord(status, reason), reason)
				}
				if t.Status == qsTakeProposing && t.Deadline > 0 && r.ms() > t.Deadline+5000 {
					// The match never reached a final state; give up on it.
					t.Status, t.Reason = qsTakeFailed, "no answer"
					exclude[t.OrderID] = true
					failedRates = append(failedRates, t.Rate)
					needRound = true
				}
			case qsTakeSwapping:
				status, final, ok := r.be.meshSwap(t.MatchID)
				if !ok {
					break
				}
				t.SwapStatus = status
				if !final {
					break
				}
				step.Committed -= t.In
				if status == meshpkg.SwapComplete {
					t.Status = qsTakeComplete
					step.Spent += t.In
					step.Received += t.Out
					r.logf("good", "Swap with %s complete: received %s", t.OwnerName, amtStr(step.Plan.ToAsset, t.Out))
				} else {
					t.Status, t.Reason = qsTakeFailed, "swap "+status
					exclude[t.OrderID] = true
					failedRates = append(failedRates, t.Rate)
					needRound = true
					r.logf("bad", "Swap with %s ended %s; the coins are back in your wallet", t.OwnerName, status)
				}
			}
			switch t.Status {
			case qsTakeProposing:
				proposing++
				if waitTake == nil || t.Deadline < waitTake.Deadline {
					waitTake = t
				}
			case qsTakeSwapping:
				swapping++
				if waitTake == nil {
					waitTake = t
				}
			}
		}
		// What the user sees as the current wait.
		step.Wait = nil
		if waitTake != nil {
			if waitTake.Status == qsTakeProposing {
				step.Wait = &QuickSwapWait{Until: waitTake.Deadline,
					Msg: fmt.Sprintf("Waiting for %s to accept order %s. If they don't answer, Quick Swap retries with the next best order (attempt %d of %d).",
						waitTake.OwnerName, short(waitTake.OrderID), step.Round, step.MaxRounds)}
			} else {
				step.Wait = &QuickSwapWait{Msg: fmt.Sprintf("Atomic swap with %s: %s", waitTake.OwnerName, qsSwapWords(waitTake.SwapStatus))}
			}
		}
		r.mtx.Unlock()

		if r.stopRequested() && needRound && stopMsg == "" {
			stopMsg = "stopped by you"
			needRound = false
		}
		if needRound && proposing == 0 && stopMsg == "" {
			needRound = false
			if msg := r.meshRound(idx, steps, exclude, self, failedRates); msg != "" {
				stopMsg = msg
			}
			failedRates = nil
			continue
		}
		if proposing == 0 && swapping == 0 && !needRound {
			break
		}
		if r.now().After(deadline) {
			// Swaps keep running in the mesh swapper; Quick Swap just
			// stops waiting for them.
			if stopMsg == "" {
				stopMsg = "the step took too long; check the Mesh tab for its swaps"
			}
			break
		}
		r.sleep()
	}
	r.mtx.Lock()
	step.Wait = nil
	if step.Received > 0 && step.Spent > 0 && step.Plan.AvgRate > 0 {
		var qty, quote uint64
		for _, t := range step.Takes {
			if t.Status == qsTakeComplete {
				qty += t.Qty
				if step.Plan.Sell {
					quote += t.Out
				} else {
					quote += t.In
				}
			}
		}
		if qty > 0 {
			step.WorsePct = qsRateWorsePct(step.Plan.Sell, step.Plan.AvgRate, calc.BaseQuoteToRate(qty, quote))
		}
	}
	r.mtx.Unlock()
	return stopMsg
}

func qsFailWord(status, reason string) string {
	switch {
	case status == meshpkg.MatchTimedOut:
		return "timed out"
	case meshpkg.OrderGoneReason(reason):
		return "vanished"
	}
	return "failed"
}

// qsSwapWords describes a taker's swap status.
func qsSwapWords(s string) string {
	switch s {
	case meshpkg.SwapNew, "":
		return "waiting for the maker to lock their coins"
	case meshpkg.SwapInitAudited, meshpkg.SwapPartPending:
		return "maker's coins locked and checked; locking ours"
	case meshpkg.SwapPartSent:
		return "both sides locked; waiting for the maker to redeem, then we redeem"
	case meshpkg.SwapComplete:
		return "complete"
	case meshpkg.SwapRefunded:
		return "refunded"
	}
	return s
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// meshRound walks the current book for what is left of step idx's budget and
// proposes the fills, after checking that the route's projected output stays
// within the tolerance. It returns a stop reason, if any.
func (r *qsRun) meshRound(idx int, steps []*QuickSwapStepPlan, exclude map[string]bool, self string, failedRates []uint64) string {
	mkts, err := r.marketsByKey()
	r.mtx.Lock()
	defer r.mtx.Unlock()
	step := r.st.Steps[idx]
	if err != nil {
		r.logf("bad", "Can't read the order books: %v", err)
		return "can't read the order books"
	}
	final := idx == len(steps)-1
	// partial ends the step with what it has. The last step must still
	// reach the guaranteed minimum; an earlier one goes on, and the next
	// step's drift check decides.
	partial := func(why string, got uint64) string {
		if final && got < r.st.MinOut {
			return fmt.Sprintf("%s, so only %s of the guaranteed %s could be filled", why, amtStr(r.st.To, got), amtStr(r.st.To, r.st.MinOut))
		}
		r.logf("warn", "Step %d: %s; going on with what was filled", idx+1, why)
		return ""
	}
	var got uint64
	for _, t := range step.Takes {
		if t.Status == qsTakeSwapping || t.Status == qsTakeComplete {
			got += t.Out
		}
	}
	if step.Round >= step.MaxRounds {
		if got == 0 {
			return fmt.Sprintf("no order could be taken after %d attempts", step.Round)
		}
		return partial(fmt.Sprintf("no more replacement attempts (%d of %d used)", step.Round, step.MaxRounds), got)
	}
	m := mkts[step.Plan.marketShape.key()]
	if m == nil {
		return "the market is gone"
	}
	if got >= step.Target {
		return ""
	}
	left := step.Budget - min(step.Budget, step.Spent+step.Committed)
	fills, _, _ := qsWalk(m, step.Plan.FromAsset, left, exclude, self)
	fills, _, fout := qsTrim(fills, step.Plan.Sell, step.Target-got)
	if len(fills) == 0 {
		if got == 0 {
			return "no matching orders left on the book"
		}
		return partial("no more orders on the book to replace the failed ones", got)
	}
	// Projected final output if the rest runs on the current books. The
	// committed (agreed, still swapping) takes count as delivered.
	proj := qsProject(steps, mkts, idx, got+fout, exclude, self, r.reserve)
	r.setProjectionLocked(proj)
	if proj < r.st.MinOut {
		worse := 0.0
		if r.st.Expected > 0 {
			worse = float64(r.st.Expected-min(proj, r.st.Expected)) / float64(r.st.Expected) * 100
		}
		msg := fmt.Sprintf("the next best orders would give about %s (%.2f%% below the quote), less than your guaranteed minimum %s",
			amtStr(r.st.To, proj), worse, amtStr(r.st.To, r.st.MinOut))
		return msg
	}
	step.Round++
	var worstFailed uint64
	for _, fr := range failedRates {
		if worstFailed == 0 || qsRateWorsePct(step.Plan.Sell, fr, worstFailed) > 0 {
			worstFailed = fr
		}
	}
	replacement := step.Round > 1
	now := r.ms()
	for _, f := range fills {
		t := &QuickSwapTake{OrderID: f.OrderID, Owner: f.Owner, OwnerName: r.be.peerName(f.Owner), Rate: f.Rate,
			Lots: f.Lots, Qty: f.Qty, In: f.In, Out: f.Out, Status: qsTakeProposing, Replacement: replacement, Started: now,
			Deadline: now + r.timing.proposalTimeout.Milliseconds()}
		if t.OwnerName == "" {
			t.OwnerName = "peer " + short(f.Owner)
		}
		ref := worstFailed
		if ref == 0 && step.Plan.AvgRate > 0 {
			ref = step.Plan.AvgRate
		}
		t.WorsePct = qsRateWorsePct(step.Plan.Sell, ref, f.Rate)
		mid, err := r.be.meshTake(step.Plan.Market, f.OrderID, f.Lots)
		if err != nil {
			t.Status, t.Reason = qsTakeFailed, err.Error()
			exclude[f.OrderID] = true
			r.logf("warn", "Can't take order %s: %v", short(f.OrderID), err)
			step.Takes = append(step.Takes, t)
			continue
		}
		t.MatchID = mid
		step.Takes = append(step.Takes, t)
		if replacement {
			step.Note = fmt.Sprintf("Route adjusted: replaced with the next best order at %s (%s), still within your %.2g%%.",
				rateStr(step.Plan.Base, step.Plan.Quote, f.Rate), qsWorseStr(t.WorsePct), r.st.DriftPct)
			r.logf("warn", "Replacement: order %s, %s @ %s from %s (%s); projected %s, within %.2g%%", short(f.OrderID),
				amtStr(step.Plan.Base, f.Qty), rateStr(step.Plan.Base, step.Plan.Quote, f.Rate), t.OwnerName,
				qsWorseStr(t.WorsePct), amtStr(r.st.To, proj), r.st.DriftPct)
		} else {
			r.logf("", "Taking order %s: %s @ %s from %s", short(f.OrderID), amtStr(step.Plan.Base, f.Qty),
				rateStr(step.Plan.Base, step.Plan.Quote, f.Rate), t.OwnerName)
		}
	}
	return ""
}

func qsWorseStr(p float64) string {
	switch {
	case p > 0.005:
		return fmt.Sprintf("%.2f%% worse", p)
	case p < -0.005:
		return fmt.Sprintf("%.2f%% better", -p)
	}
	return "same price"
}

// runServerStep runs step idx on a DEX server: one immediate limit order at
// the worst rate the tolerance allows for the projected route.
func (r *qsRun) runServerStep(idx int) string {
	mkts, err := r.marketsByKey()
	r.mtx.Lock()
	step := r.st.Steps[idx]
	if err != nil {
		r.mtx.Unlock()
		return "can't read the order books"
	}
	m := mkts[step.Plan.marketShape.key()]
	if m == nil {
		r.mtx.Unlock()
		return "the market is gone"
	}
	fills, _, _ := qsWalk(m, step.Plan.FromAsset, step.Budget, nil, "")
	fills, fin, fout := qsTrim(fills, step.Plan.Sell, step.Target)
	if len(fills) == 0 {
		r.mtx.Unlock()
		return "no matching orders on the book"
	}
	proj := qsProject(r.st.Route.Steps, mkts, idx, fout, nil, "", r.reserve)
	r.setProjectionLocked(proj)
	if proj < r.st.MinOut {
		r.mtx.Unlock()
		return fmt.Sprintf("the books would give about %s, less than your guaranteed minimum %s", amtStr(r.st.To, proj), amtStr(r.st.To, r.st.MinOut))
	}
	// Limit rate: the walk's worst rate moved by the tolerance that is left.
	worst := fills[len(fills)-1].Rate
	slack := 0.0
	if proj > 0 {
		slack = float64(proj-r.st.MinOut) / float64(proj)
	}
	var rate, qty uint64
	if step.Plan.Sell {
		rate = uint64(float64(worst) * (1 - slack))
		if m.RateStep > 0 {
			rate = (rate + m.RateStep - 1) / m.RateStep * m.RateStep
		}
		qty = fin // base atoms, whole lots
	} else {
		rate = uint64(float64(worst) * (1 + slack))
		if m.RateStep > 0 {
			rate = rate / m.RateStep * m.RateStep
		}
		// Whole lots that the budget pays for at the limit rate.
		lotCost := calc.BaseToQuote(rate, m.LotSize)
		if lotCost > 0 {
			qty = min(fout, step.Budget/lotCost*m.LotSize)
		}
	}
	if qty == 0 || rate == 0 {
		r.mtx.Unlock()
		return "the amount is less than one lot"
	}
	r.mtx.Unlock()
	oid, err := r.be.serverTrade(step.Plan, qty, rate)
	r.mtx.Lock()
	if err != nil {
		r.logf("bad", "Order on %s failed: %v", step.Plan.Host, err)
		r.mtx.Unlock()
		return "the server order failed: " + err.Error()
	}
	step.ServerOrder = oid
	step.Round = 1
	r.logf("", "Placed an immediate limit order on %s: %s at %s or better", step.Plan.Host,
		amtStr(step.Plan.Base, qty), rateStr(step.Plan.Base, step.Plan.Quote, rate))
	step.Wait = &QuickSwapWait{Msg: "Waiting for the server to match the order and for its swaps"}
	r.mtx.Unlock()
	deadline := r.now().Add(r.timing.stepTimeout)
	for {
		done, base, quote, status, err := r.be.serverOrder(oid)
		r.mtx.Lock()
		if err == nil {
			if step.Plan.Sell {
				step.Spent, step.Received = base, quote
			} else {
				step.Spent, step.Received = quote, base
			}
			step.Wait = &QuickSwapWait{Msg: "Server order " + status}
		}
		r.mtx.Unlock()
		if err == nil && done {
			break
		}
		if r.now().After(deadline) {
			return "the server order took too long; see the Trade page"
		}
		r.sleep()
	}
	r.mtx.Lock()
	step.Wait = nil
	r.mtx.Unlock()
	return ""
}

// ---- Core glue ----

func (c *Core) quickSwaps() *quickSwapper {
	c.qsOnce.Do(func() { c.qs = &quickSwapper{runs: make(map[string]*qsRun)} })
	return c.qs
}

// qsCore implements qsBackend with Core.
type qsCore struct{ c *Core }

func (b *qsCore) self() string {
	if m, err := b.c.meshClient(); err == nil {
		return m.Self().String()
	}
	return ""
}

func (b *qsCore) peerName(id string) string {
	if m, err := b.c.meshClient(); err == nil {
		if pi, ok := m.Peer(id); ok {
			return pi.Name
		}
	}
	return ""
}

func (b *qsCore) markets() ([]*qsMarket, error) {
	c := b.c
	var out []*qsMarket
	haveWallet := func(a uint32) bool { _, ok := c.wallet(a); return ok }
	if m, err := c.meshClient(); err == nil {
		self := m.Self().String()
		for _, d := range meshpkg.DefaultMarkets {
			mkt, err := meshpkg.ParseMarket(d.Name)
			if err != nil || !haveWallet(mkt.Base) || !haveWallet(mkt.Quote) {
				continue
			}
			bv, err := c.MeshBook(d.Name)
			if err != nil {
				continue
			}
			qm := &qsMarket{Source: QuickSwapMesh, Host: MeshHost, Name: d.Name, Base: mkt.Base, Quote: mkt.Quote, LotSize: d.LotSize}
			for _, o := range bv.Orders {
				if o.Own || o.Owner == self || o.Hidden != "" || o.Warning != "" {
					continue
				}
				qo := &qsOrder{ID: o.ID, Owner: o.Owner, Rate: o.Rate, Qty: o.Remaining, LotSize: o.LotSize, Sell: o.Sell}
				if o.Sell {
					qm.Sells = append(qm.Sells, qo)
				} else {
					qm.Buys = append(qm.Buys, qo)
				}
			}
			qm.sortBooks()
			out = append(out, qm)
		}
	}
	for host, xc := range c.Exchanges() {
		if xc.ViewOnly || xc.ConnectionStatus != comms.Connected {
			continue
		}
		for name, mkt := range xc.Markets {
			if !haveWallet(mkt.BaseID) || !haveWallet(mkt.QuoteID) {
				continue
			}
			book, err := c.Book(host, mkt.BaseID, mkt.QuoteID)
			if err != nil {
				continue
			}
			qm := &qsMarket{Source: QuickSwapServer, Host: host, Name: name, Base: mkt.BaseID, Quote: mkt.QuoteID,
				LotSize: mkt.LotSize, RateStep: mkt.RateStep}
			for _, o := range book.Sells {
				qm.Sells = append(qm.Sells, &qsOrder{ID: o.Token, Rate: o.MsgRate, Qty: o.QtyAtomic, LotSize: mkt.LotSize, Sell: true})
			}
			for _, o := range book.Buys {
				qm.Buys = append(qm.Buys, &qsOrder{ID: o.Token, Rate: o.MsgRate, Qty: o.QtyAtomic, LotSize: mkt.LotSize})
			}
			qm.sortBooks()
			out = append(out, qm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, nil
}

func (b *qsCore) meshTake(market, orderID string, lots uint64) (string, error) {
	return b.c.MeshTakeOrder(market, orderID, lots)
}

func (b *qsCore) meshMatch(matchID string) (string, string, uint64, uint64, bool) {
	m, err := b.c.meshClient()
	if err != nil {
		return "", "", 0, 0, false
	}
	id, err := meshpkg.ParseOrderID(matchID)
	if err != nil {
		return "", "", 0, 0, false
	}
	rec, ok := m.MatchRecord(id)
	if !ok {
		return "", "", 0, 0, false
	}
	return rec.Status, rec.Reason, rec.Match.Qty, rec.Match.QuoteQty, true
}

func (b *qsCore) meshSwap(matchID string) (string, bool, bool) {
	m, err := b.c.meshClient()
	if err != nil {
		return "", false, false
	}
	id, err := meshpkg.ParseOrderID(matchID)
	if err != nil {
		return "", false, false
	}
	rec, ok := m.SwapRecord(id)
	if !ok {
		return "", false, false
	}
	return rec.Status, rec.Final(), true
}

func (b *qsCore) serverTrade(st *QuickSwapStepPlan, qty, rate uint64) (string, error) {
	ord, err := b.c.Trade(nil, &TradeForm{Host: st.Host, IsLimit: true, Sell: st.Sell, Base: st.Base, Quote: st.Quote,
		Qty: qty, Rate: rate, TifNow: true})
	if err != nil {
		return "", err
	}
	return ord.ID.String(), nil
}

func (b *qsCore) serverOrder(oid string) (bool, uint64, uint64, string, error) {
	id, err := hex.DecodeString(oid)
	if err != nil {
		return false, 0, 0, "", err
	}
	ord, err := b.c.Order(id)
	if err != nil {
		return false, 0, 0, "", err
	}
	var base, quote uint64
	active := false
	for _, m := range ord.Matches {
		if m.IsCancel {
			continue
		}
		if m.Active {
			active = true
			continue
		}
		if m.Refund == nil && !m.Revoked && m.Status >= order.MatchComplete {
			base += m.Qty
			quote += calc.BaseToQuote(m.Rate, m.Qty)
		}
	}
	done := ord.Status >= order.OrderStatusExecuted && !active
	return done, base, quote, ord.Status.String(), nil
}

func (b *qsCore) available(assetID uint32) uint64 {
	bal, err := b.c.AssetBalance(assetID)
	if err != nil || bal == nil {
		return 0
	}
	return bal.Available
}

// reserve is what Quick Swap keeps of an intermediate coin for the next
// swap's network fees: 0.001 coin.
func (b *qsCore) reserve(assetID uint32) uint64 {
	ui, err := asset.UnitInfo(assetID)
	if err != nil {
		return 0
	}
	return ui.Conventional.ConversionFactor / 1000
}

// qsQuoteReserve is the fee reserve the quote keeps back from an
// intermediate coin: none if the wallet already holds that much.
func qsQuoteReserve(be qsBackend) func(uint32) uint64 {
	have := make(map[uint32]bool)
	return func(a uint32) uint64 {
		r := be.reserve(a)
		if v, ok := have[a]; ok {
			if v {
				return 0
			}
			return r
		}
		have[a] = be.available(a) >= r
		if have[a] {
			return 0
		}
		return r
	}
}

func qsQuote(be qsBackend, form *QuickSwapForm) (*QuickSwapQuote, []*qsMarket, error) {
	if form.From == form.To {
		return nil, nil, errors.New("pick two different coins")
	}
	if form.Amount == 0 {
		return nil, nil, errors.New("enter an amount")
	}
	if err := qsCheckDrift(form.DriftPct); err != nil {
		return nil, nil, err
	}
	mkts, err := be.markets()
	if err != nil {
		return nil, nil, err
	}
	routes := qsFindRoutes(mkts, form.From, form.To, form.Amount, be.self(), qsQuoteReserve(be))
	q := &QuickSwapQuote{DriftPct: form.DriftPct, Routes: len(routes), Others: []*QuickSwapRoute{}, Markets: []string{}}
	for _, m := range mkts {
		q.Markets = append(q.Markets, m.Source+":"+m.Name)
	}
	if len(routes) == 0 {
		return q, mkts, errNoRoute
	}
	q.Route = routes[0]
	q.MinOut = qsMinOut(q.Route.Out, form.DriftPct)
	if len(routes) > 1 {
		q.Others = routes[1:min(len(routes), 5)]
	}
	return q, mkts, nil
}

// QuickSwapQuote finds the best route for a swap.
func (c *Core) QuickSwapQuote(form *QuickSwapForm) (*QuickSwapQuote, error) {
	if _, ok := c.wallet(form.From); !ok {
		return nil, fmt.Errorf("no %s wallet", dex.BipIDSymbol(form.From))
	}
	if _, ok := c.wallet(form.To); !ok {
		return nil, fmt.Errorf("no %s wallet", dex.BipIDSymbol(form.To))
	}
	q, _, err := qsQuote(&qsCore{c}, form)
	return q, err
}

// qsStart re-quotes and starts a run. The guaranteed minimum is the one the
// user was shown (form.MinOut), if the books still allow it.
func (qs *quickSwapper) start(be qsBackend, form *QuickSwapForm, timing qsTiming) (*qsRun, error) {
	q, _, err := qsQuote(be, form)
	if err != nil {
		return nil, err
	}
	minOut := q.MinOut
	if form.MinOut > 0 {
		if q.Route.Out < form.MinOut {
			return nil, fmt.Errorf("prices moved: the best route now gives %s, less than the minimum you were shown (%s). Check the new quote",
				amtStr(form.To, q.Route.Out), amtStr(form.To, form.MinOut))
		}
		minOut = max(minOut, form.MinOut)
	}
	if bal := be.available(form.From); bal < q.Route.In {
		return nil, fmt.Errorf("not enough %s: the route spends %s, you have %s available", dex.BipIDSymbol(form.From),
			amtStr(form.From, q.Route.In), amtStr(form.From, bal))
	}
	qs.mtx.Lock()
	for _, r := range qs.runs {
		if s := r.snapshot(); s.Status == QuickSwapRunning {
			qs.mtx.Unlock()
			return nil, errors.New("a Quick Swap is already running; wait for it or stop it first")
		}
	}
	r := newQSRun(be, q.Route, form.DriftPct, minOut, timing)
	qs.runs[r.st.ID] = r
	qs.mtx.Unlock()
	go r.run()
	return r, nil
}

// QuickSwapStart starts a Quick Swap.
func (c *Core) QuickSwapStart(form *QuickSwapForm) (*QuickSwapRun, error) {
	if _, err := c.meshClient(); err != nil && len(c.Exchanges()) == 0 {
		return nil, errors.New("no markets: connect a DEX server or enable the mesh")
	}
	r, err := c.quickSwaps().start(&qsCore{c}, form, qsDefaultTiming)
	if err != nil {
		return nil, err
	}
	return r.snapshot(), nil
}

// QuickSwapRuns returns all Quick Swaps of this session, newest first.
func (c *Core) QuickSwapRuns() []*QuickSwapRun {
	qs := c.quickSwaps()
	qs.mtx.Lock()
	runs := make([]*qsRun, 0, len(qs.runs))
	for _, r := range qs.runs {
		runs = append(runs, r)
	}
	qs.mtx.Unlock()
	out := make([]*QuickSwapRun, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	return out
}

// QuickSwapRun returns one Quick Swap.
func (c *Core) QuickSwapRun(id string) (*QuickSwapRun, error) {
	qs := c.quickSwaps()
	qs.mtx.Lock()
	r := qs.runs[id]
	qs.mtx.Unlock()
	if r == nil {
		return nil, errors.New("unknown quick swap")
	}
	return r.snapshot(), nil
}

// QuickSwapStop asks a running Quick Swap to stop: no new orders are taken,
// swaps already agreed finish, and the user keeps what they have.
func (c *Core) QuickSwapStop(id string) (*QuickSwapRun, error) {
	qs := c.quickSwaps()
	qs.mtx.Lock()
	r := qs.runs[id]
	qs.mtx.Unlock()
	if r == nil {
		return nil, errors.New("unknown quick swap")
	}
	r.requestStop()
	return r.snapshot(), nil
}
