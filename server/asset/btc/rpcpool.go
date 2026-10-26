// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"context"
	"encoding/json"
	"sync/atomic"
)

// rpcPoolSize is the number of RPC connections a Backend uses to its node.
//
// rpcclient.Client in HTTP POST mode sends one request at a time from a
// single goroutine, so every RPC the backend makes waits for all the ones
// queued before it. Verifying an order's funding coin takes about five
// round trips (gettxout, getrawtransaction, getblock and one
// getrawtransaction per input of the funding tx), so with a node behind a
// high latency link (~0.3 s per call for the LBC node) an order with 20
// inputs needed ~30 s of serial RPCs. That was longer than the order router
// waits per pass, and the order failed with "failed to find funding coins".
// Spreading requests over a few connections lets the router's concurrent
// coin lookups actually run concurrently. Keep it well below the node's
// default rpcmaxclients (10).
const rpcPoolSize = 4

// requesterPool is a RawRequester that sends each request on the pooled
// requester with the fewest requests in flight.
type requesterPool struct {
	requesters []RawRequester
	inFlight   []atomic.Int32
}

var _ RawRequester = (*requesterPool)(nil)

func newRequesterPool(requesters []RawRequester) *requesterPool {
	return &requesterPool{
		requesters: requesters,
		inFlight:   make([]atomic.Int32, len(requesters)),
	}
}

func (p *requesterPool) pick() int {
	best := 0
	bestN := p.inFlight[0].Load()
	for i := 1; i < len(p.requesters) && bestN > 0; i++ {
		if n := p.inFlight[i].Load(); n < bestN {
			best, bestN = i, n
		}
	}
	return best
}

// RawRequest sends the request on the least busy requester.
func (p *requesterPool) RawRequest(ctx context.Context, method string, params []json.RawMessage) (json.RawMessage, error) {
	i := p.pick()
	p.inFlight[i].Add(1)
	defer p.inFlight[i].Add(-1)
	return p.requesters[i].RawRequest(ctx, method, params)
}

// Shutdown shuts down all of the requesters.
func (p *requesterPool) Shutdown() {
	for _, r := range p.requesters {
		r.Shutdown()
	}
}

// WaitForShutdown waits for all of the requesters to shut down.
func (p *requesterPool) WaitForShutdown() {
	for _, r := range p.requesters {
		r.WaitForShutdown()
	}
}
