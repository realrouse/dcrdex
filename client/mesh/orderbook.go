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

	orderSigDomain  = "tatanka-mesh-order-v1"
	cancelSigDomain = "tatanka-mesh-cancel-v1"

	msgTypeOrder  = "order"
	msgTypeCancel = "cancel"
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
// Qty is in base asset atoms, Rate is in the conventional dcrdex message-rate
// encoding (quote atoms per 1e8 base atoms).
type MeshOrder struct {
	Market
	Owner  string `json:"owner"` // libp2p peer ID of the mesh client
	Sell   bool   `json:"sell"`
	Qty    uint64 `json:"qty"`
	Rate   uint64 `json:"rate"`
	Stamp  int64  `json:"stamp"`  // unix ms
	Expiry int64  `json:"expiry"` // unix ms
	Nonce  uint64 `json:"nonce"`
	PubKey []byte `json:"pubkey"` // libp2p marshaled public key
	Sig    []byte `json:"sig"`
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
	_ = binary.Write(b, binary.BigEndian, o.Rate)
	_ = binary.Write(b, binary.BigEndian, o.Stamp)
	_ = binary.Write(b, binary.BigEndian, o.Expiry)
	_ = binary.Write(b, binary.BigEndian, o.Nonce)
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
	if o.Qty == 0 || o.Rate == 0 {
		return errors.New("zero qty or rate")
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
	Type   string      `json:"type"`
	Order  *MeshOrder  `json:"order,omitempty"`
	Cancel *MeshCancel `json:"cancel,omitempty"`
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
	default:
		return nil, fmt.Errorf("unknown book message type %q", m.Type)
	}
	return m, nil
}

type tombstone struct {
	owner  string
	expiry int64 // unix ms
}

// Book is the set of live standing orders for one market as seen by this
// client. It is safe for concurrent use.
type Book struct {
	market Market
	mtx    sync.Mutex
	orders map[OrderID]*MeshOrder
	// cancelled tracks cancelled order IDs so that stale re-broadcasts can't
	// resurrect them.
	cancelled map[OrderID]*tombstone
}

// NewBook creates an empty book for market.
func NewBook(m Market) *Book {
	return &Book{
		market:    m,
		orders:    make(map[OrderID]*MeshOrder),
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
	b.orders[id] = o
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
	o, found := b.orders[c.OrderID]
	if found {
		if o.Owner != c.Owner {
			return false, errors.New("cancel is not from the order owner")
		}
		expiry = o.Expiry
		delete(b.orders, c.OrderID)
	}
	b.cancelled[c.OrderID] = &tombstone{owner: c.Owner, expiry: expiry}
	return found, nil
}

// Prune removes expired orders and tombstones.
func (b *Book) Prune(now time.Time) {
	nowMs := now.UnixMilli()
	b.mtx.Lock()
	defer b.mtx.Unlock()
	for id, o := range b.orders {
		if o.Expiry <= nowMs {
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
// descending rate.
func (b *Book) Orders(now time.Time) []*MeshOrder {
	nowMs := now.UnixMilli()
	b.mtx.Lock()
	ords := make([]*MeshOrder, 0, len(b.orders))
	for _, o := range b.orders {
		if o.Expiry > nowMs {
			ords = append(ords, o)
		}
	}
	b.mtx.Unlock()
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
	return ords
}
