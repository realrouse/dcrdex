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
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// DefaultRebroadcastInterval is how often own standing orders are re-broadcast.
const DefaultRebroadcastInterval = 15 * time.Second

// pubSub is the part of the mesh client used for order book gossip and inbox
// messages.
type pubSub interface {
	Broadcast(ctx context.Context, topic string, data []byte) error
	Subscribe(ctx context.Context, topic string, handler mc.TopicHandler) error
}

type ownOrder struct {
	ord        *MeshOrder
	topic      string
	remaining  uint64 // not yet filled (agreed matches are filled)
	reserved   uint64 // accepted by us, awaiting the taker's confirm
	lastUpdate *MeshOrderUpdate
}

func (oo *ownOrder) available() uint64 { return oo.remaining - oo.reserved }

// NegotiationConfig holds match negotiation timing and swap policy.
type NegotiationConfig struct {
	// RetryInterval is how often unanswered proposals/accepts are re-sent.
	RetryInterval time.Duration
	// ProposalTimeout is how long a taker waits for an accept or reject.
	ProposalTimeout time.Duration
	// ReservationTimeout is how long a maker holds quantity for an accepted
	// match while waiting for the taker's confirm.
	ReservationTimeout time.Duration
	Fees               FeeParameters
}

// DefaultNegotiationConfig is for simnet.
var DefaultNegotiationConfig = NegotiationConfig{
	RetryInterval:      2 * time.Second,
	ProposalTimeout:    20 * time.Second,
	ReservationTimeout: 20 * time.Second,
	Fees:               FeeParameters{MaxFeeExposure: 1, BaseFeesPerMatch: 1, QuoteFeesPerMatch: 1},
}

// OrderBooks manages market subscriptions, the books seen on them, this
// client's own standing orders, and match negotiation over the encrypted
// inbox. Own orders are re-broadcast periodically and whenever a new peer
// subscribes to the market, so late subscribers get the current book.
type OrderBooks struct {
	ps      pubSub
	priv    libp2pcrypto.PrivKey
	encPriv *secp256k1.PrivateKey
	self    peer.ID
	log     dex.Logger
	now     func() time.Time
	cfg     NegotiationConfig
	// addr returns our receiving address for an asset (for swap params).
	addr func(assetID uint32) (string, error)

	mtx     sync.RWMutex
	books   map[Market]*Book
	own     map[OrderID]*ownOrder
	matches map[MatchID]*matchState
	swapper *swapper // nil until EnableSwaps
	// policies are lock policy overrides (simnet debug API).
	policies map[Market]LockPolicy
	// guard is the inbox replay cache and rate limit.
	guard *inboxGuard
}

// NewOrderBooks constructs an OrderBooks.
func NewOrderBooks(ps pubSub, priv libp2pcrypto.PrivKey, encPriv *secp256k1.PrivateKey, log dex.Logger) *OrderBooks {
	self, _ := peer.IDFromPrivateKey(priv)
	return &OrderBooks{
		ps:       ps,
		priv:     priv,
		encPriv:  encPriv,
		self:     self,
		log:      log,
		now:      time.Now,
		cfg:      DefaultNegotiationConfig,
		addr:     func(uint32) (string, error) { return "", errors.New("no address source") },
		books:    make(map[Market]*Book),
		own:      make(map[OrderID]*ownOrder),
		matches:  make(map[MatchID]*matchState),
		policies: make(map[Market]LockPolicy),
		guard:    newInboxGuard(),
	}
}

// SetAddressFunc sets the receiving address source used for swap params.
func (ob *OrderBooks) SetAddressFunc(f func(assetID uint32) (string, error)) {
	ob.mtx.Lock()
	ob.addr = f
	ob.mtx.Unlock()
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
	if ev.Peer == ob.self {
		return // our own gossip echoed back
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
	case msgTypeUpdate:
		changed, err := book.Update(msg.Update, ev.Peer, now)
		if err != nil {
			ob.log.Debugf("Rejected update from %s on %s: %v", ev.Peer, topic, err)
			return
		}
		if changed {
			ob.log.Infof("Mesh order %s on %s now has %d remaining", msg.Update.OrderID, topic, msg.Update.Remaining)
		}
	}
}

