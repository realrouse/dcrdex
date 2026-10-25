//go:build !harness && !botlive

package core

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/db"
	"decred.org/dcrdex/dex/encode"
)

// TBondChecker is a TXCWallet that implements asset.BondRefundChecker.
type TBondChecker struct {
	*TXCWallet
	mtx        sync.Mutex
	confs      map[string]uint32 // refund coin ID hex -> confs, missing = not found
	spent      bool
	spentErr   error
	spentCalls int
}

func (w *TBondChecker) BondRefundConfirmations(_ context.Context, refundCoinID []byte) (uint32, error) {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	confs, found := w.confs[hex.EncodeToString(refundCoinID)]
	if !found {
		return 0, asset.CoinNotFoundError
	}
	return confs, nil
}

func (w *TBondChecker) BondSpent(_ context.Context, ver uint16, bondCoinID, bondScript []byte) (bool, error) {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	w.spentCalls++
	return w.spent, w.spentErr
}

func (w *TBondChecker) setConfs(coinID []byte, confs uint32) {
	w.mtx.Lock()
	w.confs[hex.EncodeToString(coinID)] = confs
	w.mtx.Unlock()
}

func newBondRefundRig(t *testing.T) (*testRig, *TBondChecker, *xcWallet) {
	t.Helper()
	rig := newTestRig()
	t.Cleanup(rig.shutdown)
	xcw, tw := newTWallet(tUTXOAssetA.ID)
	checker := &TBondChecker{TXCWallet: tw, confs: make(map[string]uint32)}
	xcw.Wallet = checker
	rig.core.wallets[tUTXOAssetA.ID] = xcw
	rig.core.Login(tPW)
	rig.dc.acct.isAuthed = true
	tw.contractExpired = true // LockTimeExpired
	return rig, checker, xcw
}

func newRefundableBond() *db.Bond {
	return &db.Bond{
		AssetID:  tUTXOAssetA.ID,
		CoinID:   encode.RandomBytes(36),
		Data:     encode.RandomBytes(32),
		Amount:   tFee,
		LockTime: uint64(time.Now().Add(-time.Minute).Unix()),
		KeyIndex: 0,
		RefundTx: encode.RandomBytes(100),
	}
}

func (rig *testRig) refundExpired(t *testing.T, now time.Time) {
	t.Helper()
	c, dc := rig.core, rig.dc
	cfg := c.dexBondConfig(dc, now.Unix())
	state := c.bondStateOfDEX(dc, cfg)
	if _, _, err := c.refundExpiredBonds(rig.core.ctx, dc.acct, cfg, state, now.Unix()); err != nil {
		t.Fatalf("refundExpiredBonds error: %v", err)
	}
}

func (rig *testRig) numExpired() int {
	rig.dc.acct.authMtx.RLock()
	defer rig.dc.acct.authMtx.RUnlock()
	return len(rig.dc.acct.expiredBonds)
}

func TestBondRefundPendingAndRetry(t *testing.T) {
	rig, checker, _ := newBondRefundRig(t)
	bond := newRefundableBond()
	rig.dc.acct.expiredBonds = []*db.Bond{bond}
	key := bondRefundKey(rig.dc.acct.host, bond)

	refund1 := &tCoin{id: encode.RandomBytes(36), val: tFee - 100}
	checker.refundBondCoin = refund1

	now := time.Now()
	// First attempt broadcasts, but the bond stays in expiredBonds and is
	// tracked as refund-pending.
	rig.refundExpired(t, now)
	if rig.numExpired() != 1 {
		t.Fatal("bond dropped from expiredBonds after broadcast")
	}
	st := rig.core.bondRefundState(key)
	if st == nil || len(st.refundCoins) != 1 || st.attempts != 1 {
		t.Fatalf("refund not tracked: %+v", st)
	}

	// Before the retry interval, with no confirmations, nothing happens. In
	// particular, no new refund is generated.
	checker.refundBondCoin = &tCoin{id: encode.RandomBytes(36), val: tFee - 100}
	rig.refundExpired(t, now.Add(bondRefundRetryInterval/2))
	if st.attempts != 1 || rig.numExpired() != 1 || checker.spentCalls != 0 {
		t.Fatalf("unexpected action before retry interval: attempts %d, spent calls %d", st.attempts, checker.spentCalls)
	}

	// After the interval, the refund is unconfirmed (unknown), and the bond
	// output is unspent, so it is rebroadcast.
	refund2 := checker.refundBondCoin.(*tCoin)
	rig.refundExpired(t, now.Add(bondRefundRetryInterval+time.Second))
	if checker.spentCalls != 1 {
		t.Fatalf("expected a bond spent check, got %d", checker.spentCalls)
	}
	if st.attempts != 2 || len(st.refundCoins) != 2 || rig.numExpired() != 1 {
		t.Fatalf("refund not retried: %+v", st)
	}

	// The second refund confirms. The bond is marked refunded and dropped.
	checker.setConfs(refund2.id, 1)
	rig.refundExpired(t, now.Add(bondRefundRetryInterval+2*time.Second))
	if rig.numExpired() != 0 {
		t.Fatal("bond not dropped after refund confirmed")
	}
	if rig.core.bondRefundState(key) != nil {
		t.Fatal("refund state not cleared")
	}
}

