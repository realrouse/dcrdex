// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	mc "github.com/bisoncraft/mesh/client"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Mesh trading protocol versioning. ProtocolVersion is the version of the
// order book, match negotiation and swap messages this client speaks.
// MinProtocolVersion is the oldest version it can still trade with. Two
// peers are compatible when each one's version is at least the other's
// minimum. Bump ProtocolVersion (and MinProtocolVersion for breaking
// changes) whenever a wire message or a swap rule changes.
//
// Version 2 (bisonary S0-S2): matches only come from finalized validator
// epochs, so a version 1 maker would accept proposals a version 2 network
// forbids. The two don't trade.
const (
	ProtocolVersion    uint32 = 2
	MinProtocolVersion uint32 = 2
)

const (
	// PresenceTopic is the mesh topic on which clients announce themselves.
	PresenceTopic = "presence.tatanka-dex"
	// DefaultPresenceInterval is how often presence is re-announced.
	DefaultPresenceInterval = 15 * time.Second
	// PresenceTimeout is how long a peer is considered online after its
	// last presence message or order.
	PresenceTimeout = 50 * time.Second
	// peerForget is how long an offline peer is kept in the registry.
	peerForget = 15 * time.Minute
	// MaxPeerNameLen caps a peer's display name (bytes).
	MaxPeerNameLen = 32
	// maxPresenceApp caps the app version string.
	maxPresenceApp = 40
	// maxPresencePolicies caps the number of market policies announced.
	maxPresencePolicies = 12
	// minPresenceResend is the minimum time between presence broadcasts
	// triggered by peers joining.
	minPresenceResend = 2 * time.Second
	// maxPeers caps the peer registry.
	maxPeers = 2000

	presenceSigDomain = "tatanka-mesh-presence-v1"
)

// Presence is a signed announcement of a mesh trading client: its display
// name, protocol and app versions, the mesh node it is connected to, and
// the swap lock policy (maker, taker seconds) it uses per market.
type Presence struct {
	Owner    string              `json:"owner"`
	Proto    uint32              `json:"proto"`
	MinProto uint32              `json:"minProto"`
	App      string              `json:"app,omitempty"`
	Name     string              `json:"name,omitempty"`
	Node     string              `json:"node,omitempty"`
	Policies map[string][2]int64 `json:"pol,omitempty"`
	Stamp    int64               `json:"stamp"` // unix ms
	PubKey   []byte              `json:"pubkey"`
	Sig      []byte              `json:"sig"`
}

func (p *Presence) serialize() []byte {
	var b bytes.Buffer
	b.WriteString(presenceSigDomain)
	writeStr(&b, p.Owner)
	var u [8]byte
	binary.BigEndian.PutUint32(u[:4], p.Proto)
	b.Write(u[:4])
	binary.BigEndian.PutUint32(u[:4], p.MinProto)
	b.Write(u[:4])
	writeStr(&b, p.App)
	writeStr(&b, p.Name)
	writeStr(&b, p.Node)
	mkts := make([]string, 0, len(p.Policies))
	for m := range p.Policies {
		mkts = append(mkts, m)
	}
	sort.Strings(mkts)
	binary.BigEndian.PutUint32(u[:4], uint32(len(mkts)))
	b.Write(u[:4])
	for _, m := range mkts {
		writeStr(&b, m)
		pol := p.Policies[m]
		binary.BigEndian.PutUint64(u[:], uint64(pol[0]))
		b.Write(u[:])
		binary.BigEndian.PutUint64(u[:], uint64(pol[1]))
		b.Write(u[:])
	}
	binary.BigEndian.PutUint64(u[:], uint64(p.Stamp))
	b.Write(u[:])
	return b.Bytes()
}

// Sign sets the owner, public key and signature.
func (p *Presence) Sign(priv libp2pcrypto.PrivKey) error {
	pid, pubB, err := ownerOf(priv)
	if err != nil {
		return err
	}
	p.Owner, p.PubKey = pid.String(), pubB
	p.Sig, err = priv.Sign(p.serialize())
	return err
}

