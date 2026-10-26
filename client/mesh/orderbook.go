// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"github.com/decred/dcrd/crypto/blake256"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// OrderBookTopicPrefix is the prefix of mesh order book topics, e.g.
	// "orderbook.dcr_btc".
	OrderBookTopicPrefix = "orderbook."

	// MaxOrderLifetime is the longest an order may stay on the book without
	// being re-posted.
	MaxOrderLifetime = time.Hour
	// MaxClockSkew is how far into the future an order or cancel stamp may be.
	MaxClockSkew = time.Minute
	// MinSwapValue is the smallest amount of either asset a single lot may
	// move. It is a conservative dust floor for the supported UTXO assets
	// (DCR, BTC, LBC) at default relay fees, so an agreed match can always
	// be funded with non-dust contract outputs.
	MinSwapValue = 10_000
	// MaxOrdersPerOwner caps one owner's live orders on a book.
	MaxOrdersPerOwner = 100
	// maxTombstones caps cancels remembered for orders we never saw.
	maxTombstones = 10_000

	orderSigDomain  = "tatanka-mesh-order-v2"
	cancelSigDomain = "tatanka-mesh-cancel-v1"
	updateSigDomain = "tatanka-mesh-order-update-v1"

	msgTypeOrder  = "order"
	msgTypeCancel = "cancel"
	msgTypeUpdate = "update"
)

// OrderID is the blake256 hash of an order's serialized, signed fields.
type OrderID [32]byte

func (id OrderID) String() string { return hex.EncodeToString(id[:]) }

func (id OrderID) MarshalJSON() ([]byte, error) { return json.Marshal(id.String()) }

func (id *OrderID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := ParseOrderID(s)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// ParseOrderID parses a hex order ID.
func ParseOrderID(s string) (OrderID, error) {
	var id OrderID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, fmt.Errorf("invalid order ID %q", s)
	}
	copy(id[:], b)
	return id, nil
}

// Market identifies a base/quote pair by BIP-44 asset IDs.
type Market struct {
	Base  uint32 `json:"base"`
	Quote uint32 `json:"quote"`
}

// Name is the market name, e.g. "dcr_btc".
func (m Market) Name() (string, error) {
	b, q := dex.BipIDSymbol(m.Base), dex.BipIDSymbol(m.Quote)
	if b == "" || q == "" {
		return "", fmt.Errorf("unknown asset in market %d-%d", m.Base, m.Quote)
	}
	if m.Base == m.Quote {
		return "", errors.New("base and quote assets are the same")
	}
	return strings.ToLower(b + "_" + q), nil
}

// Topic is the mesh topic for the market's order book.
func (m Market) Topic() (string, error) {
	name, err := m.Name()
	if err != nil {
		return "", err
	}
	return OrderBookTopicPrefix + name, nil
}

// ParseMarket parses a market name like "dcr_btc".
func ParseMarket(name string) (Market, error) {
	parts := strings.Split(strings.ToLower(name), "_")
	if len(parts) != 2 {
		return Market{}, fmt.Errorf("invalid market %q", name)
	}
	b, okB := dex.BipSymbolID(parts[0])
	q, okQ := dex.BipSymbolID(parts[1])
	if !okB || !okQ {
		return Market{}, fmt.Errorf("unknown asset in market %q", name)
	}
	m := Market{Base: b, Quote: q}
	if _, err := m.Name(); err != nil {
		return Market{}, err
	}
	return m, nil
}

// MeshOrder is a signed standing limit order gossiped on a market topic.
// Qty and LotSize are in base asset atoms, Rate is in the conventional dcrdex
// message-rate encoding (quote atoms per 1e8 base atoms). Matches must be an
// integer number of lots. EncPubKey is the owner's static secp256k1 inbox
// encryption key (compressed), used by takers to encrypt match proposals.
type MeshOrder struct {
	Market
	Owner     string `json:"owner"` // libp2p peer ID of the mesh client
	Sell      bool   `json:"sell"`
	Qty       uint64 `json:"qty"`
	LotSize   uint64 `json:"lotSize"`
	Rate      uint64 `json:"rate"`
	Stamp     int64  `json:"stamp"`  // unix ms
	Expiry    int64  `json:"expiry"` // unix ms
	Nonce     uint64 `json:"nonce"`
	EncPubKey []byte `json:"encPubKey"`
	PubKey    []byte `json:"pubkey"` // libp2p marshaled public key
	Sig       []byte `json:"sig"`
}

