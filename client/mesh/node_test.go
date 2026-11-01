// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	mc "github.com/bisoncraft/mesh/client"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// tBus delivers broadcasts to every subscriber, like the mesh relays do.
type tBus struct {
	mtx  sync.Mutex
	subs map[string][]tSub
}

type tSub struct {
	self peer.ID
	h    mc.TopicHandler
}

type tBusClient struct {
	bus  *tBus
	self peer.ID
}

func (c *tBusClient) Broadcast(_ context.Context, topic string, data []byte) error {
	c.bus.mtx.Lock()
	subs := append([]tSub(nil), c.bus.subs[topic]...)
	c.bus.mtx.Unlock()
	for _, s := range subs {
		go s.h(mc.TopicEvent{Type: mc.TopicEventData, Data: data, Peer: c.self})
	}
	return nil
}

func (c *tBusClient) Subscribe(_ context.Context, topic string, h mc.TopicHandler) error {
	c.bus.mtx.Lock()
	c.bus.subs[topic] = append(c.bus.subs[topic], tSub{c.self, h})
	c.bus.mtx.Unlock()
	return nil
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestCheckReachAddrs(t *testing.T) {
	good := []string{"/ip4/89.40.0.147/tcp/17300", "/ip6/2001:db8::1/tcp/1"}
	if _, err := checkReachAddrs(good, false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{
		nil,
		{"/ip4/1.1.1.1/tcp/1", "/ip4/1.1.1.2/tcp/1", "/ip4/1.1.1.3/tcp/1", "/ip4/1.1.1.4/tcp/1", "/ip4/1.1.1.5/tcp/1"},
		{"/ip4/10.0.0.1/tcp/1"},
		{"/ip4/127.0.0.1/tcp/1"},
		{"/ip4/0.0.0.0/tcp/1"},
		{"/dns4/example.com/tcp/1"},
		{"/ip4/1.1.1.1/udp/1/quic-v1"},
		{"/ip4/1.1.1.1/tcp/1/p2p/12D3KooWEyoppNCUx8Yx66oV9fJnriXwCcXwDDUA2kj6vnc6iDEp"},
	} {
		if _, err := checkReachAddrs(bad, false); err == nil {
			t.Fatalf("no error for %v", bad)
		}
	}
	if _, err := checkReachAddrs([]string{"/ip4/10.0.0.1/tcp/1"}, true); err != nil {
		t.Fatalf("private address refused with allowPrivate: %v", err)
	}
}

func waitForNode(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newTestOB(t *testing.T, bus *tBus) *OrderBooks {
	priv := newPriv(t)
	enc, _ := secp256k1.GeneratePrivateKey()
	ob := NewOrderBooks(&tBusClient{bus, pid(t, priv)}, priv, enc, tLogger)
	ob.SetReachAllowPrivate(true)
	if err := ob.SubscribeReach(tCtx); err != nil {
		t.Fatal(err)
	}
	return ob
}

func TestEmbeddedNodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dirA, dirB, dirC := t.TempDir(), t.TempDir(), t.TempDir()
	idA, err := NodePeerID(dirA)
	if err != nil {
		t.Fatal(err)
	}
	idB, _ := NodePeerID(dirB)
	portA, portB, portC := freePort(t), freePort(t), freePort(t)
	bootA := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", portA, idA)

	// A and B whitelist each other; C is not in A's whitelist.
	a, err := NewNode(&NodeConfig{DataDir: dirA, Port: portA, Whitelist: []string{idB.String()},
		PublicIP: "127.0.0.1", NoPortMap: true, Log: tLogger.SubLogger("A")})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewNode(&NodeConfig{DataDir: dirB, Port: portB, Bootstrap: []string{bootA},
		PublicIP: "127.0.0.1", NoPortMap: true, Log: tLogger.SubLogger("B")})
	c, _ := NewNode(&NodeConfig{DataDir: dirC, Port: portC, Bootstrap: []string{bootA},
		NoPortMap: true, Log: tLogger.SubLogger("C")})
	for _, n := range []*Node{a, b, c} {
		if _, err := n.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer n.Stop()
	}
	waitForNode(t, "A and B peered", 30*time.Second, func() bool {
		return a.Status().MeshPeers == 1 && b.Status().MeshPeers == 1 && b.Status().Running
	})
	if st := b.Status(); st.MeshNodes != 1 || st.PublicAddr != fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", portB) || st.IPSource != "manual" {
		t.Fatalf("wrong B status %+v", st)
	}
	waitForNode(t, "C whitelist mismatch", 30*time.Second, func() bool {
		st := c.Status()
		return len(st.Mismatch) == 1 && st.Mismatch[0] == idA.String() && st.MeshPeers == 0
	})

	// Reachability: a peer dials B and answers.
	reachResultTimeout = 3 * time.Second
	defer func() { reachResultTimeout = 25 * time.Second }()
	bus := &tBus{subs: make(map[string][]tSub)}
	responder, requester := newTestOB(t, bus), newTestOB(t, bus)
	probe := func(ctx context.Context, id peer.ID, addrs []ma.Multiaddr) ([]*ReachResult, error) {
		return requester.ProbeReach(ctx, id, addrs)
	}
	st := b.CheckReach(ctx, probe)
	if st.Reach != ReachReachable || len(st.ReachResults) != 1 || !st.ReachResults[0].OK {
		t.Fatalf("B not reachable: %+v %+v", st, st.ReachResults)
	}

	// Rate limited: the same requester right away gets no answer.
	if st := b.CheckReach(ctx, probe); st.Reach != ReachUnknown {
		t.Fatalf("rate limit: want unknown, got %s", st.Reach)
	}

	// A node at the wrong address (port closed) is not reachable.
	responder.reach.mtx.Lock()
	responder.reach.lastDial, responder.reach.perPeer = time.Time{}, map[string]time.Time{}
	responder.reach.mtx.Unlock()
	closed, _ := ma.NewMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", freePort(t)))
	res, err := requester.ProbeReach(ctx, idB, []ma.Multiaddr{closed})
	if err != nil || len(res) != 1 || res[0].OK || res[0].Err == "" {
		t.Fatalf("closed port: %v %+v", err, res)
	}

	// The dial checks the peer ID: A's address does not prove B's ID.
	responder.reach.mtx.Lock()
	responder.reach.lastDial, responder.reach.perPeer = time.Time{}, map[string]time.Time{}
	responder.reach.mtx.Unlock()
	addrA, _ := ma.NewMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", portA))
	res, _ = requester.ProbeReach(ctx, idB, []ma.Multiaddr{addrA})
	if len(res) != 1 || res[0].OK {
		t.Fatalf("wrong peer ID accepted: %+v", res)
	}

	// C has no public address: unreachable without probing.
	if st := c.CheckReach(ctx, probe); st.Reach != ReachUnreachable {
		t.Fatalf("C: want unreachable, got %s", st.Reach)
	}

	b.Stop()
	if st := b.Status(); st.Running {
		t.Fatal("B still running")
	}
}
