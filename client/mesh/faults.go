// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// SIMNET DEBUG ONLY (S8 resilience campaign). faultPS wraps the mesh
// pub/sub client so the debug API can inject message loss, delay and
// reordering, and app-level network partitions (drop everything from a set
// of peers), on a running node. With no faults set it passes everything
// through unchanged.

import (
	"context"
	"math/rand"
	"sync"
	"time"

	mc "github.com/bisoncraft/mesh/client"
)

// MeshFaults is the injected fault configuration.
type MeshFaults struct {
	// DropInPct / DropOutPct: percent of received / sent data messages dropped.
	DropInPct  float64 `json:"dropInPct"`
	DropOutPct float64 `json:"dropOutPct"`
	// DelayMs (+ uniform random JitterMs) delays received messages; jitter
	// reorders them.
	DelayMs  int64 `json:"delayMs"`
	JitterMs int64 `json:"jitterMs"`
	// Block: peer IDs whose messages are dropped (one side of a partition).
	Block []string `json:"block"`
	// EpochOnly limits loss and delay (not Block) to the epoch topic.
	EpochOnly bool `json:"epochOnly"`
}

// FaultStats counts what the fault layer did.
type FaultStats struct {
	Faults     MeshFaults `json:"faults"`
	Received   int64      `json:"received"`
	DroppedIn  int64      `json:"droppedIn"`
	Blocked    int64      `json:"blocked"`
	Delayed    int64      `json:"delayed"`
	Sent       int64      `json:"sent"`
	DroppedOut int64      `json:"droppedOut"`
}

type faultPS struct {
	inner pubSub
	mtx   sync.Mutex
	on    bool
	f     MeshFaults
	block map[string]bool
	rng   *rand.Rand
	st    FaultStats
}

func newFaultPS(inner pubSub) *faultPS {
	return &faultPS{inner: inner, rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func (p *faultPS) set(f MeshFaults) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	p.f = f
	p.block = make(map[string]bool, len(f.Block))
	for _, id := range f.Block {
		p.block[id] = true
	}
	p.on = f.DropInPct > 0 || f.DropOutPct > 0 || f.DelayMs > 0 || f.JitterMs > 0 || len(f.Block) > 0
}

func (p *faultPS) stats() FaultStats {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	st := p.st
	st.Faults = p.f
	return st
}

func (p *faultPS) Broadcast(ctx context.Context, topic string, data []byte) error {
	p.mtx.Lock()
	drop := false
	if p.on {
		p.st.Sent++
		if p.f.DropOutPct > 0 && (!p.f.EpochOnly || topic == EpochTopic) && p.rng.Float64()*100 < p.f.DropOutPct {
			drop = true
			p.st.DroppedOut++
		}
	}
	p.mtx.Unlock()
	if drop {
		return nil
	}
	return p.inner.Broadcast(ctx, topic, data)
}

func (p *faultPS) Subscribe(ctx context.Context, topic string, h mc.TopicHandler) error {
	return p.inner.Subscribe(ctx, topic, func(ev mc.TopicEvent) {
		if ev.Type != mc.TopicEventData {
			h(ev)
			return
		}
		p.mtx.Lock()
		if !p.on {
			p.mtx.Unlock()
			h(ev)
			return
		}
		p.st.Received++
		if p.block[ev.Peer.String()] {
			p.st.Blocked++
			p.mtx.Unlock()
			return
		}
		lossy := !p.f.EpochOnly || topic == EpochTopic
		if lossy && p.f.DropInPct > 0 && p.rng.Float64()*100 < p.f.DropInPct {
			p.st.DroppedIn++
			p.mtx.Unlock()
			return
		}
		var d time.Duration
		if lossy && (p.f.DelayMs > 0 || p.f.JitterMs > 0) {
			ms := p.f.DelayMs
			if p.f.JitterMs > 0 {
				ms += p.rng.Int63n(p.f.JitterMs + 1)
			}
			d = time.Duration(ms) * time.Millisecond
			p.st.Delayed++
		}
		p.mtx.Unlock()
		if d > 0 {
			time.AfterFunc(d, func() { h(ev) })
			return
		}
		h(ev)
	})
}

// Unsubscribe passes through (the epoch engine renews quiet subscriptions).
func (p *faultPS) Unsubscribe(ctx context.Context, topic string) error {
	type unsubscriber interface {
		Unsubscribe(ctx context.Context, topic string) error
	}
	if u, ok := p.inner.(unsubscriber); ok {
		return u.Unsubscribe(ctx, topic)
	}
	return nil
}

// SetFaults sets the injected faults (debug API only).
func (ob *OrderBooks) SetFaults(f MeshFaults) { ob.faults.set(f) }

// Faults returns the fault configuration and counters.
func (ob *OrderBooks) Faults() FaultStats { return ob.faults.stats() }