func (o *MeshOrder) serialize() []byte {
	b := new(bytes.Buffer)
	b.WriteString(orderSigDomain)
	_ = binary.Write(b, binary.BigEndian, o.Base)
	_ = binary.Write(b, binary.BigEndian, o.Quote)
	if o.Sell {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	_ = binary.Write(b, binary.BigEndian, o.Qty)
	_ = binary.Write(b, binary.BigEndian, o.LotSize)
	_ = binary.Write(b, binary.BigEndian, o.Rate)
	_ = binary.Write(b, binary.BigEndian, o.Stamp)
	_ = binary.Write(b, binary.BigEndian, o.Expiry)
	_ = binary.Write(b, binary.BigEndian, o.Nonce)
	_ = binary.Write(b, binary.BigEndian, uint16(len(o.EncPubKey)))
	b.Write(o.EncPubKey)
	_ = binary.Write(b, binary.BigEndian, uint16(len(o.Owner)))
	b.WriteString(o.Owner)
	return b.Bytes()
}

// ID is the order ID.
func (o *MeshOrder) ID() OrderID {
	return blake256.Sum256(o.serialize())
}

// Sign sets the owner, public key and signature using priv.
func (o *MeshOrder) Sign(priv libp2pcrypto.PrivKey) error {
	pid, pubB, err := ownerOf(priv)
	if err != nil {
		return err
	}
	o.Owner, o.PubKey = pid.String(), pubB
	o.Sig, err = priv.Sign(o.serialize())
	return err
}

// Validate checks the fields, the signature, and that the order is live at
// time now.
func (o *MeshOrder) Validate(now time.Time) error {
	if _, err := o.Market.Name(); err != nil {
		return err
	}
	if o.Qty == 0 || o.Rate == 0 || o.LotSize == 0 {
		return errors.New("zero qty, lot size or rate")
	}
	if o.Qty%o.LotSize != 0 {
		return errors.New("qty is not a multiple of the lot size")
	}
	if o.LotSize < MinSwapValue || calc.BaseToQuote(o.Rate, o.LotSize) < MinSwapValue {
		return fmt.Errorf("lot is dust: base or quote value per lot is below %d atoms", MinSwapValue)
	}
	if len(o.EncPubKey) != 33 {
		return errors.New("bad encryption pubkey length")
	}
	nowMs := now.UnixMilli()
	if o.Stamp > nowMs+MaxClockSkew.Milliseconds() {
		return errors.New("order stamp is in the future")
	}
	if o.Expiry <= o.Stamp || o.Expiry-o.Stamp > MaxOrderLifetime.Milliseconds() {
		return errors.New("bad order lifetime")
	}
	if o.Expiry <= nowMs {
		return errors.New("order expired")
	}
	return verifySig(o.Owner, o.PubKey, o.serialize(), o.Sig)
}

// MeshCancel is a signed cancellation of a standing order.
type MeshCancel struct {
	OrderID OrderID `json:"orderID"`
	Owner   string  `json:"owner"`
	Stamp   int64   `json:"stamp"`
	PubKey  []byte  `json:"pubkey"`
	Sig     []byte  `json:"sig"`
}

func (c *MeshCancel) serialize() []byte {
	b := new(bytes.Buffer)
	b.WriteString(cancelSigDomain)
	b.Write(c.OrderID[:])
	_ = binary.Write(b, binary.BigEndian, c.Stamp)
	_ = binary.Write(b, binary.BigEndian, uint16(len(c.Owner)))
	b.WriteString(c.Owner)
	return b.Bytes()
}

// Sign sets the owner, public key and signature using priv.
func (c *MeshCancel) Sign(priv libp2pcrypto.PrivKey) error {
	pid, pubB, err := ownerOf(priv)
	if err != nil {
		return err
	}
	c.Owner, c.PubKey = pid.String(), pubB
	c.Sig, err = priv.Sign(c.serialize())
	return err
}

// Validate checks the cancel signature and stamp.
func (c *MeshCancel) Validate(now time.Time) error {
	if c.Stamp > now.Add(MaxClockSkew).UnixMilli() {
		return errors.New("cancel stamp is in the future")
	}
	return verifySig(c.Owner, c.PubKey, c.serialize(), c.Sig)
}

// MeshOrderUpdate is a signed update of an order's remaining quantity after
// (partial) fills. Remaining only ever decreases; zero removes the order.
type MeshOrderUpdate struct {
	OrderID   OrderID `json:"orderID"`
	Owner     string  `json:"owner"`
	Remaining uint64  `json:"remaining"`
	Stamp     int64   `json:"stamp"`
	PubKey    []byte  `json:"pubkey"`
	Sig       []byte  `json:"sig"`
}

func (u *MeshOrderUpdate) serialize() []byte {
	b := new(bytes.Buffer)
	b.WriteString(updateSigDomain)
	b.Write(u.OrderID[:])
	_ = binary.Write(b, binary.BigEndian, u.Remaining)
	_ = binary.Write(b, binary.BigEndian, u.Stamp)
	_ = binary.Write(b, binary.BigEndian, uint16(len(u.Owner)))
	b.WriteString(u.Owner)
	return b.Bytes()
}

// Sign sets the owner, public key and signature using priv.
func (u *MeshOrderUpdate) Sign(priv libp2pcrypto.PrivKey) error {
	pid, pubB, err := ownerOf(priv)
	if err != nil {
		return err
	}
	u.Owner, u.PubKey = pid.String(), pubB
	u.Sig, err = priv.Sign(u.serialize())
	return err
}

// Validate checks the update signature and stamp.
func (u *MeshOrderUpdate) Validate(now time.Time) error {
	if u.Stamp > now.Add(MaxClockSkew).UnixMilli() {
		return errors.New("update stamp is in the future")
	}
	return verifySig(u.Owner, u.PubKey, u.serialize(), u.Sig)
}

func ownerOf(priv libp2pcrypto.PrivKey) (peer.ID, []byte, error) {
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return "", nil, err
	}
	pubB, err := libp2pcrypto.MarshalPublicKey(priv.GetPublic())
	return pid, pubB, err
}

