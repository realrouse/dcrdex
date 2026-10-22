//go:build !harness && !vspd

package dcr

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
)

func TestOracleFeeRateNonBlocking(t *testing.T) {
	wallet, node, shutdown := tNewWalletMonitorBlocks(false)
	defer shutdown()
	node.estFeeErr = errors.New("no local estimate")
	cfg := *wallet.config()
	cfg.apiFeeFallback = true
	cfg.feeRateLimit = 1000
	wallet.cfgV.Store(&cfg)

	var calls atomic.Int32
	var rate atomic.Value // float64 DCR/kB
	rate.Store(0.0001)    // 10 atoms/B
	var fail atomic.Bool
	gate := make(chan struct{})
	close(gate)
	var gateV atomic.Value
	gateV.Store(gate)
	defer func(f func(context.Context, dex.Network, uint64) (float64, error)) { oracleFeeFetcher = f }(oracleFeeFetcher)
	oracleFeeFetcher = func(ctx context.Context, _ dex.Network, _ uint64) (float64, error) {
		calls.Add(1)
		<-gateV.Load().(chan struct{})
		if fail.Load() {
			return 0, errors.New("oracle down")
		}
		return rate.Load().(float64), nil
	}

	// First call has nothing cached and waits for the one fetch.
	if r, err := wallet.feeRate(2); err != nil || r != 10 {
		t.Fatalf("first fee rate = %d, %v", r, err)
	}
	// Fresh: served from cache.
	if r, _ := wallet.feeRate(2); r != 10 || calls.Load() != 1 {
		t.Fatalf("fresh rate not served from cache (rate %d, %d calls)", r, calls.Load())
	}

	// Stale: the old rate is returned at once while a single refresh
	// runs in the background, even with the oracle hanging.
	hang := make(chan struct{})
	gateV.Store(hang)
	rate.Store(0.0002)
	wallet.oracleFeesMtx.Lock()
	wallet.oracleFees[2] = feeStamped{10, time.Now().Add(-2 * freshFeeAge)}
	wallet.oracleFeesMtx.Unlock()
	for i := 0; i < 5; i++ {
		start := time.Now()
		if r, _ := wallet.feeRate(2); r != 10 {
			t.Fatalf("stale rate not served while refreshing, got %d", r)
		}
		if time.Since(start) > 100*time.Millisecond {
			t.Fatalf("feeRate blocked on the oracle")
		}
	}
	// The lock is not held across the fetch.
	locked := make(chan struct{})
	go func() {
		wallet.oracleFeesMtx.Lock()
		wallet.oracleFeesMtx.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatalf("oracleFeesMtx held during the fetch")
	}
	close(hang)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if r, _ := wallet.feeRate(2); r == 20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background refresh did not update the rate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("expected a single refresh, saw %d fetches in total", n)
	}

	// A failing oracle keeps serving the last good rate.
	fail.Store(true)
	wallet.oracleFeesMtx.Lock()
	wallet.oracleFees[2] = feeStamped{20, time.Now().Add(-2 * freshFeeAge)}
	wallet.oracleFeesMtx.Unlock()
	if r, _ := wallet.feeRate(2); r != 20 {
		t.Fatalf("last good rate not served, got %d", r)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		wallet.oracleFeesMtx.Lock()
		failing := wallet.oracleFailing
		wallet.oracleFeesMtx.Unlock()
		if failing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("oracle not flagged failing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r, _ := wallet.feeRate(2); r != 20 {
		t.Fatalf("last good rate not served while failing, got %d", r)
	}

	// Nothing usable cached and the oracle failing: no rate, no wait.
	wallet.oracleFeesMtx.Lock()
	wallet.oracleFees[2] = feeStamped{20, time.Now().Add(-2 * maxStaleOracleFeeAge)}
	wallet.oracleFeesMtx.Unlock()
	start := time.Now()
	if r, err := wallet.feeRate(2); r != 0 || err != nil {
		t.Fatalf("expected 0, nil, got %d, %v", r, err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("feeRate waited while the oracle is failing")
	}
	// FeeRateWithFallback still gives a usable rate.
	if r := wallet.targetFeeRateWithFallback(2, 0); r == 0 {
		t.Fatalf("no fallback rate")
	}
}
