// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// Reachability checks for mesh nodes run by bisonw users ("Help the network:
// run a mesh node"). The mesh nodes have no dial-back service, so the check
// is done by other bisonw clients: we publish a probe on ReachTopic naming
// our node's peer ID and public addresses, and online peers that run this
// version dial it with a throwaway libp2p host and publish the result. A dial
// only succeeds if the node at that address proves the named peer ID in the
// libp2p handshake, and probes are rate limited, so the probe can't be used to
// make peers connect to arbitrary services in bulk.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	mc "github.com/bisoncraft/mesh/client"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

const (
	// ReachTopic carries mesh node reachability probes and their results.
	ReachTopic = "reach.tatanka-dex"

	reachDialTimeout  = 10 * time.Second
	reachMaxAddrs     = 4
	reachProbeMaxAge  = 30 * time.Second
	reachGlobalEvery  = 3 * time.Second // we dial at most once per this
	reachPerPeerEvery = 30 * time.Second
)

// reachResultTimeout is how long a probe waits for answers (var for tests).
var reachResultTimeout = 25 * time.Second

// reachMsg is a probe ("probe": dial my node) or a result ("result").
type reachMsg struct {
	Type   string   `json:"type"`
	Nonce  string   `json:"nonce"`
	Node   string   `json:"node,omitempty"`   // probe: node peer ID
	Addrs  []string `json:"addrs,omitempty"`  // probe: node multiaddrs
	Target string   `json:"target,omitempty"` // result: who asked
	OK     bool     `json:"ok,omitempty"`
	Err    string   `json:"err,omitempty"`
	Stamp  int64    `json:"stamp"`
}

// ReachResult is one peer's answer to a reachability probe.
type ReachResult struct {
	Peer string `json:"peer"`
	Name string `json:"name,omitempty"`
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
}

type reach struct {
	allowPrivate bool

	mtx      sync.Mutex
	dialer   host.Host
	lastDial time.Time
	perPeer  map[string]time.Time
	waiters  map[string]chan *ReachResult // by nonce
}

func newReach() *reach {
	return &reach{perPeer: make(map[string]time.Time), waiters: make(map[string]chan *ReachResult)}
}

// SetReachAllowPrivate allows probing private and loopback addresses (simnet
// and local tests).
func (ob *OrderBooks) SetReachAllowPrivate(allow bool) {
	ob.reach.mtx.Lock()
	ob.reach.allowPrivate = allow
	ob.reach.mtx.Unlock()
}

// SubscribeReach subscribes to the reachability topic, so we answer probes.
func (ob *OrderBooks) SubscribeReach(ctx context.Context) error {
	err := ob.ps.Subscribe(ctx, ReachTopic, func(ev mc.TopicEvent) {
		if ev.Type != mc.TopicEventData || ev.Peer == ob.self {
			return
		}
		var m reachMsg
		if err := json.Unmarshal(ev.Data, &m); err != nil || len(m.Nonce) != 32 {
			return
		}
		switch m.Type {
		case "probe":
			go ob.answerProbe(ctx, ev.Peer.String(), &m)
		case "result":
			if m.Target != ob.self.String() {
				return
			}
			ob.reach.mtx.Lock()
			ch := ob.reach.waiters[m.Nonce]
			ob.reach.mtx.Unlock()
			if ch != nil {
				r := &ReachResult{Peer: ev.Peer.String(), OK: m.OK, Err: m.Err}
				if pi, ok := ob.Peer(r.Peer); ok && pi != nil {
					r.Name = pi.Name
				}
				select {
				case ch <- r:
				default:
				}
			}
		}
	})
	if err != nil && !errors.Is(err, mc.ErrRedundantSubscription) {
		return err
	}
	return nil
}

// checkReachAddrs parses probe addresses: plain ip4/ip6 TCP only.
func checkReachAddrs(addrs []string, allowPrivate bool) ([]ma.Multiaddr, error) {
	if len(addrs) == 0 || len(addrs) > reachMaxAddrs {
		return nil, errors.New("bad address count")
	}
	out := make([]ma.Multiaddr, 0, len(addrs))
	for _, s := range addrs {
		a, err := ma.NewMultiaddr(s)
		if err != nil {
			return nil, err
		}
		ps := a.Protocols()
		if len(ps) != 2 || (ps[0].Code != ma.P_IP4 && ps[0].Code != ma.P_IP6) || ps[1].Code != ma.P_TCP {
			return nil, fmt.Errorf("unsupported address %s", s)
		}
		ip, err := manet.ToIP(a)
		if err != nil {
			return nil, err
		}
		if ip.IsUnspecified() || ip.IsMulticast() || ((ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) && !allowPrivate) {
			return nil, fmt.Errorf("address %s is not public", s)
		}
		out = append(out, a)
	}
	return out, nil
}

