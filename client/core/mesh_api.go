// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"errors"
	"fmt"
	"os"
	"time"

	"decred.org/dcrdex/client/db"
	meshpkg "decred.org/dcrdex/client/mesh"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/msgjson"
)

// NoteTypeMesh is the notification type for mesh activity (orders, matches,
// swaps, peers and connection changes). The payload is a *mesh.Event.
const NoteTypeMesh = "mesh"

// TopicMeshEvent is the topic of mesh activity notes.
const TopicMeshEvent Topic = "MeshEvent"

// MeshNote is a data notification carrying a mesh event.
type MeshNote struct {
	db.Notification
	Event *meshpkg.Event `json:"event"`
}

func newMeshNote(e *meshpkg.Event) *MeshNote {
	return &MeshNote{
		Notification: db.NewNotification(NoteTypeMesh, TopicMeshEvent, "", "", db.Data),
		Event:        e,
	}
}

// MeshMarketInfo describes a mesh market for the UI.
type MeshMarketInfo struct {
	Name         string `json:"name"`
	BaseID       uint32 `json:"baseID"`
	QuoteID      uint32 `json:"quoteID"`
	BaseSymbol   string `json:"baseSymbol"`
	QuoteSymbol  string `json:"quoteSymbol"`
	LotSize      uint64 `json:"lotSize"`
	TestRate     uint64 `json:"testRate"`
	RateStep     uint64 `json:"rateStep"`
	MakerLockSec int64  `json:"makerLockSec"`
	TakerLockSec int64  `json:"takerLockSec"`
	Subscribed   bool   `json:"subscribed"`
}

// MeshStatus is the mesh connectivity state.
type MeshStatus struct {
	Enabled            bool                `json:"enabled"`
	Connected          bool                `json:"connected"`
	PeerID             string              `json:"peerID"`
	Name               string              `json:"name"`
	Node               string              `json:"node"`
	Nodes              []*meshpkg.NodeInfo `json:"nodes"`
	Bootstrap          []string            `json:"bootstrap"`
	Peers              []*meshpkg.PeerInfo `json:"peers"`
	ProtocolVersion    uint32              `json:"protocolVersion"`
	MinProtocolVersion uint32              `json:"minProtocolVersion"`
	AppVersion         string              `json:"appVersion"`
	Markets            []*MeshMarketInfo   `json:"markets"`
	// Epochs is the epoch consensus status (bisonary epoch mode), or nil.
	Epochs *meshpkg.EpochStatus `json:"epochs,omitempty"`
}

// MeshOrderForm is a standing order posted from the UI. Rate is in the
// message-rate encoding (quote atoms per 1e8 base atoms).
type MeshOrderForm struct {
	Market      string `json:"market"`
	Sell        bool   `json:"sell"`
	Lots        uint64 `json:"lots"`
	Rate        uint64 `json:"rate"`
	LifetimeSec int64  `json:"lifetimeSec"`
}

// MeshSwapView is a swap record for display (no secrets), with refund info.
type MeshSwapView struct {
	*meshpkg.SwapRecord
	// OwnLockTime is when our own contract becomes refundable (unix s).
	OwnLockTime int64 `json:"ownLockTime"`
	// PeerName is the counterparty's display name.
	PeerName string `json:"peerName,omitempty"`
	Peer     string `json:"peer"`
}

// MeshActivity is our own orders, matches and swaps.
type MeshActivity struct {
	Orders  []*meshpkg.ViewOrder   `json:"orders"`
	Matches []*meshpkg.MatchRecord `json:"matches"`
	Swaps   []*MeshSwapView        `json:"swaps"`
}

// ErrMeshDisabled is returned by mesh methods when the mesh is not running.
var ErrMeshDisabled = errors.New("the mesh is not enabled or not connected (simnet with --mesh only)")

