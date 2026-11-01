// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// An embedded mesh (tatanka) node, for "Help the network: run a mesh node".
// bisonw runs the bisoncraft/mesh node code in-process on its own libp2p
// host, tries to open the port on the router (UPnP, then NAT-PMP), and asks
// other bisonw peers to dial it back (reach.go) to tell whether it is
// reachable from the internet.
//
// The mesh is a permissioned federation: mesh nodes only peer with nodes in
// their whitelist, and whitelist changes need a 2/3 quorum of the existing
// nodes. So a new node only joins the mesh (relays orders and messages) after
// the node operators add its peer ID. Until then it runs, can be reached, and
// reports the whitelist mismatch.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	"github.com/bisoncraft/mesh/tatanka"
	"github.com/decred/slog"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// DefaultNodePort is the default TCP port of the embedded mesh node.
const DefaultNodePort = 17300

// Reachability verdicts.
const (
	ReachUnknown     = "unknown"
	ReachChecking    = "checking"
	ReachReachable   = "reachable"
	ReachUnreachable = "unreachable"
)

// NodeConfig configures an embedded mesh node.
type NodeConfig struct {
	DataDir string
	// Port is the preferred TCP port (0: DefaultNodePort). If it is busy, a
	// free port is used instead.
	Port int
	// Bootstrap is the mesh bootstrap list (multiaddrs with /p2p/<ID>). The
	// bootstrap node IDs make up the whitelist, with Whitelist and our own ID.
	Bootstrap []string
	Whitelist []string
	// PublicIP overrides the advertised public IP (manual port forward or a
	// VPS). Otherwise the port mapping's or the observed external IP is used.
	PublicIP string
	// NoPortMap disables UPnP and NAT-PMP.
	NoPortMap bool
	Log       dex.Logger
}

// NodeStatus is the embedded node's state for the UI.
type NodeStatus struct {
	Running    bool     `json:"running"`
	Starting   bool     `json:"starting"`
	Error      string   `json:"error,omitempty"`
	PeerID     string   `json:"peerID,omitempty"`
	Port       int      `json:"port,omitempty"`
	Listen     []string `json:"listen,omitempty"`
	PublicAddr string   `json:"publicAddr,omitempty"`
	// IPSource is where the public IP came from: "manual", "upnp",
	// "nat-pmp", "observed" or "" (unknown).
	IPSource   string `json:"ipSource,omitempty"`
	PortMap    string `json:"portMap,omitempty"` // "upnp", "nat-pmp" or ""
	PortMapErr string `json:"portMapErr,omitempty"`
	// MeshPeers are whitelisted mesh nodes we are peered with, of
	// MeshNodes known. Clients are other connected peers (bisonw clients).
	MeshPeers int `json:"meshPeers"`
	MeshNodes int `json:"meshNodes"`
	Clients   int `json:"clients"`
	// Mismatch lists mesh nodes that rejected us (whitelist mismatch: our
	// node ID is not in their whitelist yet).
	Mismatch []string `json:"mismatch,omitempty"`
	// Reach is the last reachability verdict.
	Reach        string         `json:"reach"`
	ReachNote    string         `json:"reachNote,omitempty"`
	ReachChecked int64          `json:"reachChecked,omitempty"` // unix ms
	ReachResults []*ReachResult `json:"reachResults,omitempty"`
	StartedAt    int64          `json:"startedAt,omitempty"` // unix ms
}

// ProbeFunc asks peers to dial the node (OrderBooks.ProbeReach).
type ProbeFunc func(ctx context.Context, id peer.ID, addrs []ma.Multiaddr) ([]*ReachResult, error)

