// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// SIMNET DEBUG ONLY (S8 resilience campaign): an epoch-topic flooder and a
// fake relay. The fake relay re-publishes epoch messages it received from
// others, unchanged (stale replays) or tampered with (changed fingerprints,
// matches, orders, stamps) while keeping the original signatures, the way a
// malicious relay node would. Only reachable through the debug API.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	mrand "math/rand"
)

const advCaptureMax = 400

// EnableEpochCapture keeps the last epoch messages received from others.
func (e *EpochEngine) EnableEpochCapture() {
	e.mtx.Lock()
	e.advCapOn = true
	e.mtx.Unlock()
}

func (e *EpochEngine) captureLocked(m *epochMsg) {
	if !e.advCapOn || m.Type == epMsgFrag {
		return
	}
	c := *m
	e.advCap = append(e.advCap, &c)
	if len(e.advCap) > advCaptureMax {
		e.advCap = e.advCap[len(e.advCap)-advCaptureMax:]
	}
}

// EpochFloodResult reports what a flood or fake relay published.
type EpochFloodResult struct {
	Sent     int            `json:"sent"`
	ByType   map[string]int `json:"byType"`
	Tampered int            `json:"tampered"`
	Captured int            `json:"captured"`
}

// FloodEpochs publishes n junk messages of size bytes on the epoch topic:
// "junk" (random bytes), "badjson" (an epoch envelope with garbage fields)
// or "orders" (validly shaped orders with bad signatures).
func (e *EpochEngine) FloodEpochs(ctx context.Context, mode string, n, size int) *EpochFloodResult {
	res := &EpochFloodResult{ByType: map[string]int{}}
	if size <= 0 {
		size = 256
	}
	for i := 0; i < n; i++ {
		var b []byte
		switch mode {
		case "badjson":
			b, _ = json.Marshal(map[string]any{"type": "rec", "rec": map[string]any{"epoch": mrand.Uint64(), "sigs": []int{1, 2, 3}}})
		case "orders":
			o := &MeshOrder{Market: Market{Base: 42, Quote: 0}, Owner: e.self, Sell: i%2 == 0, Qty: 1e8, LotSize: 1e8,
				Rate: 1 + mrand.Uint64()%1e6, Stamp: e.nowMs(), Expiry: e.nowMs() + 600_000, Nonce: mrand.Uint64(), Sig: make([]byte, 64)}
			b, _ = json.Marshal(&epochMsg{Type: epMsgOrder, Order: o})
		default:
			b = make([]byte, size)
			_, _ = rand.Read(b)
		}
		if err := e.ps.Broadcast(ctx, EpochTopic, b); err == nil {
			res.Sent++
			res.ByType[mode]++
		}
	}
	return res
}

// FakeRelay re-publishes up to n captured messages; with tamper, each is
// changed after it was signed.
func (e *EpochEngine) FakeRelay(ctx context.Context, n int, tamper bool) *EpochFloodResult {
	e.mtx.Lock()
	msgs := append([]*epochMsg(nil), e.advCap...)
	e.mtx.Unlock()
	res := &EpochFloodResult{ByType: map[string]int{}, Captured: len(msgs)}
	for i := 0; i < len(msgs) && res.Sent < n; i++ {
		// Deep copy through JSON so the engine's own objects are untouched.
		raw, err := json.Marshal(msgs[i])
		if err != nil {
			continue
		}
		var m epochMsg
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if tamper && tamperEpochMsg(&m) {
			res.Tampered++
		}
		e.send(ctx, &m)
		res.Sent++
		res.ByType[m.Type]++
	}
	return res
}

func flip(h *EpochHash) { h[0] ^= 0xff }

// tamperEpochMsg changes a signed field of m.
func tamperEpochMsg(m *epochMsg) bool {
	switch {
	case m.Rec != nil:
		r := m.Rec
		if len(r.Matches) > 0 {
			r.Matches[0].Qty *= 2
			r.Matches[0].Rate++
		} else {
			flip(&r.StateHash)
		}
		flip(&r.Fingerprint)
		return true
	case len(m.Recs) > 0:
		for _, r := range m.Recs {
			flip(&r.StateHash)
		}
		return true
	case m.Att != nil:
		flip(&m.Att.Fingerprint)
		return true
	case m.Prop != nil:
		if len(m.Prop.Orders) > 0 {
			m.Prop.Orders = m.Prop.Orders[1:]
		} else {
			m.Prop.Stamp++
		}
		return true
	case m.Order != nil:
		m.Order.Rate = m.Order.Rate*2 + 1
		return true
	case m.HB != nil:
		m.HB.Stamp += 10_000 // a clock lie under another validator's name
		return true
	case m.Book != nil:
		if len(m.Book.Resting) > 0 {
			m.Book.Resting = m.Book.Resting[1:] // frame the validator as hiding
		}
		return true
	}
	return false
}
