// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/db"
)

var (
	// bondRefundRetryInterval is how long to wait for a broadcast bond refund
	// to confirm before checking the bond output and rebroadcasting the
	// refund.
	bondRefundRetryInterval = 15 * time.Minute
	// bondRefundSpentGrace is how long we will wait for one of our own
	// unconfirmed refunds to confirm when the bond output is reported spent
	// (e.g. by a full node that counts mempool spends), before marking the bond
	// refunded anyway.
	bondRefundSpentGrace = 6 * time.Hour
	// refundedBondRecheckWindow limits the startup re-check of bonds that are
	// marked refunded in the DB to bonds with lock times within this window.
	// Older bonds were refunded long ago, and checking them can be expensive
	// for SPV wallets.
	refundedBondRecheckWindow = 60 * 24 * time.Hour
	// refundedBondRecheckRetry is the delay before retrying a failed re-check.
	refundedBondRecheckRetry = 10 * time.Minute
	// refundedBondRecheckTimeout limits a single re-check, which may involve
	// a long block filters scan for SPV wallets.
	refundedBondRecheckTimeout = time.Hour
)

const maxRefundedBondRechecks = 6

// bondRefundState tracks a broadcast bond refund that has not yet been
// confirmed.
type bondRefundState struct {
	refundCoins    [][]byte
	refundStrs     []string
	refundVal      uint64
	firstSent      time.Time
	lastSent       time.Time
	lastSpentCheck time.Time
	attempts       int
}

func (s *bondRefundState) addRefund(coinID []byte, coinStr string) {
	if len(coinID) == 0 {
		return
	}
	for _, c := range s.refundCoins {
		if bytes.Equal(c, coinID) {
			return
		}
	}
	s.refundCoins = append(s.refundCoins, coinID)
	s.refundStrs = append(s.refundStrs, coinStr)
}

func bondRefundKey(host string, bond *db.Bond) string {
	return host + ":" + hex.EncodeToString(bond.UniqueID())
}

func (c *Core) bondRefundState(key string) *bondRefundState {
	c.bondRefundsMtx.Lock()
	defer c.bondRefundsMtx.Unlock()
	return c.bondRefunds[key]
}

func (c *Core) setBondRefundState(key string, st *bondRefundState) {
	c.bondRefundsMtx.Lock()
	defer c.bondRefundsMtx.Unlock()
	if c.bondRefunds == nil {
		c.bondRefunds = make(map[string]*bondRefundState)
	}
	c.bondRefunds[key] = st
}

func (c *Core) clearBondRefundState(key string) {
	c.bondRefundsMtx.Lock()
	defer c.bondRefundsMtx.Unlock()
	delete(c.bondRefunds, key)
}

type refundCheckResult int

const (
	// refundNotPending means no refund is being tracked. One should be
	// broadcast.
	refundNotPending refundCheckResult = iota
	// refundWaiting means a broadcast refund is not yet confirmed, but it's
	// too soon to retry.
	refundWaiting
	// refundConfirmed means one of the tracked refunds is mined.
	refundConfirmed
	// refundSpentElsewhere means the bond output is spent, but not by any of
	// the tracked refunds that we have seen confirmed.
	refundSpentElsewhere
	// refundRetry means the refund has not confirmed in time and the bond
	// output is unspent (or its status is unknown). It should be rebroadcast.
	refundRetry
)

// checkPendingBondRefund checks the status of a tracked bond refund.
func (c *Core) checkPendingBondRefund(ctx context.Context, checker asset.BondRefundChecker, st *bondRefundState,
	bond *db.Bond, bondIDStr string, now time.Time) (refundCheckResult, string) {

	if st == nil || len(st.refundCoins) == 0 {
		return refundNotPending, ""
	}
	var anyUnconfirmed bool
	for i, coinID := range st.refundCoins {
		confs, err := checker.BondRefundConfirmations(ctx, coinID)
		if err != nil {
			if !errors.Is(err, asset.CoinNotFoundError) {
				c.log.Errorf("Error checking confirmations of refund %s for bond %v: %v", st.refundStrs[i], bondIDStr, err)
			}
			continue
		}
		if confs > 0 {
			return refundConfirmed, st.refundStrs[i]
		}
		anyUnconfirmed = true
	}
	if now.Sub(st.lastSent) < bondRefundRetryInterval {
		return refundWaiting, ""
	}
	st.lastSpentCheck = now
	spent, err := checker.BondSpent(ctx, bond.Version, bond.CoinID, bond.Data)
	if err != nil {
		// Rebroadcasting a valid refund is harmless.
		c.log.Warnf("Unable to determine if bond %v is spent: %v", bondIDStr, err)
		return refundRetry, ""
	}
	if spent {
		if anyUnconfirmed && now.Sub(st.firstSent) < bondRefundSpentGrace {
			// The spender may be our own unconfirmed refund.
			c.log.Debugf("Bond %v output reported spent while our refund %v is unconfirmed. Waiting.",
				bondIDStr, st.refundStrs)
			return refundWaiting, ""
		}
		return refundSpentElsewhere, ""
	}
	return refundRetry, ""
}