// Validate checks the fields and signature.
func (p *Presence) Validate(now time.Time) error {
	if len(p.Name) > MaxPeerNameLen || !utf8.ValidString(p.Name) {
		return errors.New("bad name")
	}
	if len(p.App) > maxPresenceApp || !utf8.ValidString(p.App) {
		return errors.New("bad app version")
	}
	if len(p.Policies) > maxPresencePolicies {
		return errors.New("too many policies")
	}
	if p.Node != "" {
		if _, err := peer.Decode(p.Node); err != nil {
			return fmt.Errorf("bad node ID: %w", err)
		}
	}
	if p.Stamp > now.Add(MaxClockSkew).UnixMilli() {
		return errors.New("presence stamp in the future")
	}
	if p.Stamp < now.Add(-PresenceTimeout).UnixMilli() {
		return errors.New("stale presence")
	}
	return verifySig(p.Owner, p.PubKey, p.serialize(), p.Sig)
}

// SanitizeName trims a display name to printable characters and at most
// MaxPeerNameLen bytes.
func SanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '<' || r == '>' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	for len(s) > MaxPeerNameLen {
		_, sz := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-sz]
	}
	return s
}

// ProtocolCompatible reports whether a peer speaking proto, which needs at
// least minProto, can trade with this client.
func ProtocolCompatible(proto, minProto uint32) (bool, string) {
	if proto < MinProtocolVersion {
		return false, fmt.Sprintf("peer speaks mesh protocol v%d, we need at least v%d", proto, MinProtocolVersion)
	}
	if minProto > ProtocolVersion {
		return false, fmt.Sprintf("peer needs mesh protocol v%d or newer, we speak v%d", minProto, ProtocolVersion)
	}
	return true, ""
}

// PeerInfo is what this client knows about another mesh trading client.
type PeerInfo struct {
	ID        string              `json:"id"`
	Name      string              `json:"name,omitempty"`
	Proto     uint32              `json:"proto"` // 0: no presence seen yet
	MinProto  uint32              `json:"minProto"`
	App       string              `json:"app,omitempty"`
	Node      string              `json:"node,omitempty"`
	Policies  map[string][2]int64 `json:"policies,omitempty"`
	FirstSeen int64               `json:"firstSeen"`
	LastSeen  int64               `json:"lastSeen"`
	Online    bool                `json:"online"`
	// Seen lists where the peer was seen: presence, book, inbox, topic.
	Seen       []string `json:"seen"`
	Compatible bool     `json:"compatible"`
	Reason     string   `json:"reason,omitempty"`
}

func (pi *PeerInfo) addSeen(src string) {
	for _, s := range pi.Seen {
		if s == src {
			return
		}
	}
	pi.Seen = append(pi.Seen, src)
	sort.Strings(pi.Seen)
}

func (pi *PeerInfo) copy() *PeerInfo {
	c := *pi
	c.Seen = append([]string(nil), pi.Seen...)
	if pi.Policies != nil {
		c.Policies = make(map[string][2]int64, len(pi.Policies))
		for k, v := range pi.Policies {
			c.Policies[k] = v
		}
	}
	return &c
}

// Event kinds delivered to the event sink.
const (
	EventOrder    = "order"     // a new order on a book (own or a peer's)
	EventCancel   = "cancel"    // an order was cancelled
	EventUpdate   = "update"    // an order's remaining quantity changed
	EventMatch    = "match"     // a match changed status
	EventSwap     = "swap"      // a swap changed status
	EventTrade    = "trade"     // a completed trade was added to the price history
	EventPeer     = "peer"      // a peer joined or updated its presence
	EventPeerLeft = "peer_left" // a peer went offline
	EventConn     = "conn"      // our mesh connection changed
)

// Event is a mesh activity notification for user interfaces.
type Event struct {
	Kind    string    `json:"kind"`
	Market  string    `json:"market,omitempty"`
	Peer    string    `json:"peer,omitempty"`
	OrderID string    `json:"orderID,omitempty"`
	MatchID string    `json:"matchID,omitempty"`
	Status  string    `json:"status,omitempty"`
	Side    string    `json:"side,omitempty"`
	Sell    bool      `json:"sell,omitempty"`
	Qty     uint64    `json:"qty,omitempty"`
	Rate    uint64    `json:"rate,omitempty"`
	Own     bool      `json:"own,omitempty"`
	Maker   string    `json:"maker,omitempty"`
	Taker   string    `json:"taker,omitempty"`
	Node    string    `json:"node,omitempty"`
	Info    *PeerInfo `json:"info,omitempty"`
	Stamp   int64     `json:"stamp"`
}