// Node is an embedded mesh node.
type Node struct {
	cfg       *NodeConfig
	log       dex.Logger
	whitelist map[peer.ID]bool

	mtx      sync.RWMutex
	st       NodeStatus
	h        host.Host
	mapping  *portMapping
	pubIP    net.IP
	observed net.IP
	mismatch map[peer.ID]time.Time
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewNode prepares a node. Start runs it.
func NewNode(cfg *NodeConfig) (*Node, error) {
	if cfg.DataDir == "" || cfg.Log == nil {
		return nil, errors.New("node: missing data dir or logger")
	}
	n := &Node{cfg: cfg, log: cfg.Log, whitelist: make(map[peer.ID]bool), mismatch: make(map[peer.ID]time.Time)}
	for _, a := range cfg.Bootstrap {
		maddr, err := ma.NewMultiaddr(a)
		if err != nil {
			return nil, fmt.Errorf("bad bootstrap address %q: %w", a, err)
		}
		ai, err := peer.AddrInfoFromP2pAddr(maddr)
		if err != nil {
			return nil, fmt.Errorf("bad bootstrap address %q: %w", a, err)
		}
		n.whitelist[ai.ID] = true
	}
	for _, s := range cfg.Whitelist {
		id, err := peer.Decode(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("bad whitelist peer %q: %w", s, err)
		}
		n.whitelist[id] = true
	}
	if cfg.PublicIP != "" {
		ip := net.ParseIP(cfg.PublicIP)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("bad public IPv4 %q", cfg.PublicIP)
		}
		n.pubIP = ip.To4()
	}
	n.st.Reach = ReachUnknown
	return n, nil
}

// NodePeerID returns (creating if needed) the node's peer ID in dataDir.
func NodePeerID(dataDir string) (peer.ID, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	return tatanka.InitTatankaNode(dataDir)
}

func loadNodeKey(dataDir string) (crypto.PrivKey, error) {
	if _, err := NodePeerID(dataDir); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "p.key"))
	if err != nil {
		return nil, err
	}
	return crypto.UnmarshalPrivateKey(b)
}

// pickPort returns the preferred port if it is free, else a free port.
func pickPort(pref int) (int, error) {
	if pref <= 0 {
		pref = DefaultNodePort
	}
	if l, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", pref)); err == nil {
		l.Close()
		return pref, nil
	}
	l, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Start starts the node in the background and returns the port it uses.
func (n *Node) Start(ctx context.Context) (int, error) {
	n.mtx.Lock()
	if n.cancel != nil {
		port := n.st.Port
		n.mtx.Unlock()
		return port, nil
	}
	port, err := pickPort(n.cfg.Port)
	if err != nil {
		n.mtx.Unlock()
		return 0, fmt.Errorf("no free port: %w", err)
	}
	priv, err := loadNodeKey(n.cfg.DataDir)
	if err != nil {
		n.mtx.Unlock()
		return 0, fmt.Errorf("node key: %w", err)
	}
	id, _ := peer.IDFromPrivateKey(priv)
	n.whitelist[id] = true
	ctx, cancel := context.WithCancel(ctx)
	n.cancel, n.done = cancel, make(chan struct{})
	n.st = NodeStatus{Starting: true, PeerID: id.String(), Port: port, Reach: ReachUnknown,
		MeshNodes: len(n.whitelist) - 1, StartedAt: time.Now().UnixMilli()}
	n.mtx.Unlock()
	go n.run(ctx, priv, port)
	return port, nil
}

// Stop stops the node and removes the port mapping.
func (n *Node) Stop() {
	n.mtx.Lock()
	cancel, done := n.cancel, n.done
	n.mtx.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		n.log.Warnf("Mesh node did not stop in time")
	}
	n.mtx.Lock()
	n.cancel, n.done = nil, nil
	n.st.Running, n.st.Starting = false, false
	n.mtx.Unlock()
}

func (n *Node) setErr(err error) {
	n.mtx.Lock()
	n.st.Error, n.st.Starting, n.st.Running = err.Error(), false, false
	n.mtx.Unlock()
	n.log.Errorf("Mesh node: %v", err)
}

// publicAddr is the advertised public multiaddr, nil if unknown.
func (n *Node) publicAddr() ma.Multiaddr {
	n.mtx.RLock()
	ip, port := n.pubIP, n.st.Port
	if ip == nil && n.mapping != nil {
		ip = n.mapping.ExternalIP
	}
	if ip == nil {
		ip = n.observed
	}
	n.mtx.RUnlock()
	if ip == nil {
		return nil
	}
	a, err := manet.FromNetAddr(&net.TCPAddr{IP: ip, Port: port})
	if err != nil {
		return nil
	}
	return a
}