// meshBootstrap returns the mesh bootstrap addresses: the configured list,
// else TATANKA_SIMNET_ADDR (comma-separated).
func (c *Core) meshBootstrap() ([]string, error) {
	if len(c.cfg.MeshBootstrap) > 0 {
		var all []string
		for _, a := range c.cfg.MeshBootstrap {
			l, err := meshpkg.ParseBootstrapList(a)
			if err != nil {
				return nil, err
			}
			all = append(all, l...)
		}
		return all, nil
	}
	env := os.Getenv("TATANKA_SIMNET_ADDR")
	if env == "" {
		return nil, errors.New("no mesh bootstrap address: set --meshbootstrap or TATANKA_SIMNET_ADDR")
	}
	return meshpkg.ParseBootstrapList(env)
}

// meshClient returns the running mesh client or ErrMeshDisabled.
func (c *Core) meshClient() (*meshpkg.Client, error) {
	c.meshMtx.RLock()
	m, cm := c.mesh, c.meshCM
	c.meshMtx.RUnlock()
	if m == nil || cm == nil || !cm.On() {
		return nil, ErrMeshDisabled
	}
	return m, nil
}

func meshMarketInfo(m *meshpkg.Client, name string) (*MeshMarketInfo, meshpkg.Market, error) {
	mkt, err := meshpkg.ParseMarket(name)
	if err != nil {
		return nil, mkt, err
	}
	pol := m.LockPolicy(mkt)
	info := &MeshMarketInfo{
		Name: name, BaseID: mkt.Base, QuoteID: mkt.Quote,
		BaseSymbol: dex.BipIDSymbol(mkt.Base), QuoteSymbol: dex.BipIDSymbol(mkt.Quote),
		LotSize:      1e8,
		RateStep:     meshpkg.MarketRateStep(name),
		MakerLockSec: int64(pol.Maker / time.Second), TakerLockSec: int64(pol.Taker / time.Second),
	}
	if d, ok := meshpkg.MarketDefault(name); ok {
		info.LotSize, info.TestRate = d.LotSize, d.TestRate
	}
	for _, sm := range m.Markets() {
		if sm == mkt {
			info.Subscribed = true
		}
	}
	return info, mkt, nil
}

// MeshStatus returns the mesh connectivity state. If the mesh is not
// running, Enabled is false.
func (c *Core) MeshStatus() *MeshStatus {
	m, err := c.meshClient()
	if err != nil {
		return &MeshStatus{Enabled: false, ProtocolVersion: meshpkg.ProtocolVersion,
			MinProtocolVersion: meshpkg.MinProtocolVersion, AppVersion: c.cfg.AppVersion}
	}
	st := &MeshStatus{
		Enabled:            true,
		PeerID:             m.Self().String(),
		Name:               m.Name(),
		Node:               m.ConnectedNode(),
		Nodes:              m.KnownNodes(),
		Bootstrap:          m.Bootstrap(),
		Peers:              m.Peers(),
		ProtocolVersion:    meshpkg.ProtocolVersion,
		MinProtocolVersion: meshpkg.MinProtocolVersion,
		AppVersion:         c.cfg.AppVersion,
	}
	st.Connected = st.Node != ""
	if e := m.OrderBooks.Epochs(); e != nil {
		st.Epochs = e.Status()
	}
	seen := make(map[string]bool)
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if info, _, err := meshMarketInfo(m, name); err == nil {
			st.Markets = append(st.Markets, info)
		}
	}
	for _, d := range meshpkg.DefaultMarkets {
		add(d.Name)
	}
	for _, mkt := range m.Markets() {
		if name, err := mkt.Name(); err == nil {
			add(name)
		}
	}
	return st
}

// MeshBook returns a mesh market's order book, subscribing if needed.
func (c *Core) MeshBook(market string) (*meshpkg.BookView, error) {
	m, err := c.meshClient()
	if err != nil {
		return nil, err
	}
	mkt, err := meshpkg.ParseMarket(market)
	if err != nil {
		return nil, err
	}
	if err := m.SubscribeMarket(c.ctx, mkt); err != nil {
		return nil, err
	}
	return m.BookView(mkt), nil
}