// presence holds the presence state of an OrderBooks.
type presence struct {
	mtx      sync.Mutex
	name     string
	app      string
	nodeFunc func() string
	peers    map[string]*PeerInfo
	lastSent time.Time
	sink     atomic.Pointer[func(*Event)]
	// fakeProto, if set, replaces the announced versions (simnet debug API
	// only, to show how peers treat an incompatible client).
	fakeProto *[2]uint32
}

func newPresence() *presence {
	return &presence{peers: make(map[string]*PeerInfo), nodeFunc: func() string { return "" }}
}

// SetEventSink sets a function that receives mesh activity events. It is
// called synchronously, sometimes with internal locks held, so it must not
// block or call back into OrderBooks.
func (ob *OrderBooks) SetEventSink(f func(*Event)) {
	ob.pres.sink.Store(&f)
}

func (ob *OrderBooks) emit(e *Event) {
	f := ob.pres.sink.Load()
	if f == nil || *f == nil {
		return
	}
	if e.Stamp == 0 {
		e.Stamp = ob.now().UnixMilli()
	}
	(*f)(e)
}

// SetIdentity sets the display name and app version announced in presence
// messages, and the function giving the connected mesh node's peer ID.
func (ob *OrderBooks) SetIdentity(name, app string, node func() string) {
	if len(app) > maxPresenceApp {
		app = app[:maxPresenceApp]
	}
	ob.pres.mtx.Lock()
	ob.pres.name, ob.pres.app = SanitizeName(name), app
	if node != nil {
		ob.pres.nodeFunc = node
	}
	ob.pres.mtx.Unlock()
}

// Name is our announced display name.
func (ob *OrderBooks) Name() string {
	ob.pres.mtx.Lock()
	defer ob.pres.mtx.Unlock()
	return ob.pres.name
}

// Self is our mesh peer ID.
func (ob *OrderBooks) Self() peer.ID { return ob.self }

// ConnectedNode is the peer ID of the mesh node we're connected to, or "".
func (ob *OrderBooks) ConnectedNode() string {
	ob.pres.mtx.Lock()
	f := ob.pres.nodeFunc
	ob.pres.mtx.Unlock()
	return f()
}

// buildPresence creates our signed presence message.
func (ob *OrderBooks) buildPresence() (*Presence, error) {
	ob.pres.mtx.Lock()
	name, app, nodeFunc := ob.pres.name, ob.pres.app, ob.pres.nodeFunc
	proto, minProto := ProtocolVersion, MinProtocolVersion
	if fp := ob.pres.fakeProto; fp != nil {
		proto, minProto = fp[0], fp[1]
	}
	ob.pres.mtx.Unlock()
	pols := make(map[string][2]int64)
	ob.mtx.RLock()
	for m := range ob.books {
		if len(pols) >= maxPresencePolicies {
			break
		}
		name, err := m.Name()
		if err != nil {
			continue
		}
		p := ob.policyLocked(m)
		pols[name] = [2]int64{int64(p.Maker / time.Second), int64(p.Taker / time.Second)}
	}
	ob.mtx.RUnlock()
	p := &Presence{
		Proto:    proto,
		MinProto: minProto,
		App:      app,
		Name:     name,
		Node:     nodeFunc(),
		Policies: pols,
		Stamp:    ob.now().UnixMilli(),
	}
	if err := p.Sign(ob.priv); err != nil {
		return nil, err
	}
	return p, nil
}

// AnnouncePresence broadcasts our presence now.
func (ob *OrderBooks) AnnouncePresence(ctx context.Context) {
	p, err := ob.buildPresence()
	if err != nil {
		ob.log.Errorf("Error building presence: %v", err)
		return
	}
	b, err := json.Marshal(p)
	if err != nil {
		return
	}
	ob.pres.mtx.Lock()
	ob.pres.lastSent = ob.now()
	ob.pres.mtx.Unlock()
	if err := ob.ps.Broadcast(ctx, PresenceTopic, b); err != nil {
		ob.log.Debugf("Error broadcasting presence: %v", err)
	}
}