func verifySig(owner string, pubB, msg, sig []byte) error {
	pub, err := libp2pcrypto.UnmarshalPublicKey(pubB)
	if err != nil {
		return fmt.Errorf("bad public key: %w", err)
	}
	pid, err := peer.IDFromPublicKey(pub)
	if err != nil {
		return err
	}
	if pid.String() != owner {
		return errors.New("public key does not match owner")
	}
	ok, err := pub.Verify(msg, sig)
	if err != nil || !ok {
		return errors.New("bad signature")
	}
	return nil
}

// bookMsg is the wire envelope on an order book topic.
type bookMsg struct {
	Type   string           `json:"type"`
	Order  *MeshOrder       `json:"order,omitempty"`
	Cancel *MeshCancel      `json:"cancel,omitempty"`
	Update *MeshOrderUpdate `json:"update,omitempty"`
}

func encodeBookMsg(m *bookMsg) ([]byte, error) { return json.Marshal(m) }

func decodeBookMsg(b []byte) (*bookMsg, error) {
	m := new(bookMsg)
	if err := json.Unmarshal(b, m); err != nil {
		return nil, err
	}
	switch m.Type {
	case msgTypeOrder:
		if m.Order == nil {
			return nil, errors.New("order message without order")
		}
	case msgTypeCancel:
		if m.Cancel == nil {
			return nil, errors.New("cancel message without cancel")
		}
	case msgTypeUpdate:
		if m.Update == nil {
			return nil, errors.New("update message without update")
		}
	default:
		return nil, fmt.Errorf("unknown book message type %q", m.Type)
	}
	return m, nil
}

type tombstone struct {
	owner  string
	expiry int64 // unix ms
}

// BookOrder is an order on the book with its latest known remaining quantity.
type BookOrder struct {
	*MeshOrder
	Remaining uint64 `json:"remaining"`
}

type bookEntry struct {
	ord       *MeshOrder
	remaining uint64
	updStamp  int64
}

// Book is the set of live standing orders for one market as seen by this
// client. It is safe for concurrent use.
type Book struct {
	market Market
	mtx    sync.Mutex
	orders map[OrderID]*bookEntry
	// cancelled tracks cancelled order IDs so that stale re-broadcasts can't
	// resurrect them.
	cancelled map[OrderID]*tombstone
}

// NewBook creates an empty book for market.
func NewBook(m Market) *Book {
	return &Book{
		market:    m,
		orders:    make(map[OrderID]*bookEntry),
		cancelled: make(map[OrderID]*tombstone),
	}
}

// AddOrder validates and adds an order received from sender (the mesh peer
// that published it, or "" if local). It returns true if the order is new.
func (b *Book) AddOrder(o *MeshOrder, sender peer.ID, now time.Time) (bool, error) {
	if o.Market != b.market {
		return false, errors.New("order is for a different market")
	}
	if sender != "" && sender.String() != o.Owner {
		return false, fmt.Errorf("order owner %s was not the sender %s", o.Owner, sender)
	}
	if err := o.Validate(now); err != nil {
		return false, err
	}
	id := o.ID()
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if ts, found := b.cancelled[id]; found && ts.owner == o.Owner {
		return false, nil
	}
	if _, found := b.orders[id]; found {
		return false, nil
	}
	if sender != "" {
		n := 0
		for _, e := range b.orders {
			if e.ord.Owner == o.Owner && e.ord.Expiry > now.UnixMilli() {
				n++
			}
		}
		if n >= MaxOrdersPerOwner {
			return false, fmt.Errorf("owner %s already has %d live orders", o.Owner, n)
		}
	}
	b.orders[id] = &bookEntry{ord: o, remaining: o.Qty}
	return true, nil
}