// PostOrder signs a new standing order, adds it to the local book and
// broadcasts it. The market is subscribed if it isn't already. A zero
// lotSize means a single lot of qty.
func (ob *OrderBooks) PostOrder(ctx context.Context, m Market, sell bool, qty, lotSize, rate uint64, lifetime time.Duration) (*MeshOrder, error) {
	if lifetime <= 0 || lifetime > MaxOrderLifetime {
		return nil, fmt.Errorf("lifetime must be in (0, %s]", MaxOrderLifetime)
	}
	if lotSize == 0 {
		lotSize = qty
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
		Market:    m,
		Sell:      sell,
		Qty:       qty,
		LotSize:   lotSize,
		Rate:      rate,
		Stamp:     now.UnixMilli(),
		Expiry:    now.Add(lifetime).UnixMilli(),
		Nonce:     binary.BigEndian.Uint64(encode.RandomBytes(8)),
		EncPubKey: ob.encPriv.PubKey().SerializeCompressed(),
	}
	if err := ord.Sign(ob.priv); err != nil {
		return nil, err
	}
	ob.mtx.Lock()
	book := ob.books[m]
	ob.mtx.Unlock()
	if _, err := book.AddOrder(ord, "", now); err != nil {
		return nil, err
	}
	ob.mtx.Lock()
	ob.own[ord.ID()] = &ownOrder{ord: ord, topic: topic, remaining: qty}
	ob.mtx.Unlock()
	ob.broadcast(ctx, topic, &bookMsg{Type: msgTypeOrder, Order: ord})
	return ord, nil
}

// CancelOrder cancels one of this client's own standing orders. Matches
// already accepted are still honored.
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

// fillOwn records a filled quantity on an own order and broadcasts the
// remaining-quantity update. The caller must not hold ob.mtx.
func (ob *OrderBooks) fillOwn(ctx context.Context, id OrderID, qty uint64, wasReserved bool) {
	ob.mtx.Lock()
	oo, found := ob.own[id]
	var book *Book
	var upd *MeshOrderUpdate
	if found {
		if wasReserved {
			oo.reserved -= qty
		}
		oo.remaining -= qty
		upd = &MeshOrderUpdate{OrderID: id, Remaining: oo.remaining, Stamp: ob.now().UnixMilli()}
		if err := upd.Sign(ob.priv); err != nil {
			ob.mtx.Unlock()
			ob.log.Errorf("Error signing order update: %v", err)
			return
		}
		oo.lastUpdate = upd
		if oo.remaining == 0 {
			delete(ob.own, id)
		}
		book = ob.books[oo.ord.Market]
	}
	ob.mtx.Unlock()
	if !found {
		// Cancelled while the match was pending. The book entry is already gone.
		return
	}
	if _, err := book.Update(upd, "", ob.now()); err != nil {
		ob.log.Errorf("Error applying own order update: %v", err)
	}
	ob.broadcast(ctx, oo.topic, &bookMsg{Type: msgTypeUpdate, Update: upd})
}

// Book returns the live orders seen on the market.
func (ob *OrderBooks) Book(m Market) ([]*BookOrder, error) {
	ob.mtx.RLock()
	book, found := ob.books[m]
	ob.mtx.RUnlock()
	if !found {
		return nil, errors.New("not subscribed to market")
	}
	return book.Orders(ob.now()), nil
}

// OwnOrders returns this client's live standing orders.
func (ob *OrderBooks) OwnOrders() []*BookOrder {
	nowMs := ob.now().UnixMilli()
	ob.mtx.RLock()
	defer ob.mtx.RUnlock()
	ords := make([]*BookOrder, 0, len(ob.own))
	for _, oo := range ob.own {
		if oo.ord.Expiry > nowMs {
			ords = append(ords, &BookOrder{MeshOrder: oo.ord, Remaining: oo.remaining})
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

// broadcastOwn re-broadcasts own live orders (and their latest fill update),
// for market m only if non-nil.
func (ob *OrderBooks) broadcastOwn(ctx context.Context, m *Market) {
	nowMs := ob.now().UnixMilli()
	type send struct {
		topic string
		ord   *MeshOrder
		upd   *MeshOrderUpdate
	}
	ob.mtx.RLock()
	toSend := make([]send, 0, len(ob.own))
	for _, oo := range ob.own {
		if oo.ord.Expiry > nowMs && (m == nil || oo.ord.Market == *m) {
			toSend = append(toSend, send{oo.topic, oo.ord, oo.lastUpdate})
		}
	}
	ob.mtx.RUnlock()
	for _, s := range toSend {
		ob.broadcast(ctx, s.topic, &bookMsg{Type: msgTypeOrder, Order: s.ord})
		if s.upd != nil {
			ob.broadcast(ctx, s.topic, &bookMsg{Type: msgTypeUpdate, Update: s.upd})
		}
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
