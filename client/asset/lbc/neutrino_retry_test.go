// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"errors"
	"testing"
	"time"

	"github.com/lbryio/lbcd/chaincfg"
	"github.com/lbryio/lbcd/chaincfg/chainhash"
	"github.com/lbryio/lbcd/wire"
	btcutil "github.com/lbryio/lbcutil"
	"github.com/lbryio/lbcutil/gcs"
	"github.com/realrouse/lbcd-spv/neutrino"
	"github.com/realrouse/lbcd-spv/neutrino/blockntfns"
	"github.com/realrouse/lbcd-spv/neutrino/headerfs"
)

// tChainSource has a main chain of one block (height 1) and fails filter and
// block fetches failN times.
type tChainSource struct {
	hdr      wire.BlockHeader
	reorged  bool
	failN    int
	fErr     error
	calls    int
	blkCalls int
}

func (c *tChainSource) ChainParams() chaincfg.Params { return chaincfg.RegressionNetParams }
func (c *tChainSource) BestBlock() (*headerfs.BlockStamp, error) {
	return &headerfs.BlockStamp{Height: 1, Hash: c.hdr.BlockHash()}, nil
}
func (c *tChainSource) GetBlockHeaderByHeight(h uint32) (*wire.BlockHeader, error) {
	if c.reorged {
		other := c.hdr
		other.Nonce++
		return &other, nil
	}
	return &c.hdr, nil
}
func (c *tChainSource) GetBlockHeader(*chainhash.Hash) (*wire.BlockHeader, uint32, error) {
	return &c.hdr, 1, nil
}
func (c *tChainSource) GetBlock(chainhash.Hash, ...neutrino.QueryOption) (*btcutil.Block, error) {
	c.blkCalls++
	if c.blkCalls <= c.failN {
		return nil, errors.New("couldn't retrieve block from network")
	}
	return btcutil.NewBlock(&wire.MsgBlock{Header: c.hdr}), nil
}
func (c *tChainSource) GetFilterHeaderByHeight(uint32) (*chainhash.Hash, error) {
	return &chainhash.Hash{}, nil
}
func (c *tChainSource) GetCFilter(chainhash.Hash, wire.FilterType, ...neutrino.QueryOption) (*gcs.Filter, error) {
	c.calls++
	if c.calls <= c.failN {
		return nil, c.fErr
	}
	return &gcs.Filter{}, nil
}
func (c *tChainSource) Subscribe(uint32) (*blockntfns.Subscription, error) { return nil, nil }

func TestRescanSourceRetries(t *testing.T) {
	defer func(min, max time.Duration) { rescanRetryMin, rescanRetryMax = min, max }(rescanRetryMin, rescanRetryMax)
	rescanRetryMin, rescanRetryMax = time.Millisecond, 2*time.Millisecond

	newSrc := func(cs *tChainSource) (*rescanSource, chan struct{}) {
		quit := make(chan struct{})
		return &rescanSource{rescanChainSource: cs, quit: quit, stop: make(chan struct{})}, quit
	}
	hdr := wire.BlockHeader{Version: 1, Timestamp: time.Unix(1700000000, 0)}
	hash := hdr.BlockHash()

	// A failed filter query is retried until it succeeds.
	cs := &tChainSource{hdr: hdr, failN: 5, fErr: neutrino.ErrFilterFetchFailed}
	src, _ := newSrc(cs)
	if f, err := src.GetCFilter(hash, wire.GCSFilterRegular); err != nil || f == nil {
		t.Fatalf("GetCFilter: %v", err)
	}
	if cs.calls != 6 {
		t.Fatalf("want 6 filter attempts, got %d", cs.calls)
	}

	// Other errors are returned at once.
	other := errors.New("other")
	cs = &tChainSource{hdr: hdr, failN: 5, fErr: other}
	src, _ = newSrc(cs)
	if _, err := src.GetCFilter(hash, wire.GCSFilterRegular); !errors.Is(err, other) || cs.calls != 1 {
		t.Fatalf("want immediate error, got %v after %d calls", err, cs.calls)
	}

	// A block that left the main chain is not retried.
	cs = &tChainSource{hdr: hdr, failN: 5, fErr: neutrino.ErrFilterFetchFailed, reorged: true}
	src, _ = newSrc(cs)
	if _, err := src.GetCFilter(hash, wire.GCSFilterRegular); err == nil || cs.calls != 1 {
		t.Fatalf("want reorg error, got %v after %d calls", err, cs.calls)
	}

	// Blocks are retried too.
	cs = &tChainSource{hdr: hdr, failN: 3}
	src, _ = newSrc(cs)
	if b, err := src.GetBlock(hash); err != nil || b == nil || cs.blkCalls != 4 {
		t.Fatalf("GetBlock: %v after %d calls", err, cs.blkCalls)
	}

	// Stopping the rescan ends the retries.
	cs = &tChainSource{hdr: hdr, failN: 1 << 30, fErr: neutrino.ErrFilterFetchFailed}
	src, quit := newSrc(cs)
	done := make(chan error, 1)
	go func() { _, err := src.GetCFilter(hash, wire.GCSFilterRegular); done <- err }()
	time.Sleep(20 * time.Millisecond)
	close(quit)
	select {
	case err := <-done:
		if !errors.Is(err, neutrino.ErrFilterFetchFailed) {
			t.Fatalf("want fetch error after quit, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not stop on quit")
	}
}