func TestBondRefundConfirmedFirstTry(t *testing.T) {
	rig, checker, _ := newBondRefundRig(t)
	bond := newRefundableBond()
	rig.dc.acct.expiredBonds = []*db.Bond{bond}
	refund := &tCoin{id: encode.RandomBytes(36), val: tFee - 100}
	checker.refundBondCoin = refund
	now := time.Now()
	rig.refundExpired(t, now)
	checker.setConfs(refund.id, 0) // seen, unconfirmed
	rig.refundExpired(t, now.Add(time.Minute))
	if rig.numExpired() != 1 {
		t.Fatal("bond dropped before refund confirmed")
	}
	checker.setConfs(refund.id, 3)
	rig.refundExpired(t, now.Add(2*time.Minute))
	if rig.numExpired() != 0 {
		t.Fatal("bond not dropped after refund confirmed")
	}
}

func TestBondRefundSpentElsewhere(t *testing.T) {
	rig, checker, _ := newBondRefundRig(t)
	bond := newRefundableBond()
	rig.dc.acct.expiredBonds = []*db.Bond{bond}
	checker.refundBondCoin = &tCoin{id: encode.RandomBytes(36), val: tFee - 100}
	now := time.Now()
	rig.refundExpired(t, now)

	// The tracked refund is unknown to the wallet (e.g. rejected), and the
	// bond is spent by something else (e.g. the backup refund tx).
	checker.spent = true
	rig.refundExpired(t, now.Add(bondRefundRetryInterval+time.Second))
	if rig.numExpired() != 0 {
		t.Fatal("bond not dropped after it was spent elsewhere")
	}
}

func TestBondRefundSpentWhileOursUnconfirmed(t *testing.T) {
	rig, checker, _ := newBondRefundRig(t)
	bond := newRefundableBond()
	rig.dc.acct.expiredBonds = []*db.Bond{bond}
	refund := &tCoin{id: encode.RandomBytes(36), val: tFee - 100}
	checker.refundBondCoin = refund
	now := time.Now()
	rig.refundExpired(t, now)

	// A full node may report the output spent by our own mempool refund.
	// Wait for it rather than marking refunded or rebroadcasting.
	checker.setConfs(refund.id, 0)
	checker.spent = true
	rig.refundExpired(t, now.Add(bondRefundRetryInterval+time.Second))
	st := rig.core.bondRefundState(bondRefundKey(rig.dc.acct.host, bond))
	if rig.numExpired() != 1 || st == nil || st.attempts != 1 {
		t.Fatal("unexpected action while our refund is unconfirmed")
	}
	// After the grace period, give up waiting.
	rig.refundExpired(t, now.Add(bondRefundSpentGrace+time.Second))
	if rig.numExpired() != 0 {
		t.Fatal("bond not dropped after grace period")
	}
}

func TestBondRefundFailureSpentCheck(t *testing.T) {
	rig, checker, _ := newBondRefundRig(t)
	bond := newRefundableBond()
	rig.dc.acct.expiredBonds = []*db.Bond{bond}
	checker.refundBondErr = errors.New("missing inputs")
	now := time.Now()
	// Refund fails and the bond output is unspent. Keep it.
	rig.refundExpired(t, now)
	if rig.numExpired() != 1 || checker.spentCalls != 1 {
		t.Fatalf("unexpected state after failed refund, spent calls = %d", checker.spentCalls)
	}
	// The spent check is rate limited.
	rig.refundExpired(t, now.Add(time.Minute))
	if checker.spentCalls != 1 {
		t.Fatal("spent check not rate limited")
	}
	// Refund mined before a restart, so the bond output is spent.
	checker.spent = true
	rig.refundExpired(t, now.Add(bondRefundRetryInterval+time.Second))
	if rig.numExpired() != 0 {
		t.Fatal("bond not dropped when refund failed and output is spent")
	}
}

