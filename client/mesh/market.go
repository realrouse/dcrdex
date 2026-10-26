// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	mc "github.com/bisoncraft/mesh/client"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
)

// DefaultRebroadcastInterval is how often own standing orders are re-broadcast.
const DefaultRebroadcastInterval = 15 * time.Second

// pubSub is the part of the mesh client used for order book gossip.
type pubSub interface {
	Broadcast(ctx context.Context, topic string, data []byte) error
	Subscribe(ctx context.Context, topic string, handler mc.TopicHandler) error
}

type ownOrder struct {
	ord   *MeshOrder
	topic string
}

// OrderBooks manages market subscriptions, the books seen on them, and this
// client's own standing orders. Own orders are re-broadcast periodically and
// whenever a new peer subscribes to the market, so late subscribers get the
// current book.
type OrderBooks struct {
	ps   pubSub
	priv libp2pcrypto.PrivKey
	log  dex.Logger
	now  func() time.Time

	mtx   sync.RWMutex
	books map[Market]*Book
	own   map[OrderID]*ownOrder
}

// NewOrderBooks constructs an OrderBooks.
func NewOrderBooks(ps pubSub, priv libp2pcrypto.PrivKey, log dex.Logger) *OrderBooks {
	return &OrderBooks{
		ps:    ps,
		priv:  priv,
		log:   log,
		now:   time.Now,
		books: make(map[Market]*Book),
		own:   make(map[OrderID]*ownOrder),
	}
}

// SubscribeMarket subscribes to the market's order book topic. It is a no-op
// if already subscribed.
func (ob *OrderBooks) SubscribeMarket(ctx context.Context, m Market) error {
	topic, err := m.Topic()
	if err != nil {
		return err
	}
	ob.mtx.Lock()
	if _, found := ob.books[m]; found {
		ob.mtx.Unlock()
		return nil
	}
	book := NewBook(m)
	ob.books[m] = book
	ob.mtx.Unlock()

	err = ob.ps.Subscribe(ctx, topic, func(ev mc.TopicEvent) {
		ob.handleEvent(ctx, m, topic, book, ev)
	})
	if err != nil && !errors.Is(err, mc.ErrRedundantSubscription) {
		// The registration may still be retried by the mesh client on
		// reconnect, so keep the book.
		ob.log.Warnf("Error subscribing to %s: %v", topic, err)
	}
	ob.log.Infof("Subscribed to mesh order book %s", topic)
	return nil
}

func (ob *OrderBooks) handleEvent(ctx context.Context, m Market, topic string, book *Book, ev mc.TopicEvent) {
	switch ev.Type {
	case mc.TopicEventPeerSubscribed:
		ob.log.Debugf("Peer %s subscribed to %s. Sending own orders.", ev.Peer, topic)
		ob.broadcastOwn(ctx, &m)
		return
	case mc.TopicEventData:
	default:
		return
	}
	msg, err := decodeBookMsg(ev.Data)
	if err != nil {
		ob.log.Debugf("Bad order book message on %s from %s: %v", topic, ev.Peer, err)
		return
	}
	now := ob.now()
	switch msg.Type {
	case msgTypeOrder:
		isNew, err := book.AddOrder(msg.Order, ev.Peer, now)
		if err != nil {
			ob.log.Debugf("Rejected order from %s on %s: %v", ev.Peer, topic, err)
			return
		}
		if isNew {
			ob.log.Infof("Mesh order %s added to %s (owner %s)", msg.Order.ID(), topic, msg.Order.Owner)
		}
	case msgTypeCancel:
		removed, err := book.Cancel(msg.Cancel, ev.Peer, now)
		if err != nil {
			ob.log.Debugf("Rejected cancel from %s on %s: %v", ev.Peer, topic, err)
			return
		}
		if removed {
			ob.log.Infof("Mesh order %s cancelled on %s", msg.Cancel.OrderID, topic)
		}
	}
}

