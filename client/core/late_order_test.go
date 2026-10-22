package core

import (
	"errors"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/msgjson"
	"decred.org/dcrdex/dex/order"
)

func TestLateOrderResponse(t *testing.T) {
	defer func(to, grace time.Duration) {
		tradeRequestTimeout, lateOrderGrace = to, grace
	}(tradeRequestTimeout, lateOrderGrace)
	tradeRequestTimeout = 50 * time.Millisecond
	lateOrderGrace = time.Minute // expiry is driven by the test

	rig := newTestRig()
	defer rig.shutdown()
	tCore := rig.core
	dcrWallet, tDcrWallet := newTWallet(tUTXOAssetA.ID)
	tCore.wallets[tUTXOAssetA.ID] = dcrWallet
	dcrWallet.address = "DsVmA7aqqWeKWy461hXjytbZbgCqbB8g2dq"
	dcrWallet.Unlock(rig.crypter)
	btcWallet, _ := newTWallet(tUTXOAssetB.ID)
	tCore.wallets[tUTXOAssetB.ID] = btcWallet
	btcWallet.address = "12DXGkvxFjuq5btXYkwWfBZaz1rVwFgini"
	btcWallet.Unlock(rig.crypter)

	qty := dcrBtcLotSize * 2
	form := &TradeForm{
		Host:    tDexHost,
		IsLimit: true,
		Sell:    true,
		Base:    tUTXOAssetA.ID,
		Quote:   tUTXOAssetB.ID,
		Qty:     qty,
		Rate:    dcrBtcRateStep * 1000,
	}

	type heldOrder struct {
		commit  order.Commitment
		oid     order.OrderID
		deliver func()
	}
	// placeTimedOut places an order that the server does not answer in
	// time. The server's late response can be delivered with deliver.
	placeTimedOut := func() *heldOrder {
		t.Helper()
		tDcrWallet.fundingCoins = asset.Coins{&tCoin{id: encode.RandomBytes(36), val: qty * 2}}
		tDcrWallet.fundRedeemScripts = []dex.Bytes{nil}
		tDcrWallet.returnedCoins = nil
		held := make(chan *heldOrder, 1)
		rig.ws.queueResponse(msgjson.LimitRoute, func(msg *msgjson.Message, f msgFunc) error {
			msgOrder := new(msgjson.LimitOrder)
			if err := msg.Unmarshal(msgOrder); err != nil {
				t.Errorf("unmarshal error: %v", err)
			}
			lo := convertMsgLimitOrder(msgOrder)
			resp := orderResponse(msg.ID, msgOrder, lo, false, false, false)
			h := &heldOrder{oid: lo.ID(), deliver: func() { f(resp) }}
			copy(h.commit[:], msgOrder.Commit)
			held <- h
			return nil
		})
		if _, err := tCore.Trade(tPW, form); !errors.Is(err, ErrTradeRequestTimeout) {
			t.Fatalf("expected ErrTradeRequestTimeout, got %v", err)
		}
		h := <-held
		if tDcrWallet.returnedCoins != nil {
			t.Fatalf("funding coins returned while a late response may still arrive")
		}
		tCore.sentCommitsMtx.Lock()
		_, found := tCore.sentCommits[h.commit]
		tCore.sentCommitsMtx.Unlock()
		if !found {
			t.Fatalf("commitment forgotten after timeout")
		}
		return h
	}

	notes := tCore.NotificationFeed()
	waitNote := func(topic Topic) *OrderNote {
		t.Helper()
		timeout := time.After(2 * time.Second)
		for {
			select {
			case n := <-notes.C:
				if on, ok := n.(*OrderNote); ok && n.Topic() == topic {
					return on
				}
			case <-timeout:
				t.Fatalf("no %s note", topic)
			}
		}
	}

	preimageRequest := func(h *heldOrder) *msgjson.Message {
		req, _ := msgjson.NewRequest(rig.dc.NextID(), msgjson.PreimageRoute, &msgjson.PreimageRequest{
			OrderID:    h.oid[:],
			Commitment: h.commit[:],
		})
		return req
	}

	// 1. A late acceptance is adopted, and the preimage request that
	// follows it is answered.
	h := placeTimedOut()
	h.deliver()
	note := waitNote(TopicLateOrderAdopted)
	if !note.Order.ID.Equal(h.oid[:]) {
		t.Fatalf("adopted the wrong order")
	}
	if tracker, _ := rig.dc.findOrder(h.oid); tracker == nil {
		t.Fatalf("adopted order is not tracked")
	}
	if tDcrWallet.returnedCoins != nil {
		t.Fatalf("funding coins of adopted order were returned")
	}
	if err := handlePreimageRequest(tCore, rig.dc, preimageRequest(h)); err != nil {
		t.Fatalf("preimage request error: %v", err)
	}
	waitNote(TopicPreimageSent)

	// 2. A preimage request that arrives just before the late response
	// waits for it and is then answered.
	h = placeTimedOut()
	if err := handlePreimageRequest(tCore, rig.dc, preimageRequest(h)); err != nil {
		t.Fatalf("preimage request error: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	h.deliver()
	waitNote(TopicLateOrderAdopted)
	waitNote(TopicPreimageSent)

	// 3. No response at all: the coins are released once the request
	// expires.
	placeTimedOut()
	rig.ws.mtx.Lock()
	expire := rig.ws.lastExpire
	rig.ws.mtx.Unlock()
	expire()
	deadline := time.Now().Add(2 * time.Second)
	for {
		tDcrWallet.fundingMtx.RLock()
		returned := tDcrWallet.returnedCoins != nil
		tDcrWallet.fundingMtx.RUnlock()
		if returned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("funding coins not returned after the request expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