// refundedBondCheck is a bond that is marked refunded in the DB, but which
// should be checked on chain to see if the bond output is actually spent.
// All fields are protected by the dexAccount's authMtx.
type refundedBondCheck struct {
	bond     *db.Bond
	attempts int
	nextTry  time.Time
	inFlight bool
}

// queueRefundedBondRechecks collects recently refunded bonds for the startup
// re-check. Bonds that an older client marked refunded without the refund
// actually being mined would otherwise be lost track of. The dexAccount's
// authMtx must be held.
func queueRefundedBondRechecks(acct *dexAccount, bonds []*db.Bond, now time.Time) {
	for _, b := range bonds {
		if !b.Refunded || b.KeyIndex == ^uint32(0) && len(b.RefundTx) == 0 {
			continue
		}
		if time.Unix(int64(b.LockTime), 0).Add(refundedBondRecheckWindow).Before(now) {
			continue
		}
		acct.refundedBondChecks = append(acct.refundedBondChecks, &refundedBondCheck{bond: b})
	}
}

func removeRefundedBondCheck(checks []*refundedBondCheck, chk *refundedBondCheck) []*refundedBondCheck {
	for i, ch := range checks {
		if ch == chk {
			return append(checks[:i:i], checks[i+1:]...)
		}
	}
	return checks
}

// recheckRefundedBonds checks, for wallets that support it, whether the bond
// outputs of recently refunded bonds are actually spent. Bonds whose outputs
// are still unspent are returned to the expiredBonds list so that they are
// refunded again. The checks are run in goroutines since they may take a while
// for SPV wallets.
func (c *Core) recheckRefundedBonds(dc *dexConnection) {
	now := time.Now()
	dc.acct.authMtx.RLock()
	var due []*refundedBondCheck
	for _, chk := range dc.acct.refundedBondChecks {
		if !chk.inFlight && !now.Before(chk.nextTry) {
			due = append(due, chk)
		}
	}
	dc.acct.authMtx.RUnlock()

	// Don't hold the authMtx while connecting to or querying wallets.
	for _, chk := range due {
		bond := chk.bond
		wallet, err := c.connectedWallet(bond.AssetID)
		if err != nil {
			continue // try again later
		}
		checker, ok := wallet.Wallet.(asset.BondRefundChecker)
		if !ok {
			dc.acct.authMtx.Lock()
			dc.acct.refundedBondChecks = removeRefundedBondCheck(dc.acct.refundedBondChecks, chk)
			dc.acct.authMtx.Unlock()
			continue
		}
		if err := wallet.checkPeersAndSyncStatus(); err != nil {
			continue // wait until synced so a recent refund is not missed
		}
		dc.acct.authMtx.Lock()
		if chk.inFlight {
			dc.acct.authMtx.Unlock()
			continue
		}
		chk.inFlight = true
		dc.acct.authMtx.Unlock()
		bondIDStr := coinIDString(bond.AssetID, bond.CoinID) + " (" + unbip(bond.AssetID) + ")"
		c.log.Infof("Checking whether refunded bond %v was actually spent.", bondIDStr)
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			ctx, cancel := context.WithTimeout(c.ctx, refundedBondRecheckTimeout)
			defer cancel()
			spent, err := checker.BondSpent(ctx, bond.Version, bond.CoinID, bond.Data)
			c.applyRefundedBondRecheck(dc, chk, bondIDStr, spent, err)
		}()
	}
}

func (c *Core) applyRefundedBondRecheck(dc *dexConnection, chk *refundedBondCheck, bondIDStr string, spent bool, err error) {
	dc.acct.authMtx.Lock()
	defer dc.acct.authMtx.Unlock()
	chk.inFlight = false
	if err != nil {
		chk.attempts++
		if chk.attempts >= maxRefundedBondRechecks {
			c.log.Warnf("Giving up checking whether refunded bond %v is actually spent: %v", bondIDStr, err)
			dc.acct.refundedBondChecks = removeRefundedBondCheck(dc.acct.refundedBondChecks, chk)
			return
		}
		c.log.Warnf("Unable to check whether refunded bond %v is actually spent (will retry): %v", bondIDStr, err)
		chk.nextTry = time.Now().Add(refundedBondRecheckRetry)
		return
	}
	dc.acct.refundedBondChecks = removeRefundedBondCheck(dc.acct.refundedBondChecks, chk)
	if spent {
		c.log.Debugf("Refunded bond %v is confirmed spent.", bondIDStr)
		return
	}
	for _, b := range dc.acct.expiredBonds {
		if b.AssetID == chk.bond.AssetID && bytes.Equal(b.CoinID, chk.bond.CoinID) {
			return // already being refunded
		}
	}
	bond := *chk.bond
	bond.Refunded = false
	dc.acct.expiredBonds = append(dc.acct.expiredBonds, &bond)
	c.log.Warnf("Bond %v was marked refunded, but its output is still unspent. Returning it to the refund queue.", bondIDStr)
	c.triggerBondRotation()
}
