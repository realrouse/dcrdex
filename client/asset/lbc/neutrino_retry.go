// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"errors"
	"time"

	"github.com/lbryio/lbcd/chaincfg/chainhash"
	"github.com/lbryio/lbcd/wire"
	btcutil "github.com/lbryio/lbcutil"
	"github.com/lbryio/lbcutil/gcs"
	"github.com/realrouse/lbcd-spv/neutrino"
)

// Retry pacing for rescanSource. Variables so tests can shorten them.
var (
	rescanRetryMin = time.Second
	rescanRetryMax = 15 * time.Second
)

// rescanChainSource is the subset of neutrino.ChainSource that rescanSource
// wraps. It is satisfied by *neutrino.RescanChainSource.
type rescanChainSource interface {
	neutrino.ChainSource
}

// rescanSource is the neutrino.ChainSource given to wallet rescans. It retries
// compact filter and block fetches that fail for network reasons, for as long
// as the block is still in the main chain and the rescan is running.
//
// Without it, a neutrino rescan that is catching up (the wallet is behind the
// header chain, as after every restart) returns from its goroutine on the first
// failed GetCFilter or GetBlock, for example when the only peer is reconnecting
// while the rescan asks for its first batch of filters ("Query failed with 0 out
// of N filters received"). Nothing restarts it: the wallet stays at its old
// height, shows as syncing, and only the spv stall watchdog's full rescan (after
// 10 minutes) recovers it.
type rescanSource struct {
	rescanChainSource
	quit <-chan struct{} // the rescan's quit channel
	stop <-chan struct{} // the client's quit channel
}

var _ neutrino.ChainSource = (*rescanSource)(nil)

func newRescanSource(cs *neutrino.ChainService, quit, stop <-chan struct{}) *rescanSource {
	return &rescanSource{
		rescanChainSource: &neutrino.RescanChainSource{ChainService: cs},
		quit:              quit,
		stop:              stop,
	}
}

// inMainChain reports whether the block with hash is in the current best
// header chain. A block that was reorged out must not be retried.
func (s *rescanSource) inMainChain(hash *chainhash.Hash) bool {
	_, height, err := s.GetBlockHeader(hash)
	if err != nil {
		return false
	}
	hdr, err := s.GetBlockHeaderByHeight(height)
	if err != nil {
		return false
	}
	return hdr.BlockHash() == *hash
}

// retry calls f until it succeeds, retryable returns false for its error, the
// block leaves the main chain or the rescan is stopped. The last error is
// returned in the failing cases.
func (s *rescanSource) retry(what string, hash *chainhash.Hash, f func() error, retryable func(error) bool) error {
	wait := rescanRetryMin
	for attempt := 1; ; attempt++ {
		err := f()
		if err == nil {
			if attempt > 1 {
				nlog.Infof("Rescan: fetched %s for block %s after %d attempts", what, hash, attempt)
			}
			return nil
		}
		if !retryable(err) || !s.inMainChain(hash) {
			return err
		}
		if attempt == 1 || attempt%10 == 0 {
			nlog.Warnf("Rescan: unable to fetch %s for block %s (attempt %d), retrying in %v: %v",
				what, hash, attempt, wait, err)
		}
		select {
		case <-time.After(wait):
		case <-s.quit:
			return err
		case <-s.stop:
			return err
		}
		if wait *= 2; wait > rescanRetryMax {
			wait = rescanRetryMax
		}
	}
}

// GetCFilter fetches a compact filter, retrying failed network queries.
func (s *rescanSource) GetCFilter(hash chainhash.Hash, filterType wire.FilterType,
	options ...neutrino.QueryOption) (filter *gcs.Filter, err error) {
	err = s.retry("filter", &hash, func() (err error) {
		filter, err = s.rescanChainSource.GetCFilter(hash, filterType, options...)
		return err
	}, func(err error) bool {
		return errors.Is(err, neutrino.ErrFilterFetchFailed)
	})
	return filter, err
}

// GetBlock fetches a block, retrying failed network queries.
func (s *rescanSource) GetBlock(hash chainhash.Hash, options ...neutrino.QueryOption) (block *btcutil.Block, err error) {
	err = s.retry("block", &hash, func() (err error) {
		block, err = s.rescanChainSource.GetBlock(hash, options...)
		if err == nil && block == nil {
			err = errors.New("no block returned")
		}
		return err
	}, func(err error) bool {
		return !errors.Is(err, neutrino.ErrShuttingDown)
	})
	return block, err
}