// meshCheckFunds checks that the wallet for the asset we'd send has at least
// amt available, and the wallet for the asset we'd receive exists.
func (c *Core) meshCheckFunds(sendAsset, recvAsset uint32, amt uint64) error {
	for _, id := range []uint32{sendAsset, recvAsset} {
		if _, found := c.wallet(id); !found {
			return fmt.Errorf("no %s wallet", dex.BipIDSymbol(id))
		}
	}
	bal, err := c.AssetBalance(sendAsset)
	if err != nil {
		return fmt.Errorf("%s balance: %w", dex.BipIDSymbol(sendAsset), err)
	}
	if bal.Balance != nil && bal.Available < amt {
		return fmt.Errorf("not enough %s: need %d atoms plus fees, have %d available", dex.BipIDSymbol(sendAsset), amt, bal.Available)
	}
	return nil
}

// MeshPostOrder posts a standing order on a mesh market.
func (c *Core) MeshPostOrder(form *MeshOrderForm) (*meshpkg.ViewOrder, error) {
	m, err := c.meshClient()
	if err != nil {
		return nil, err
	}
	info, mkt, err := meshMarketInfo(m, form.Market)
	if err != nil {
		return nil, err
	}
	if form.Lots == 0 || form.Rate == 0 {
		return nil, errors.New("lots and rate must be positive")
	}
	if form.Lots > 1000 {
		return nil, errors.New("at most 1000 lots per order")
	}
	qty := form.Lots * info.LotSize
	quoteQty := calc.BaseToQuote(form.Rate, info.LotSize)
	if info.LotSize < meshpkg.MinSwapValue || quoteQty < meshpkg.MinSwapValue {
		return nil, fmt.Errorf("one lot must move at least %d atoms of each asset (rate too low?)", meshpkg.MinSwapValue)
	}
	if form.Sell {
		err = c.meshCheckFunds(mkt.Base, mkt.Quote, qty)
	} else {
		err = c.meshCheckFunds(mkt.Quote, mkt.Base, calc.BaseToQuote(form.Rate, qty))
	}
	if err != nil {
		return nil, err
	}
	life := time.Duration(form.LifetimeSec) * time.Second
	if life <= 0 || life > meshpkg.MaxOrderLifetime {
		life = meshpkg.MaxOrderLifetime
	}
	ord, err := m.PostOrder(c.ctx, mkt, form.Sell, qty, info.LotSize, form.Rate, life)
	if err != nil {
		return nil, err
	}
	return &meshpkg.ViewOrder{BookOrder: &meshpkg.BookOrder{MeshOrder: ord, Remaining: ord.Qty},
		ID: ord.ID().String(), Own: true, OwnerName: m.Name()}, nil
}

// MeshTakeOrder proposes a match for lots of one book order. It returns
// the match ID.
func (c *Core) MeshTakeOrder(market, orderID string, lots uint64) (string, error) {
	m, err := c.meshClient()
	if err != nil {
		return "", err
	}
	mkt, err := meshpkg.ParseMarket(market)
	if err != nil {
		return "", err
	}
	id, err := meshpkg.ParseOrderID(orderID)
	if err != nil {
		return "", err
	}
	var bo *meshpkg.ViewOrder
	for _, o := range m.BookView(mkt).Orders {
		if o.ID == orderID {
			bo = o
		}
	}
	if bo == nil {
		return "", errors.New("order is no longer on the book")
	}
	if lots == 0 {
		lots = 1
	}
	qty := lots * bo.LotSize
	// We take the other side of the maker's order.
	if bo.Sell {
		err = c.meshCheckFunds(mkt.Quote, mkt.Base, calc.BaseToQuote(bo.Rate, qty))
	} else {
		err = c.meshCheckFunds(mkt.Base, mkt.Quote, qty)
	}
	if err != nil {
		return "", err
	}
	mid, err := m.TakeOrder(c.ctx, mkt, id, qty)
	if err != nil {
		return "", err
	}
	return mid.String(), nil
}