// TestBondRefundNoChecker checks the old behavior for wallets that can't
// verify refunds.
func TestBondRefundNoChecker(t *testing.T) {
	rig := newTestRig()
	defer rig.shutdown()
	xcw, tw := newTWallet(tUTXOAssetA.ID)
	rig.core.wallets[tUTXOAssetA.ID] = xcw
	rig.core.Login(tPW)
	tw.contractExpired = true
	tw.refundBondCoin = &tCoin{id: encode.RandomBytes(36)}
	rig.dc.acct.expiredBonds = []*db.Bond{newRefundableBond()}
	rig.refundExpired(t, time.Now())
	if rig.numExpired() != 0 {
		t.Fatal("bond not dropped for wallet without refund checker")
	}
}

func TestRecheckRefundedBonds(t *testing.T) {
	rig, checker, _ := newBondRefundRig(t)
	acct := rig.dc.acct

	unspentBond := newRefundableBond()
	unspentBond.Refunded = true
	oldBond := newRefundableBond()
	oldBond.Refunded = true
	oldBond.LockTime = uint64(time.Now().Add(-refundedBondRecheckWindow - time.Hour).Unix())
	liveBond := newRefundableBond() // not refunded, not for re-check

	acct.authMtx.Lock()
	queueRefundedBondRechecks(acct, []*db.Bond{unspentBond, oldBond, liveBond}, time.Now())
	n := len(acct.refundedBondChecks)
	acct.authMtx.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 bond queued for re-check, got %d", n)
	}

	waitChecks := func() {
		t.Helper()
		for i := 0; i < 200; i++ {
			acct.authMtx.RLock()
			var inFlight bool
			for _, chk := range acct.refundedBondChecks {
				inFlight = inFlight || chk.inFlight
			}
			acct.authMtx.RUnlock()
			if !inFlight {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("re-check did not finish")
	}

	// A check error keeps it queued for a later retry.
	checker.spentErr = errors.New("not synced")
	rig.core.recheckRefundedBonds(rig.dc)
	waitChecks()
	acct.authMtx.RLock()
	n = len(acct.refundedBondChecks)
	acct.authMtx.RUnlock()
	if n != 1 || rig.numExpired() != 0 {
		t.Fatal("failed check should be retried later")
	}
	// Not retried before nextTry.
	calls := checker.spentCalls
	rig.core.recheckRefundedBonds(rig.dc)
	waitChecks()
	if checker.spentCalls != calls {
		t.Fatal("re-check retried too soon")
	}

	// Unspent: the bond goes back to expiredBonds, not marked refunded.
	checker.spentErr = nil
	acct.authMtx.Lock()
	acct.refundedBondChecks[0].nextTry = time.Time{}
	acct.authMtx.Unlock()
	rig.core.recheckRefundedBonds(rig.dc)
	waitChecks()
	if rig.numExpired() != 1 {
		t.Fatal("unspent refunded bond not returned to expiredBonds")
	}
	acct.authMtx.RLock()
	b := acct.expiredBonds[0]
	n = len(acct.refundedBondChecks)
	acct.authMtx.RUnlock()
	if b.Refunded || n != 0 {
		t.Fatal("bad state after re-check")
	}
	if unspentBond.Refunded != true {
		t.Fatal("original db bond modified")
	}

	// It is then refunded through the normal flow.
	refund := &tCoin{id: encode.RandomBytes(36), val: tFee - 100}
	checker.refundBondCoin = refund
	rig.refundExpired(t, time.Now())
	checker.setConfs(refund.id, 1)
	rig.refundExpired(t, time.Now().Add(time.Minute))
	if rig.numExpired() != 0 {
		t.Fatal("re-queued bond not refunded")
	}

	// A spent refunded bond is just dropped.
	spentBond := newRefundableBond()
	spentBond.Refunded = true
	checker.spent = true
	acct.authMtx.Lock()
	queueRefundedBondRechecks(acct, []*db.Bond{spentBond}, time.Now())
	acct.authMtx.Unlock()
	rig.core.recheckRefundedBonds(rig.dc)
	waitChecks()
	acct.authMtx.RLock()
	n = len(acct.refundedBondChecks)
	acct.authMtx.RUnlock()
	if n != 0 || rig.numExpired() != 0 {
		t.Fatal("spent refunded bond not dropped")
	}
}