// Cancel validates and applies a cancel received from sender. It returns
// true if a live order was removed.
func (b *Book) Cancel(c *MeshCancel, sender peer.ID, now time.Time) (bool, error) {
	if sender != "" && sender.String() != c.Owner {
		return false, fmt.Errorf("cancel owner %s was not the sender %s", c.Owner, sender)
	}
	if err := c.Validate(now); err != nil {
		return false, err
	}
	b.mtx.Lock()
	defer b.mtx.Unlock()
	expiry := now.Add(MaxOrderLifetime).UnixMilli()
	e, found := b.orders[c.OrderID]
	if found {
		if e.ord.Owner != c.Owner {
			return false, errors.New("cancel is not from the order owner")
		}
		expiry = e.ord.Expiry
		delete(b.orders, c.OrderID)
	} else if len(b.cancelled) >= maxTombstones {
		nowMs := now.UnixMilli()
		for id, ts := range b.cancelled {
			if ts.expiry <= nowMs {
				delete(b.cancelled, id)
			}
		}
		if len(b.cancelled) >= maxTombstones {
			return false, errors.New("too many cancels for unknown orders")
		}
	}
	b.cancelled[c.OrderID] = &tombstone{owner: c.Owner, expiry: expiry}
	return found, nil
}

// Update applies a remaining-quantity update from sender. It returns true if
// the book changed. A zero remaining removes the order.
func (b *Book) Update(u *MeshOrderUpdate, sender peer.ID, now time.Time) (bool, error) {
	if sender != "" && sender.String() != u.Owner {
		return false, fmt.Errorf("update owner %s was not the sender %s", u.Owner, sender)
	}
	if err := u.Validate(now); err != nil {
		return false, err
	}
	b.mtx.Lock()
	defer b.mtx.Unlock()
	e, found := b.orders[u.OrderID]
	if !found {
		return false, nil
	}
	if e.ord.Owner != u.Owner {
		return false, errors.New("update is not from the order owner")
	}
	if u.Stamp <= e.updStamp || u.Remaining >= e.remaining {
		return false, nil // stale or not a decrease
	}
	if u.Remaining == 0 {
		delete(b.orders, u.OrderID)
		b.cancelled[u.OrderID] = &tombstone{owner: u.Owner, expiry: e.ord.Expiry}
		return true, nil
	}
	e.remaining, e.updStamp = u.Remaining, u.Stamp
	return true, nil
}

// Order returns the order with the given ID, if live.
func (b *Book) Order(id OrderID, now time.Time) (*BookOrder, bool) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	e, found := b.orders[id]
	if !found || e.ord.Expiry <= now.UnixMilli() {
		return nil, false
	}
	return &BookOrder{MeshOrder: e.ord, Remaining: e.remaining}, true
}

// Prune removes expired orders and tombstones.
func (b *Book) Prune(now time.Time) {
	nowMs := now.UnixMilli()
	b.mtx.Lock()
	defer b.mtx.Unlock()
	for id, e := range b.orders {
		if e.ord.Expiry <= nowMs {
			delete(b.orders, id)
		}
	}
	for id, ts := range b.cancelled {
		if ts.expiry <= nowMs {
			delete(b.cancelled, id)
		}
	}
}

// Orders returns the live orders, sells first by ascending rate, then buys by
// descending rate (i.e. best price first on each side), then oldest first.
func (b *Book) Orders(now time.Time) []*BookOrder {
	nowMs := now.UnixMilli()
	b.mtx.Lock()
	ords := make([]*BookOrder, 0, len(b.orders))
	for _, e := range b.orders {
		if e.ord.Expiry > nowMs {
			ords = append(ords, &BookOrder{MeshOrder: e.ord, Remaining: e.remaining})
		}
	}
	b.mtx.Unlock()
	sortBookOrders(ords)
	return ords
}

// sortBookOrders sorts sells first by ascending rate, then buys by descending
// rate, then oldest first.
func sortBookOrders(ords []*BookOrder) {
	sort.Slice(ords, func(i, j int) bool {
		oi, oj := ords[i], ords[j]
		if oi.Sell != oj.Sell {
			return oi.Sell
		}
		if oi.Rate != oj.Rate {
			if oi.Sell {
				return oi.Rate < oj.Rate
			}
			return oi.Rate > oj.Rate
		}
		return oi.Stamp < oj.Stamp
	})
}