// SubscribePresence subscribes to the presence topic.
func (ob *OrderBooks) SubscribePresence(ctx context.Context) error {
	err := ob.ps.Subscribe(ctx, PresenceTopic, func(ev mc.TopicEvent) {
		ob.handlePresenceEvent(ctx, ev)
	})
	if err != nil && !errors.Is(err, mc.ErrRedundantSubscription) {
		ob.log.Warnf("Error subscribing to %s: %v", PresenceTopic, err)
	}
	return nil
}

func (ob *OrderBooks) handlePresenceEvent(ctx context.Context, ev mc.TopicEvent) {
	if ev.Peer == ob.self {
		return
	}
	switch ev.Type {
	case mc.TopicEventPeerSubscribed:
		ob.seenPeer(ev.Peer.String(), "topic")
		// Let the newcomer see us soon, but don't let joins flood the topic.
		ob.pres.mtx.Lock()
		due := ob.now().Sub(ob.pres.lastSent) >= minPresenceResend
		ob.pres.mtx.Unlock()
		if due {
			go ob.AnnouncePresence(ctx)
		}
	case mc.TopicEventPeerUnsubscribed:
		ob.peerLeft(ev.Peer.String())
	case mc.TopicEventData:
		var p Presence
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			ob.log.Debugf("Bad presence from %s: %v", ev.Peer, err)
			return
		}
		if p.Owner != ev.Peer.String() {
			ob.log.Debugf("Presence from %s claims owner %s", ev.Peer, p.Owner)
			return
		}
		if err := p.Validate(ob.now()); err != nil {
			ob.log.Debugf("Rejected presence from %s: %v", ev.Peer, err)
			return
		}
		ob.updatePresence(&p)
	}
}

// updatePresence records a validated presence message.
func (ob *OrderBooks) updatePresence(p *Presence) {
	nowMs := ob.now().UnixMilli()
	ob.pres.mtx.Lock()
	pi, found := ob.pres.peers[p.Owner]
	if !found {
		if len(ob.pres.peers) >= maxPeers {
			ob.pres.mtx.Unlock()
			return
		}
		pi = &PeerInfo{ID: p.Owner, FirstSeen: nowMs}
		ob.pres.peers[p.Owner] = pi
	}
	if p.Stamp < pi.LastSeen-int64(MaxClockSkew/time.Millisecond) && pi.Proto != 0 {
		ob.pres.mtx.Unlock()
		return // old message replayed
	}
	wasOnline := pi.Online
	pi.Name, pi.Proto, pi.MinProto, pi.App, pi.Node, pi.Policies = SanitizeName(p.Name), p.Proto, p.MinProto, p.App, p.Node, p.Policies
	pi.LastSeen, pi.Online = nowMs, true
	pi.addSeen("presence")
	pi.Compatible, pi.Reason = ProtocolCompatible(p.Proto, p.MinProto)
	info := pi.copy()
	ob.pres.mtx.Unlock()
	if !wasOnline || !found {
		ob.log.Infof("Mesh peer %s (%q, protocol v%d) is online", p.Owner, info.Name, p.Proto)
	}
	ob.emit(&Event{Kind: EventPeer, Peer: p.Owner, Node: p.Node, Info: info})
}

// seenPeer records a peer seen as an order owner, inbox sender or topic
// subscriber.
func (ob *OrderBooks) seenPeer(id, src string) {
	if id == "" || id == ob.self.String() {
		return
	}
	nowMs := ob.now().UnixMilli()
	ob.pres.mtx.Lock()
	pi, found := ob.pres.peers[id]
	if !found {
		if len(ob.pres.peers) >= maxPeers {
			ob.pres.mtx.Unlock()
			return
		}
		pi = &PeerInfo{ID: id, FirstSeen: nowMs, Compatible: true, Reason: "no presence announcement yet"}
		ob.pres.peers[id] = pi
	}
	wasOnline := pi.Online
	pi.LastSeen, pi.Online = nowMs, true
	pi.addSeen(src)
	info := pi.copy()
	ob.pres.mtx.Unlock()
	if !found || !wasOnline {
		ob.emit(&Event{Kind: EventPeer, Peer: id, Node: info.Node, Info: info})
	}
}

