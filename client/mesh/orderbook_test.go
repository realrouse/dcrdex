// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	mc "github.com/bisoncraft/mesh/client"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

var tEncPub = func() []byte {
	k, _ := secp256k1.GeneratePrivateKey()
	return k.PubKey().SerializeCompressed()
}()

var (
	tLogger  = dex.StdOutLogger("TEST", dex.LevelInfo)
	dcrBTC   = Market{Base: 42, Quote: 0}
	tNow     = time.Unix(1_800_000_000, 0)
	tCtx     = context.Background()
	lifetime = time.Minute
)

func newPriv(t *testing.T) libp2pcrypto.PrivKey {
	t.Helper()
	priv, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func pid(t *testing.T, priv libp2pcrypto.PrivKey) peer.ID {
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func signedOrder(t *testing.T, priv libp2pcrypto.PrivKey, now time.Time) *MeshOrder {
	t.Helper()
	o := &MeshOrder{
		Market:    dcrBTC,
		Sell:      true,
		Qty:       1e8,
		LotSize:   1e8,
		Rate:      2e5,
		Stamp:     now.UnixMilli(),
		Expiry:    now.Add(lifetime).UnixMilli(),
		Nonce:     7,
		EncPubKey: tEncPub,
	}
	if err := o.Sign(priv); err != nil {
		t.Fatal(err)
	}
	return o
}

func signedCancel(t *testing.T, priv libp2pcrypto.PrivKey, id OrderID, now time.Time) *MeshCancel {
	t.Helper()
	c := &MeshCancel{OrderID: id, Stamp: now.UnixMilli()}
	if err := c.Sign(priv); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMarket(t *testing.T) {
	m, err := ParseMarket("dcr_btc")
	if err != nil {
		t.Fatal(err)
	}
	if m != dcrBTC {
		t.Fatalf("wrong market %+v", m)
	}
	topic, _ := m.Topic()
	if topic != "orderbook.dcr_btc" {
		t.Fatalf("wrong topic %s", topic)
	}
	for _, bad := range []string{"dcr", "dcr_dcr", "dcr_xyz", "a_b_c"} {
		if _, err := ParseMarket(bad); err == nil {
			t.Fatalf("no error for %q", bad)
		}
	}
}

func TestOrderSignValidate(t *testing.T) {
	priv := newPriv(t)
	o := signedOrder(t, priv, tNow)
	if err := o.Validate(tNow); err != nil {
		t.Fatalf("valid order failed: %v", err)
	}
	// JSON round trip keeps the ID and signature valid.
	b, _ := encodeBookMsg(&bookMsg{Type: msgTypeOrder, Order: o})
	msg, err := decodeBookMsg(b)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Order.ID() != o.ID() || msg.Order.Validate(tNow) != nil {
		t.Fatal("round trip changed order")
	}

	mutate := func(f func(o *MeshOrder)) *MeshOrder {
		c := *o
		f(&c)
		return &c
	}
	other := newPriv(t)
	otherPub, _ := libp2pcrypto.MarshalPublicKey(other.GetPublic())
	bad := map[string]*MeshOrder{
		"tampered qty":  mutate(func(o *MeshOrder) { o.Qty++ }),
		"tampered rate": mutate(func(o *MeshOrder) { o.Rate++ }),
		"wrong owner":   mutate(func(o *MeshOrder) { o.Owner = pid(t, other).String() }),
		"wrong pubkey":  mutate(func(o *MeshOrder) { o.PubKey = otherPub }),
		"zero qty":      mutate(func(o *MeshOrder) { o.Qty = 0 }),
		"partial lot":   mutate(func(o *MeshOrder) { o.Qty = o.LotSize * 3 / 2 }),
		"no enc key":    mutate(func(o *MeshOrder) { o.EncPubKey = nil }),
		"same assets":   mutate(func(o *MeshOrder) { o.Quote = o.Base }),
	}
	for name, o := range bad {
		if err := o.Validate(tNow); err == nil {
			t.Fatalf("%s: no error", name)
		}
	}
	if err := o.Validate(tNow.Add(lifetime)); err == nil {
		t.Fatal("expired order validated")
	}
	future := signedOrder(t, priv, tNow.Add(2*MaxClockSkew))
	if err := future.Validate(tNow); err == nil {
		t.Fatal("future order validated")
	}
	long := &MeshOrder{Market: dcrBTC, Qty: 1, LotSize: 1, Rate: 1, EncPubKey: tEncPub, Stamp: tNow.UnixMilli(),
		Expiry: tNow.Add(MaxOrderLifetime + time.Second).UnixMilli()}
	_ = long.Sign(priv)
	if err := long.Validate(tNow); err == nil {
		t.Fatal("over-long lifetime validated")
	}
}

func TestBook(t *testing.T) {
	alice, bob := newPriv(t), newPriv(t)
	aliceID, bobID := pid(t, alice), pid(t, bob)
	book := NewBook(dcrBTC)

	o := signedOrder(t, alice, tNow)
	if isNew, err := book.AddOrder(o, aliceID, tNow); err != nil || !isNew {
		t.Fatalf("add failed: %v %v", isNew, err)
	}
	if isNew, err := book.AddOrder(o, aliceID, tNow); err != nil || isNew {
		t.Fatalf("duplicate not ignored: %v %v", isNew, err)
	}
	// Relayed by someone other than the owner.
	o2 := signedOrder(t, alice, tNow.Add(time.Second))
	if _, err := book.AddOrder(o2, bobID, tNow); err == nil {
		t.Fatal("order from non-owner sender accepted")
	}
	// Wrong market.
	wrong := &MeshOrder{Market: Market{Base: 0, Quote: 42}, Qty: 1, LotSize: 1, Rate: 1, EncPubKey: tEncPub,
		Stamp: tNow.UnixMilli(), Expiry: tNow.Add(lifetime).UnixMilli()}
	_ = wrong.Sign(alice)
	if _, err := book.AddOrder(wrong, aliceID, tNow); err == nil {
		t.Fatal("wrong market accepted")
	}
	// Bob can't cancel Alice's order.
	if _, err := book.Cancel(signedCancel(t, bob, o.ID(), tNow), bobID, tNow); err == nil {
		t.Fatal("non-owner cancel accepted")
	}
	// Alice cancels, and a stale re-broadcast doesn't resurrect it.
	if removed, err := book.Cancel(signedCancel(t, alice, o.ID(), tNow), aliceID, tNow); err != nil || !removed {
		t.Fatalf("cancel failed: %v %v", removed, err)
	}
	if isNew, _ := book.AddOrder(o, aliceID, tNow); isNew {
		t.Fatal("cancelled order resurrected")
	}
	// Cancel arriving before the order.
	o3 := signedOrder(t, bob, tNow)
	if removed, err := book.Cancel(signedCancel(t, bob, o3.ID(), tNow), bobID, tNow); err != nil || removed {
		t.Fatalf("early cancel: %v %v", removed, err)
	}
	if isNew, _ := book.AddOrder(o3, bobID, tNow); isNew {
		t.Fatal("order added after its cancel")
	}
	if n := len(book.Orders(tNow)); n != 0 {
		t.Fatalf("expected empty book, got %d", n)
	}

	// Sorting and expiry.
	mk := func(sell bool, rate uint64, life time.Duration) *MeshOrder {
		o := &MeshOrder{Market: dcrBTC, Sell: sell, Qty: 1e8, LotSize: 1e8, EncPubKey: tEncPub, Rate: rate * 1e5, Stamp: tNow.UnixMilli(),
			Expiry: tNow.Add(life).UnixMilli(), Nonce: rate}
		_ = o.Sign(bob)
		if _, err := book.AddOrder(o, "", tNow); err != nil {
			t.Fatal(err)
		}
		return o
	}
	mk(false, 5, lifetime)
	mk(true, 9, lifetime)
	mk(false, 7, lifetime)
	mk(true, 8, 10*time.Second)
	ords := book.Orders(tNow)
	got := []uint64{}
	for _, o := range ords {
		got = append(got, o.Rate/1e5)
	}
	if want := []uint64{8, 9, 7, 5}; !equalU64(got, want) {
		t.Fatalf("wrong order %v, want %v", got, want)
	}
	later := tNow.Add(11 * time.Second)
	if n := len(book.Orders(later)); n != 3 {
		t.Fatalf("expired order still listed: %d", n)
	}
	book.Prune(later)
	book.mtx.Lock()
	n := len(book.orders)
	book.mtx.Unlock()
	if n != 3 {
		t.Fatalf("prune left %d orders", n)
	}
	book.Prune(tNow.Add(MaxOrderLifetime + time.Second))
	book.mtx.Lock()
	n, nt := len(book.orders), len(book.cancelled)
	book.mtx.Unlock()
	if n != 0 || nt != 0 {
		t.Fatalf("prune left %d orders, %d tombstones", n, nt)
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeMesh is an in-memory pub/sub that mimics the mesh: broadcasts reach all
// subscribers with the sender's peer ID, and new subscriptions notify existing
// subscribers.
type fakeMesh struct {
	mtx  sync.Mutex
	subs map[string]map[peer.ID]mc.TopicHandler
	drop bool
	// dropFn, if set, drops matching broadcasts.
	dropFn func(topic string, from peer.ID) bool
}

func newFakeMesh() *fakeMesh {
	return &fakeMesh{subs: make(map[string]map[peer.ID]mc.TopicHandler)}
}

func (f *fakeMesh) setDrop(fn func(topic string, from peer.ID) bool) {
	f.mtx.Lock()
	f.dropFn = fn
	f.mtx.Unlock()
}

type fakeClient struct {
	m  *fakeMesh
	id peer.ID
}

func (f *fakeMesh) client(id peer.ID) *fakeClient { return &fakeClient{m: f, id: id} }

func (c *fakeClient) Broadcast(_ context.Context, topic string, data []byte) error {
	c.m.mtx.Lock()
	if c.m.drop || (c.m.dropFn != nil && c.m.dropFn(topic, c.id)) {
		c.m.mtx.Unlock()
		return nil
	}
	hs := make([]mc.TopicHandler, 0)
	for _, h := range c.m.subs[topic] {
		hs = append(hs, h)
	}
	c.m.mtx.Unlock()
	for _, h := range hs {
		h(mc.TopicEvent{Type: mc.TopicEventData, Data: data, Peer: c.id})
	}
	return nil
}

func (c *fakeClient) Subscribe(_ context.Context, topic string, h mc.TopicHandler) error {
	c.m.mtx.Lock()
	if c.m.subs[topic] == nil {
		c.m.subs[topic] = make(map[peer.ID]mc.TopicHandler)
	}
	others := make([]mc.TopicHandler, 0)
	for id, oh := range c.m.subs[topic] {
		if id != c.id {
			others = append(others, oh)
		}
	}
	c.m.subs[topic][c.id] = h
	c.m.mtx.Unlock()
	for _, oh := range others {
		oh(mc.TopicEvent{Type: mc.TopicEventPeerSubscribed, Peer: c.id})
	}
	return nil
}

func newTestBooks(t *testing.T, m *fakeMesh, clock *time.Time) *OrderBooks {
	priv := newPriv(t)
	encPriv, _ := secp256k1.GeneratePrivateKey()
	ob := NewOrderBooks(m.client(pid(t, priv)), priv, encPriv, tLogger)
	if clock != nil {
		ob.now = func() time.Time { return *clock }
	}
	return ob
}

func bookLen(t *testing.T, ob *OrderBooks) int {
	t.Helper()
	ords, err := ob.Book(dcrBTC)
	if err != nil {
		t.Fatal(err)
	}
	return len(ords)
}

func TestOrderBooksGossip(t *testing.T) {
	clock := time.Now()
	m := &fakeMesh{subs: make(map[string]map[peer.ID]mc.TopicHandler)}
	a, b := newTestBooks(t, m, &clock), newTestBooks(t, m, &clock)

	if err := b.SubscribeMarket(tCtx, dcrBTC); err != nil {
		t.Fatal(err)
	}
	ord, err := a.PostOrder(tCtx, dcrBTC, true, 1e8, 0, 2e5, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if bookLen(t, a) != 1 || bookLen(t, b) != 1 {
		t.Fatalf("order not on both books: a=%d b=%d", bookLen(t, a), bookLen(t, b))
	}

	// Late subscriber gets the order via the subscription notification.
	c := newTestBooks(t, m, &clock)
	if err := c.SubscribeMarket(tCtx, dcrBTC); err != nil {
		t.Fatal(err)
	}
	if bookLen(t, c) != 1 {
		t.Fatal("late subscriber didn't get the order")
	}

	// Cancel propagates.
	if err := a.CancelOrder(tCtx, ord.ID()); err != nil {
		t.Fatal(err)
	}
	if bookLen(t, a)+bookLen(t, b)+bookLen(t, c) != 0 {
		t.Fatal("cancel didn't propagate")
	}
	if err := a.CancelOrder(tCtx, ord.ID()); err == nil {
		t.Fatal("double cancel didn't error")
	}

	// A lost broadcast is recovered by the periodic re-broadcast.
	m.drop = true
	ord2, err := a.PostOrder(tCtx, dcrBTC, false, 2e8, 1e8, 1e5, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m.drop = false
	if bookLen(t, b) != 0 {
		t.Fatal("dropped order arrived")
	}
	a.maintain(tCtx)
	if bookLen(t, b) != 1 || bookLen(t, c) != 1 {
		t.Fatal("re-broadcast didn't deliver")
	}

	// Expiry removes it everywhere and stops re-broadcasts.
	clock = clock.Add(31 * time.Second)
	if bookLen(t, a)+bookLen(t, b)+bookLen(t, c) != 0 {
		t.Fatal("expired order still listed")
	}
	a.maintain(tCtx)
	if n := len(a.OwnOrders()); n != 0 {
		t.Fatalf("expired own order kept: %d", n)
	}
	_ = ord2
}

func TestDebugServer(t *testing.T) {
	clock := time.Now()
	m := &fakeMesh{subs: make(map[string]map[peer.ID]mc.TopicHandler)}
	a, b := newTestBooks(t, m, &clock), newTestBooks(t, m, &clock)
	sa := httptest.NewServer(NewDebugServer(a, func() string { return "a" }).Handler(tCtx))
	defer sa.Close()
	sb := httptest.NewServer(NewDebugServer(b, func() string { return "b" }).Handler(tCtx))
	defer sb.Close()

	post := func(url, body string, out any) int {
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	book := func(url string) []map[string]any {
		resp, err := http.Get(url + "/mesh/book?market=dcr_btc")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res struct {
			Orders []map[string]any `json:"orders"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return res.Orders
	}

	if code := post(sb.URL+"/mesh/subscribe", `{"market":"dcr_btc"}`, nil); code != 200 {
		t.Fatalf("subscribe code %d", code)
	}
	var ord struct {
		ID string `json:"id"`
	}
	if code := post(sa.URL+"/mesh/order", `{"market":"dcr_btc","sell":true,"qty":100000000,"rate":200000,"lifetimeSec":60}`, &ord); code != 200 {
		t.Fatalf("order code %d", code)
	}
	ords := book(sb.URL)
	if len(ords) != 1 || ords[0]["id"] != ord.ID {
		t.Fatalf("order not on b's book: %v", ords)
	}
	if code := post(sa.URL+"/mesh/order", `{"market":"dcr_btc","qty":1,"rate":1,"lifetimeSec":999999}`, nil); code != 400 {
		t.Fatalf("bad lifetime accepted: %d", code)
	}
	if code := post(sa.URL+"/mesh/cancel", `{"id":"`+ord.ID+`"}`, nil); code != 200 {
		t.Fatalf("cancel code %d", code)
	}
	if n := len(book(sb.URL)); n != 0 {
		t.Fatalf("cancel didn't reach b: %d", n)
	}
	if err := NewDebugServer(a, nil).ListenAndServe(tCtx, "0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback debug address accepted")
	}
}
