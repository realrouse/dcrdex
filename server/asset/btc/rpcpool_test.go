// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package btc

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tSerialRequester handles one request at a time, like rpcclient.Client in
// HTTP POST mode.
type tSerialRequester struct {
	mtx      sync.Mutex
	delay    time.Duration
	calls    atomic.Int32
	shutdown atomic.Bool
}

func (r *tSerialRequester) RawRequest(context.Context, string, []json.RawMessage) (json.RawMessage, error) {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	r.calls.Add(1)
	time.Sleep(r.delay)
	return json.RawMessage("null"), nil
}

func (r *tSerialRequester) Shutdown()        { r.shutdown.Store(true) }
func (r *tSerialRequester) WaitForShutdown() {}

func TestRequesterPool(t *testing.T) {
	const delay = 50 * time.Millisecond
	reqs := make([]*tSerialRequester, rpcPoolSize)
	rrs := make([]RawRequester, rpcPoolSize)
	for i := range reqs {
		reqs[i] = &tSerialRequester{delay: delay}
		rrs[i] = reqs[i]
	}
	pool := newRequesterPool(rrs)

	// rpcPoolSize*2 concurrent requests take about 2 delays, not
	// rpcPoolSize*2 delays as they would on a single serial requester.
	n := rpcPoolSize * 2
	var wg sync.WaitGroup
	start := time.Now()
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pool.RawRequest(context.Background(), "getblockcount", nil); err != nil {
				t.Errorf("request error: %v", err)
			}
		}()
	}
	wg.Wait()
	if el := time.Since(start); el >= time.Duration(n)*delay {
		t.Fatalf("requests were not spread over the pool: %v for %d requests", el, n)
	}
	for i, r := range reqs {
		if c := r.calls.Load(); c == 0 {
			t.Fatalf("requester %d was never used", i)
		}
	}

	// With nothing in flight, the first requester is used.
	before := reqs[0].calls.Load()
	pool.RawRequest(context.Background(), "getblockcount", nil)
	if reqs[0].calls.Load() != before+1 {
		t.Fatalf("idle pool did not use the first requester")
	}

	pool.Shutdown()
	pool.WaitForShutdown()
	for i, r := range reqs {
		if !r.shutdown.Load() {
			t.Fatalf("requester %d not shut down", i)
		}
	}
}
