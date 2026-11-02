// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBondPoster struct {
	posts atomic.Int32
	fail  atomic.Bool
}

func (f *fakeBondPoster) PostBond(context.Context) error {
	f.posts.Add(1)
	if f.fail.Load() {
		return errors.New("no mesh connection established")
	}
	return nil
}

func TestBondRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &fakeBondPoster{}
	var conn atomic.Bool
	done := make(chan struct{})
	go func() {
		runBondRefresh(ctx, p, conn.Load, 10*time.Millisecond, tLogger)
		close(done)
	}()
	// Not connected: no posts.
	time.Sleep(60 * time.Millisecond)
	if n := p.posts.Load(); n != 0 {
		t.Fatalf("posted %d times while disconnected", n)
	}
	// Connected: posts every interval, and keeps going through errors.
	conn.Store(true)
	p.fail.Store(true)
	time.Sleep(60 * time.Millisecond)
	p.fail.Store(false)
	time.Sleep(60 * time.Millisecond)
	if n := p.posts.Load(); n < 6 {
		t.Fatalf("only %d posts while connected", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresher did not stop")
	}
	// Zero interval disables it.
	runBondRefresh(context.Background(), p, conn.Load, 0, tLogger)
}
