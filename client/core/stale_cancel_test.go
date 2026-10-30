package core

import (
	"testing"
	"time"

	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
)

// newStaleCancelRig sets up a booked standing sell order with a cancel order
// placed cancelAge ago.
func newStaleCancelRig(t *testing.T, cancelAge time.Duration) (*testRig, *trackedTrade, *order.CancelOrder) {
	t.Helper()
	rig := newTestRig()
	dc := rig.dc
	tCore := rig.core
	dcrWallet, _ := newTWallet(tUTXOAssetA.ID)
	tCore.wallets[tUTXOAssetA.ID] = dcrWallet
	btcWallet, tBtcWallet := newTWallet(tUTXOAssetB.ID)
	tCore.wallets[tUTXOAssetB.ID] = btcWallet
	tBtcWallet.redemptionAddr = "somenonemptyaddress"
	tBtcWallet.validAddr = true
	walletSet, _, _, err := tCore.walletSet(dc, tUTXOAssetA.ID, tUTXOAssetB.ID, true)
	if err != nil {
		t.Fatalf("walletSet error: %v", err)
	}

	qty := dcrBtcLotSize * 10
	lo, dbOrder, preImg, _ := makeLimitOrderWithTiF(dc, true, qty, dcrBtcRateStep*100, order.StandingTiF)
	dbOrder.MetaData.Status = order.OrderStatusBooked
	tracker := newTrackedTrade(dbOrder, preImg, dc, tCore.lockTimeTaker, tCore.lockTimeMaker,
		rig.db, rig.queue, walletSet, nil, tCore.notify, tCore.formatDetails, &tCore.wg)
	oid := lo.ID()
	dc.tradeMtx.Lock()
	dc.trades[oid] = tracker
	dc.tradeMtx.Unlock()

	cancelPreImg := newPreimage()
	stamp := time.Now().Add(-cancelAge)
	co := &order.CancelOrder{
		P: order.Prefix{
			AccountID:  dc.acct.ID(),
			BaseAsset:  tUTXOAssetA.ID,
			QuoteAsset: tUTXOAssetB.ID,
			OrderType:  order.CancelOrderType,
			ClientTime: stamp,
			ServerTime: stamp,
			Commit:     cancelPreImg.Commit(),
		},
		TargetOrderID: oid,
	}
	tracker.mtx.Lock()
	if err := tracker.cancelTrade(co, cancelPreImg, tracker.epochLen()); err != nil {
		t.Fatalf("cancelTrade error: %v", err)
	}
	tracker.mtx.Unlock()
	return rig, tracker, co
}

// cancelMatches returns the server's taker (cancel order) and maker (trade)
// match messages for a cancel of tracker's order.
func cancelMatches(tracker *trackedTrade, co *order.CancelOrder) (taker, maker *msgjson.Match) {
	mid := encode.RandomBytes(32)
	cid, oid := co.ID(), tracker.ID()
	taker = &msgjson.Match{OrderID: cid[:], MatchID: mid, Quantity: tracker.Trade().Remaining()}
	maker = &msgjson.Match{OrderID: oid[:], MatchID: mid, Quantity: tracker.Trade().Remaining()}
	sign(tDexPriv, taker)
	sign(tDexPriv, maker)
	return
}

func TestStaleCancelGraceAfterPreimage(t *testing.T) {
	// Epoch length in the test market.
	rig, tracker, _ := newStaleCancelRig(t, 0)
	epochLen := time.Duration(tracker.epochLen()) * time.Millisecond
	rig.shutdown()

	// Just past the preimage request timeout, a cancel whose preimage was
	// never requested is stale.
	age := preimageReqTimeout + epochLen + 2*time.Second
	rig, tracker, _ = newStaleCancelRig(t, age)
	defer rig.shutdown()
	tracker.mtx.RLock()
	stale := tracker.hasStaleCancelOrder()
	tracker.mtx.RUnlock()
	if !stale {
		t.Fatalf("cancel without a preimage sent not stale after %v", age)
	}

	// Once we sent its preimage, the server will match or nomatch it, so
	// it is given more time.
	tracker.csumMtx.Lock()
	tracker.cancelCsum = encode.RandomBytes(32)
	tracker.csumMtx.Unlock()
	tracker.mtx.RLock()
	stale = tracker.hasStaleCancelOrder()
	tracker.mtx.RUnlock()
	if stale {
		t.Fatalf("cancel with preimage sent considered stale after only %v", age)
	}
}