func (n *Node) run(ctx context.Context, priv crypto.PrivKey, port int) {
	defer close(n.done)

	if !n.cfg.NoPortMap && n.pubIP == nil {
		m, err := mapPort(ctx, port)
		n.mtx.Lock()
		if err != nil {
			n.st.PortMapErr = err.Error()
		} else {
			n.mapping, n.st.PortMap = m, m.Method
		}
		n.mtx.Unlock()
		if err != nil {
			n.log.Infof("Mesh node: no router port mapping: %v", err)
		} else {
			n.log.Infof("Mesh node: router mapped TCP port %d (%s), external IP %s", port, m.Method, m.ExternalIP)
			defer m.remove()
		}
	}

	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port)),
		libp2p.EnableRelay(),
		libp2p.EnableRelayService(),
		libp2p.AddrsFactory(func(addrs []ma.Multiaddr) []ma.Multiaddr {
			pub := n.publicAddr()
			if pub == nil {
				return addrs
			}
			out := []ma.Multiaddr{pub}
			for _, a := range addrs {
				if !a.Equal(pub) {
					out = append(out, a)
				}
			}
			return out
		}),
	)
	if err != nil {
		n.setErr(fmt.Errorf("libp2p host: %w", err))
		return
	}
	n.mtx.Lock()
	n.h = h
	n.mtx.Unlock()
	go n.watchIdentify(ctx, h)

	var wl []peer.ID
	for id := range n.whitelist {
		wl = append(wl, id)
	}
	node, err := tatanka.NewTatankaNode(&tatanka.Config{
		DataDir:        n.cfg.DataDir,
		Logger:         &nodeLogger{Logger: n.log, n: n},
		ListenPort:     port,
		MetricsPort:    -1, // no metrics/pprof server
		BootstrapAddrs: n.cfg.Bootstrap,
		WhitelistPeers: wl,
		ForceWhitelist: true,
	}, tatanka.WithHost(h))
	if err != nil {
		h.Close()
		n.setErr(fmt.Errorf("mesh node: %w", err))
		return
	}
	runErr := make(chan error, 1)
	go func() { runErr <- node.Run(ctx) }()
	go func() {
		if err := node.WaitReady(ctx); err == nil {
			n.mtx.Lock()
			n.st.Starting, n.st.Running = false, true
			n.mtx.Unlock()
			n.log.Infof("Mesh node %s running on TCP port %d", h.ID(), port)
		}
	}()

	renew := time.NewTicker(portMapLifetime / 2)
	defer renew.Stop()
	for {
		select {
		case err := <-runErr:
			if err != nil && ctx.Err() == nil {
				n.setErr(fmt.Errorf("mesh node stopped: %w", err))
			}
			return
		case <-renew.C:
			n.mtx.RLock()
			m := n.mapping
			n.mtx.RUnlock()
			if m != nil {
				if err := m.renew(); err != nil {
					n.log.Warnf("Mesh node: renewing port mapping: %v", err)
				}
			}
		}
	}
}

// watchIdentify learns our external IP from what the whitelisted mesh nodes
// observe, for nodes without a port mapping (manual port forward).
func (n *Node) watchIdentify(ctx context.Context, h host.Host) {
	sub, err := h.EventBus().Subscribe(new(event.EvtPeerIdentificationCompleted))
	if err != nil {
		return
	}
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-sub.Out():
			if !ok {
				return
			}
			ev := e.(event.EvtPeerIdentificationCompleted)
			if !n.whitelist[ev.Peer] || ev.ObservedAddr == nil {
				continue
			}
			ip, err := manet.ToIP(ev.ObservedAddr)
			if err != nil || ip.To4() == nil || ip.IsLoopback() {
				continue
			}
			n.mtx.Lock()
			if !ip.Equal(n.observed) {
				n.observed = ip.To4()
				n.log.Infof("Mesh node: external IP observed by mesh node %s: %s", ev.Peer, ip)
			}
			n.mtx.Unlock()
		}
	}
}