func (ob *OrderBooks) answerProbe(ctx context.Context, from string, m *reachMsg) {
	now := ob.now()
	if d := now.Sub(time.UnixMilli(m.Stamp)); d > reachProbeMaxAge || d < -MaxClockSkew {
		return
	}
	nodeID, err := peer.Decode(m.Node)
	if err != nil {
		return
	}
	ob.reach.mtx.Lock()
	allowPrivate := ob.reach.allowPrivate
	if now.Sub(ob.reach.lastDial) < reachGlobalEvery || now.Sub(ob.reach.perPeer[from]) < reachPerPeerEvery {
		ob.reach.mtx.Unlock()
		return
	}
	ob.reach.lastDial, ob.reach.perPeer[from] = now, now
	if len(ob.reach.perPeer) > 1000 {
		ob.reach.perPeer = map[string]time.Time{from: now}
	}
	dialer := ob.reach.dialer
	if dialer == nil {
		dialer, err = libp2p.New(libp2p.NoListenAddrs, libp2p.DisableRelay())
		if err != nil {
			ob.reach.mtx.Unlock()
			ob.log.Errorf("Reachability dialer: %v", err)
			return
		}
		ob.reach.dialer = dialer
	}
	ob.reach.mtx.Unlock()

	res := &reachMsg{Type: "result", Nonce: m.Nonce, Target: from, Stamp: now.UnixMilli()}
	addrs, err := checkReachAddrs(m.Addrs, allowPrivate)
	if err == nil {
		dctx, cancel := context.WithTimeout(ctx, reachDialTimeout)
		err = dialer.Connect(dctx, peer.AddrInfo{ID: nodeID, Addrs: addrs})
		cancel()
		_ = dialer.Network().ClosePeer(nodeID)
		dialer.Peerstore().ClearAddrs(nodeID)
	}
	if err != nil {
		res.Err = shortDialErr(err)
	} else {
		res.OK = true
	}
	ob.log.Infof("Mesh node reachability probe from %s for %s: ok=%v %s", from, nodeID, res.OK, res.Err)
	b, _ := json.Marshal(res)
	if err := ob.ps.Broadcast(ctx, ReachTopic, b); err != nil {
		ob.log.Debugf("Error publishing reachability result: %v", err)
	}
}

// shortDialErr keeps dial errors short for the UI and the 1 KiB message limit.
func shortDialErr(err error) string {
	var nerr net.Error
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "connection refused (nothing listens on that port)"
	case strings.Contains(s, "peer id mismatch"):
		return "a different node answered on that address"
	case errors.As(err, &nerr) && nerr.Timeout(), errors.Is(err, context.DeadlineExceeded),
		strings.Contains(s, "i/o timeout"), strings.Contains(s, "deadline exceeded"):
		return "timed out (no answer: port closed or filtered)"
	case strings.Contains(s, "no route to host"), strings.Contains(s, "network is unreachable"):
		return "no route to that address"
	case len(s) > 160:
		s = s[:160] + "..."
	}
	return s
}

// ProbeReach asks online peers to dial our node and collects their answers
// for up to reachResultTimeout (or until three answered).
func (ob *OrderBooks) ProbeReach(ctx context.Context, nodeID peer.ID, addrs []ma.Multiaddr) ([]*ReachResult, error) {
	var nb [16]byte
	_, _ = rand.Read(nb[:])
	m := &reachMsg{Type: "probe", Nonce: hex.EncodeToString(nb[:]), Node: nodeID.String(), Stamp: ob.now().UnixMilli()}
	for _, a := range addrs {
		if len(m.Addrs) == reachMaxAddrs {
			break
		}
		m.Addrs = append(m.Addrs, a.String())
	}
	ch := make(chan *ReachResult, 8)
	ob.reach.mtx.Lock()
	ob.reach.waiters[m.Nonce] = ch
	ob.reach.mtx.Unlock()
	defer func() {
		ob.reach.mtx.Lock()
		delete(ob.reach.waiters, m.Nonce)
		ob.reach.mtx.Unlock()
	}()
	b, _ := json.Marshal(m)
	if err := ob.ps.Broadcast(ctx, ReachTopic, b); err != nil {
		return nil, fmt.Errorf("publishing probe: %w", err)
	}
	var out []*ReachResult
	timer := time.NewTimer(reachResultTimeout)
	defer timer.Stop()
	for len(out) < 3 {
		select {
		case r := <-ch:
			out = append(out, r)
		case <-timer.C:
			return out, nil
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
	return out, nil
}