func (ob *OrderBooks) peerLeft(id string) {
	ob.pres.mtx.Lock()
	pi, found := ob.pres.peers[id]
	wasOnline := found && pi.Online
	if found {
		pi.Online = false
	}
	ob.pres.mtx.Unlock()
	if wasOnline {
		ob.log.Infof("Mesh peer %s went offline", id)
		ob.emit(&Event{Kind: EventPeerLeft, Peer: id})
	}
}

// prunePeers marks peers offline after PresenceTimeout without news and
// forgets long-gone peers.
func (ob *OrderBooks) prunePeers() {
	now := ob.now()
	var left []string
	ob.pres.mtx.Lock()
	for id, pi := range ob.pres.peers {
		age := now.UnixMilli() - pi.LastSeen
		if pi.Online && age > PresenceTimeout.Milliseconds() {
			pi.Online = false
			left = append(left, id)
		}
		if age > peerForget.Milliseconds() {
			delete(ob.pres.peers, id)
		}
	}
	ob.pres.mtx.Unlock()
	for _, id := range left {
		ob.emit(&Event{Kind: EventPeerLeft, Peer: id})
	}
}

// RunPresence announces presence every interval and prunes the peer list
// until ctx is done.
func (ob *OrderBooks) RunPresence(ctx context.Context, interval time.Duration) {
	ob.AnnouncePresence(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			ob.AnnouncePresence(ctx)
			ob.prunePeers()
		case <-ctx.Done():
			return
		}
	}
}

// Peers returns what we know about other mesh clients, most recently seen
// first.
func (ob *OrderBooks) Peers() []*PeerInfo {
	ob.pres.mtx.Lock()
	out := make([]*PeerInfo, 0, len(ob.pres.peers))
	for _, pi := range ob.pres.peers {
		out = append(out, pi.copy())
	}
	ob.pres.mtx.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
	return out
}

// Peer returns one peer's info.
func (ob *OrderBooks) Peer(id string) (*PeerInfo, bool) {
	ob.pres.mtx.Lock()
	defer ob.pres.mtx.Unlock()
	pi, found := ob.pres.peers[id]
	if !found {
		return nil, false
	}
	return pi.copy(), true
}

// orderBlock returns why the owner's orders must be hidden (an incompatible
// protocol version), or "".
func (ob *OrderBooks) orderBlock(owner string) string {
	ob.pres.mtx.Lock()
	defer ob.pres.mtx.Unlock()
	pi, found := ob.pres.peers[owner]
	if !found || pi.Proto == 0 || pi.Compatible {
		return ""
	}
	return "incompatible: " + pi.Reason
}

func fmtLock(secs int64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// policyWarning returns a reason a take against the owner's order on m
// would be rejected because the announced lock policies differ, or "".
func (ob *OrderBooks) policyWarning(owner string, m Market) string {
	name, err := m.Name()
	if err != nil {
		return ""
	}
	ob.pres.mtx.Lock()
	pi, found := ob.pres.peers[owner]
	var theirs [2]int64
	var ok bool
	if found {
		theirs, ok = pi.Policies[name]
	}
	ob.pres.mtx.Unlock()
	if !ok {
		return ""
	}
	p := ob.LockPolicy(m)
	ours := [2]int64{int64(p.Maker / time.Second), int64(p.Taker / time.Second)}
	if theirs == ours {
		return ""
	}
	return fmt.Sprintf("lock policy differs: theirs %s/%s, ours %s/%s (maker/taker)",
		fmtLock(theirs[0]), fmtLock(theirs[1]), fmtLock(ours[0]), fmtLock(ours[1]))
}

// SetFakeProtocol makes presence announce the given versions instead of the
// real ones (0, 0 restores them) and announces at once. Simnet debug API
// only: it lets tests show how peers handle an incompatible client.
func (ob *OrderBooks) SetFakeProtocol(ctx context.Context, proto, minProto uint32) {
	ob.pres.mtx.Lock()
	if proto == 0 && minProto == 0 {
		ob.pres.fakeProto = nil
	} else {
		ob.pres.fakeProto = &[2]uint32{proto, minProto}
	}
	ob.pres.mtx.Unlock()
	ob.AnnouncePresence(ctx)
}