// Status returns the node state.
func (n *Node) Status() *NodeStatus {
	n.mtx.RLock()
	st := n.st
	h := n.h
	st.ReachResults = nil
	for _, r := range n.st.ReachResults {
		rc := *r
		st.ReachResults = append(st.ReachResults, &rc)
	}
	switch {
	case n.pubIP != nil:
		st.IPSource = "manual"
	case n.mapping != nil:
		st.IPSource = n.mapping.Method
	case n.observed != nil:
		st.IPSource = "observed"
	}
	for id, t := range n.mismatch {
		if time.Since(t) < 5*time.Minute {
			st.Mismatch = append(st.Mismatch, id.String())
		}
	}
	n.mtx.RUnlock()
	if pub := n.publicAddr(); pub != nil {
		st.PublicAddr = pub.String()
	}
	if h != nil && (st.Running || st.Starting) {
		for _, a := range h.Network().ListenAddresses() {
			st.Listen = append(st.Listen, a.String())
		}
		for _, p := range h.Network().Peers() {
			if h.Network().Connectedness(p) != network.Connected {
				continue
			}
			if n.whitelist[p] {
				st.MeshPeers++
			} else {
				st.Clients++
			}
		}
	}
	return &st
}

// CheckReach asks peers to dial the node's public address.
func (n *Node) CheckReach(ctx context.Context, probe ProbeFunc) *NodeStatus {
	n.mtx.Lock()
	if !n.st.Running && !n.st.Starting {
		n.mtx.Unlock()
		return n.Status()
	}
	if n.st.Reach == ReachChecking {
		n.mtx.Unlock()
		return n.Status()
	}
	n.st.Reach, n.st.ReachNote = ReachChecking, ""
	id := n.h.ID()
	n.mtx.Unlock()

	verdict, note := ReachUnknown, ""
	var results []*ReachResult
	if pub := n.publicAddr(); pub == nil {
		verdict = ReachUnreachable
		note = "No public address: the router did not open the port (no UPnP/NAT-PMP) and the external IP is unknown."
	} else if probe == nil {
		note = "Not connected to the mesh, so no peer can test the node."
	} else {
		var err error
		results, err = probe(ctx, id, []ma.Multiaddr{pub})
		switch {
		case err != nil:
			note = "Probe failed: " + err.Error()
		case len(results) == 0:
			note = "No peer answered the reachability probe (peers need this bisonw version). Try again later."
		default:
			verdict = ReachUnreachable
			for _, r := range results {
				if r.OK {
					verdict = ReachReachable
				}
			}
			if verdict == ReachUnreachable {
				note = "Peers could not connect to " + pub.String() + ". The router or a firewall blocks incoming connections."
			}
		}
	}
	n.mtx.Lock()
	n.st.Reach, n.st.ReachNote, n.st.ReachResults = verdict, note, results
	n.st.ReachChecked = time.Now().UnixMilli()
	n.mtx.Unlock()
	n.log.Infof("Mesh node reachability: %s (%d answers) %s", verdict, len(results), note)
	return n.Status()
}

// nodeLogger passes the node's logs on and notes whitelist mismatches.
type nodeLogger struct {
	slog.Logger
	n *Node
}

func (l *nodeLogger) note(format string, args []any) {
	var mismatch bool
	var id peer.ID
	for _, a := range args {
		switch v := a.(type) {
		case peer.ID:
			if id == "" {
				id = v
			}
		case error:
			if strings.Contains(v.Error(), "whitelist mismatch") {
				mismatch = true
			}
		}
	}
	if mismatch && id != "" && l.n.whitelist[id] {
		l.n.mtx.Lock()
		l.n.mismatch[id] = time.Now()
		l.n.mtx.Unlock()
	}
}

func (l *nodeLogger) Warnf(format string, args ...any) {
	l.note(format, args)
	l.Logger.Warnf(format, args...)
}

func (l *nodeLogger) Errorf(format string, args ...any) {
	l.note(format, args)
	// The metrics/pprof server is disabled on purpose (MetricsPort -1).
	if strings.HasPrefix(format, "Failed to start metrics server") {
		l.Logger.Debugf(format, args...)
		return
	}
	l.Logger.Errorf(format, args...)
}

func (l *nodeLogger) Infof(format string, args ...any) {
	l.note(format, args)
	if strings.HasPrefix(format, "Metrics available") || strings.HasPrefix(format, "Profiler available") {
		return
	}
	l.Logger.Infof(format, args...)
}

func (l *nodeLogger) Debugf(format string, args ...any) {
	l.note(format, args)
	l.Logger.Debugf(format, args...)
}