// PostOrder signs a new standing order, adds it to the local book and
// broadcasts it. The market is subscribed if it isn't already.
func (ob *OrderBooks) PostOrder(ctx context.Context, m Market, sell bool, qty, rate uint64, lifetime time.Duration) (*MeshOrder, error) {
	if lifetime <= 0 || lifetime > MaxOrderLifetime {
		return nil, fmt.Errorf("lifetime must be in (0, %s]", MaxOrderLifetime)
	}
	topic, err := m.Topic()
	if err != nil {
		return nil, err
	}
	if err := ob.SubscribeMarket(ctx, m); err != nil {
		return nil, err
	}
	now := ob.now()
	ord := &MeshOrder{
		Market: m,
		Sell:   sell,
		Qty:    qty,
		Rate:   rate,
		Stamp:  now.UnixMilli(),
		Expiry: now.Add(lifetime).UnixMilli(),
		Nonce:  binary.BigEndian.Uint64(encode.RandomBytes(8)),
	}
	if err := ord.Sign(ob.priv); err != nil {
		return nil, err
	}
	ob.mtx.Lock()
	book := ob.books[m]
	ob.own[ord.ID()] = &ownOrder{ord: ord, topic: topic}
	ob.mtx.Unlock()
	if _, err := book.AddOrder(ord, "", now); err != nil {
		return nil, err
	}
	ob.broadcast(ctx, topic, &bookMsg{Type: msgTypeOrder, Order: ord})
	return ord, nil
}

// CancelOrder cancels one of this client's own standing orders.
func (ob *OrderBooks) CancelOrder(ctx context.Context, id OrderID) error {
	ob.mtx.Lock()
	oo, found := ob.own[id]
	if found {
		delete(ob.own, id)
	}
	var book *Book
	if found {
		book = ob.books[oo.ord.Market]
	}
	ob.mtx.Unlock()
	if !found {
		return fmt.Errorf("no own order %s", id)
	}
	c := &MeshCancel{OrderID: id, Stamp: ob.now().UnixMilli()}
	if err := c.Sign(ob.priv); err != nil {
		return err
	}
	if _, err := book.Cancel(c, "", ob.now()); err != nil {
		return err
	}
	ob.broadcast(ctx, oo.topic, &bookMsg{Type: msgTypeCancel, Cancel: c})
	return nil
}

// Book returns the live orders seen on the market.
func (ob *OrderBooks) Book(m Market) ([]*MeshOrder, error) {
	ob.mtx.RLock()
	book, found := ob.books[m]
	ob.mtx.RUnlock()
	if !found {
		return nil, errors.New("not subscribed to market")
	}
	return book.Orders(ob.now()), nil
}

// OwnOrders returns this client's live standing orders.
func (ob *OrderBooks) OwnOrders() []*MeshOrder {
	nowMs := ob.now().UnixMilli()
	ob.mtx.RLock()
	defer ob.mtx.RUnlock()
	ords := make([]*MeshOrder, 0, len(ob.own))
	for _, oo := range ob.own {
		if oo.ord.Expiry > nowMs {
			ords = append(ords, oo.ord)
		}
	}
	return ords
}

func (ob *OrderBooks) broadcast(ctx context.Context, topic string, msg *bookMsg) {
	b, err := encodeBookMsg(msg)
	if err != nil {
		ob.log.Errorf("Error encoding %s message: %v", msg.Type, err)
		return
	}
	if err := ob.ps.Broadcast(ctx, topic, b); err != nil {
		ob.log.Warnf("Error broadcasting %s on %s (will retry on rebroadcast): %v", msg.Type, topic, err)
	}
}

// broadcastOwn re-broadcasts own live orders, for market m only if non-nil.
func (ob *OrderBooks) broadcastOwn(ctx context.Context, m *Market) {
	nowMs := ob.now().UnixMilli()
	ob.mtx.RLock()
	toSend := make([]*ownOrder, 0, len(ob.own))
	for _, oo := range ob.own {
		if oo.ord.Expiry > nowMs && (m == nil || oo.ord.Market == *m) {
			toSend = append(toSend, oo)
		}
	}
	ob.mtx.RUnlock()
	for _, oo := range toSend {
		ob.broadcast(ctx, oo.topic, &bookMsg{Type: msgTypeOrder, Order: oo.ord})
	}
}

// maintain prunes expired orders and own orders, and re-broadcasts own live
// orders.
func (ob *OrderBooks) maintain(ctx context.Context) {
	now := ob.now()
	nowMs := now.UnixMilli()
	ob.mtx.Lock()
	for id, oo := range ob.own {
		if oo.ord.Expiry <= nowMs {
			ob.log.Infof("Own mesh order %s expired", id)
			delete(ob.own, id)
		}
	}
	books := make([]*Book, 0, len(ob.books))
	for _, b := range ob.books {
		books = append(books, b)
	}
	ob.mtx.Unlock()
	for _, b := range books {
		b.Prune(now)
	}
	ob.broadcastOwn(ctx, nil)
}

// RunMaintenance runs the maintenance loop until ctx is done.
func (ob *OrderBooks) RunMaintenance(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ob.maintain(ctx)
		case <-ctx.Done():
			return
		}
	}
}