func TestCancelMatchAfterStaleRetire(t *testing.T) {
	// The race from the log: the cancel match was parsed while the cancel
	// order was still tracked, then a tick retired the cancel as stale
	// before the match was processed.
	rig, tracker, co := newStaleCancelRig(t, time.Hour)
	defer rig.shutdown()
	taker, maker := cancelMatches(tracker, co)

	tracker.mtx.Lock()
	tracker.deleteStaleCancelOrder()
	if tracker.cancel != nil {
		t.Fatalf("cancel not retired")
	}
	tracker.mtx.Unlock()

	_, err := rig.core.negotiateMatches(&serverMatches{
		tracker:    tracker,
		msgMatches: []*msgjson.Match{maker},
		cancel:     taker,
	})
	if err != nil {
		t.Fatalf("negotiateMatches error: %v", err)
	}
	tracker.mtx.RLock()
	status, nMatches := tracker.metaData.Status, len(tracker.matches)
	tracker.mtx.RUnlock()
	if status != order.OrderStatusCanceled {
		t.Fatalf("order status %s after cancel match, expected canceled", status)
	}
	if nMatches != 0 {
		t.Fatalf("cancel match negotiated as a trade match")
	}
	if rig.db.lastStatusID != co.ID() || rig.db.lastStatus != order.OrderStatusExecuted {
		t.Fatalf("cancel order not set executed")
	}
}

func TestCancelMatchAfterStaleRetireViaMatchRoute(t *testing.T) {
	// The cancel was retired as stale before the match request arrived.
	// Both sides of the cancel match still cancel the order.
	rig, tracker, co := newStaleCancelRig(t, time.Hour)
	defer rig.shutdown()
	taker, maker := cancelMatches(tracker, co)

	tracker.mtx.Lock()
	tracker.deleteStaleCancelOrder()
	tracker.mtx.Unlock()

	req, _ := msgjson.NewRequest(1, msgjson.MatchRoute, []*msgjson.Match{taker, maker})
	if err := handleMatchRoute(rig.core, rig.dc, req); err != nil {
		t.Fatalf("handleMatchRoute error: %v", err)
	}
	tracker.mtx.RLock()
	status, nMatches := tracker.metaData.Status, len(tracker.matches)
	tracker.mtx.RUnlock()
	if status != order.OrderStatusCanceled || nMatches != 0 {
		t.Fatalf("expected canceled with no trade matches, got %s with %d", status, nMatches)
	}
}

func TestCancelUnknownOrderRetires(t *testing.T) {
	rig, tracker, _ := newStaleCancelRig(t, time.Hour)
	defer rig.shutdown()
	// No cancel pending.
	tracker.mtx.Lock()
	tracker.deleteCancelOrder()
	tracker.mtx.Unlock()
	oid := tracker.ID()

	rig.queueCancel(msgjson.NewError(msgjson.UnknownOrderError, "target order not known: %v", oid))
	statusRequested := make(chan struct{}, 1)
	rig.ws.queueResponse(msgjson.OrderStatusRoute, func(msg *msgjson.Message, f msgFunc) error {
		statusRequested <- struct{}{}
		resp, _ := msgjson.NewResponse(msg.ID, []*msgjson.OrderStatus{{
			ID:     oid[:],
			Status: uint16(order.OrderStatusCanceled),
		}}, nil)
		f(resp)
		return nil
	})

	if err := rig.core.Cancel(oid[:]); err == nil {
		t.Fatalf("expected the cancel error to be returned")
	}
	select {
	case <-statusRequested:
	default:
		t.Fatalf("order status not requested after unknown order error")
	}
	tracker.mtx.RLock()
	status := tracker.metaData.Status
	tracker.mtx.RUnlock()
	if status != order.OrderStatusCanceled {
		t.Fatalf("order not retired to the server's status, got %s", status)
	}
}