// MeshCancelOrder cancels one of our standing mesh orders.
func (c *Core) MeshCancelOrder(orderID string) error {
	m, err := c.meshClient()
	if err != nil {
		return err
	}
	id, err := meshpkg.ParseOrderID(orderID)
	if err != nil {
		return err
	}
	return m.CancelOrder(c.ctx, id)
}

// MeshActivity returns our own standing orders, matches and swaps.
func (c *Core) MeshActivity() (*MeshActivity, error) {
	m, err := c.meshClient()
	if err != nil {
		return nil, err
	}
	act := &MeshActivity{Orders: []*meshpkg.ViewOrder{}, Matches: m.Matches(), Swaps: []*MeshSwapView{}}
	name := m.Name()
	for _, o := range m.OwnOrders() {
		act.Orders = append(act.Orders, &meshpkg.ViewOrder{BookOrder: o, ID: o.ID().String(), Own: true, OwnerName: name})
	}
	self := m.Self().String()
	for _, r := range m.Swaps() {
		r.EncSecret = nil
		v := &MeshSwapView{SwapRecord: r}
		if r.Match.Maker == self {
			v.OwnLockTime, v.Peer = r.Match.MakerLockTime, r.Match.Taker
		} else {
			v.OwnLockTime, v.Peer = r.Match.TakerLockTime, r.Match.Maker
		}
		if pi, ok := m.Peer(v.Peer); ok {
			v.PeerName = pi.Name
		}
		act.Swaps = append(act.Swaps, v)
	}
	return act, nil
}

// MeshCandles are a mesh market's candles of one bin size, built from the
// trade history the mesh peers publish.
type MeshCandles struct {
	Dur     string           `json:"dur"`
	DurMs   uint64           `json:"ms"`
	Candles []msgjson.Candle `json:"candles"`
}

// MeshCandles returns a mesh market's candles for a bin size (5m, 1h, 24h).
func (c *Core) MeshCandles(market, dur string) (*MeshCandles, error) {
	m, err := c.meshClient()
	if err != nil {
		return nil, err
	}
	mkt, err := meshpkg.ParseMarket(market)
	if err != nil {
		return nil, err
	}
	var ok bool
	for _, d := range meshpkg.CandleBinSizes {
		ok = ok || d == dur
	}
	binSize, err := time.ParseDuration(dur)
	if !ok || err != nil {
		return nil, fmt.Errorf("unsupported candle duration %q", dur)
	}
	cs := meshpkg.BuildCandles(meshTrades(m.OrderBooks, mkt, 0), binSize).CandlesCopy()
	if cs == nil {
		cs = []msgjson.Candle{}
	}
	return &MeshCandles{Dur: dur, DurMs: uint64(binSize.Milliseconds()), Candles: cs}, nil
}

// MeshSpots returns the 24 hour price summary of each mesh market with trades.
func (c *Core) MeshSpots() (map[string]*msgjson.Spot, error) {
	m, err := c.meshClient()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	spots := make(map[string]*msgjson.Spot)
	for _, d := range meshpkg.DefaultMarkets {
		mkt, err := meshpkg.ParseMarket(d.Name)
		if err != nil {
			continue
		}
		if s := meshpkg.MarketSpot(mkt, meshTrades(m.OrderBooks, mkt, now.Add(-meshpkg.TradeRetention).UnixMilli()), now); s != nil {
			spots[d.Name] = s
		}
	}
	return spots, nil
}

// meshTrades is a market's price history: with S7 rules in the release only
// trades verified on chain and admitted in finalized epochs, otherwise the
// signed trade summaries peers publish.
func meshTrades(ob *meshpkg.OrderBooks, mkt meshpkg.Market, sinceMs int64) []*meshpkg.TradeSummary {
	if e := ob.Epochs(); e != nil && e.PriceHistoryOn() {
		var out []*meshpkg.TradeSummary
		for _, t := range meshpkg.TradeSummaries(e.VerifiedTrades(mkt)) {
			if t.Stamp >= sinceMs {
				out = append(out, t)
			}
		}
		return out
	}
	return ob.Trades(mkt, sinceMs)
}
