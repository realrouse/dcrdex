// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"github.com/bisoncraft/mesh/bond"
	mc "github.com/bisoncraft/mesh/client"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/hdkeychain/v3"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	hdKeyPurposeMesh      uint32 = hdkeychain.HardenedKeyStart + 0x6d657368 // ASCII "mesh"
	hdKeyPurposeMeshInbox uint32 = hdkeychain.HardenedKeyStart + 0x696e6278 // ASCII "inbx"
)

type Client struct {
	*mc.Client
	log          dex.Logger
	ch           chan any
	netTickerMap map[string]uint32
	bonds        []*bond.BondParams
	bootstrap    []string
	*OrderBooks

	nodesMtx  sync.Mutex
	nodes     map[string]*NodeInfo
	connected atomic.Value // string: connected node peer ID
}

type ClientConfig struct {
	Seed []byte
	// BootstrapPeers are mesh node multiaddrs, each ending in /p2p/<peer ID>.
	BootstrapPeers []string
	// Name is the display name announced to other peers (optional).
	Name string
	// AppVersion is announced to other peers.
	AppVersion string
	Bonds      []*bond.BondParams
	Log        dex.Logger
}

// NodeInfo is a mesh node this client knows about.
type NodeInfo struct {
	ID string `json:"id"`
	// Addr is the bootstrap multiaddr, if it is a bootstrap node.
	Addr string `json:"addr,omitempty"`
	// Source: bootstrap, connected or peer (a peer's presence named it).
	Source    string `json:"source"`
	Connected bool   `json:"connected"`
	// LastConnected is when we were last connected to it (unix ms).
	LastConnected int64 `json:"lastConnected,omitempty"`
}

// ParseBootstrapList splits a comma-separated list of multiaddrs and checks
// that each one parses and names a peer (/p2p/<id>).
func ParseBootstrapList(s string) ([]string, error) {
	var out []string
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		maddr, err := ma.NewMultiaddr(a)
		if err != nil {
			return nil, fmt.Errorf("bad mesh bootstrap address %q: %w", a, err)
		}
		if _, err := peer.AddrInfoFromP2pAddr(maddr); err != nil {
			return nil, fmt.Errorf("mesh bootstrap address %q needs a /p2p/<peer ID> part: %w", a, err)
		}
		out = append(out, a)
	}
	return out, nil
}

func NewClient(cfg *ClientConfig) (*Client, error) {
	defer encode.ClearBytes(cfg.Seed)

	meshPriv, err := deriveMeshPriv(cfg.Seed)
	if err != nil {
		return nil, fmt.Errorf("error deriving mesh private key: %w", err)
	}
	encPriv := deriveInboxPriv(cfg.Seed)
	if len(cfg.BootstrapPeers) == 0 {
		return nil, errors.New("no mesh bootstrap addresses")
	}
	cl, err := mc.NewClient(&mc.Config{
		PrivateKey:      meshPriv,
		RemotePeerAddrs: cfg.BootstrapPeers,
		Logger:          cfg.Log.SubLogger("mc"),
	})
	if err != nil {
		return nil, err
	}

	c := &Client{
		Client:       cl,
		bonds:        cfg.Bonds,
		log:          cfg.Log,
		ch:           make(chan interface{}, 128),
		netTickerMap: asset.NetworkTickers(),
		bootstrap:    cfg.BootstrapPeers,
		nodes:        make(map[string]*NodeInfo),
	}
	c.connected.Store("")
	for _, a := range cfg.BootstrapPeers {
		if maddr, err := ma.NewMultiaddr(a); err == nil {
			if ai, err := peer.AddrInfoFromP2pAddr(maddr); err == nil {
				c.nodes[ai.ID.String()] = &NodeInfo{ID: ai.ID.String(), Addr: a, Source: "bootstrap"}
			}
		}
	}
	c.OrderBooks = NewOrderBooks(cl, meshPriv, encPriv, cfg.Log.SubLogger("BOOK"))
	c.OrderBooks.SetIdentity(cfg.Name, cfg.AppVersion, c.ConnectedNode)
	return c, nil
}

// ConnectedNode is the peer ID of the mesh node we're connected to, or "".
func (c *Client) ConnectedNode() string {
	return c.connected.Load().(string)
}

// Bootstrap returns the bootstrap multiaddrs.
func (c *Client) Bootstrap() []string {
	return append([]string(nil), c.bootstrap...)
}

// KnownNodes lists the mesh nodes we know: bootstrap nodes, nodes we've been
// connected to, and nodes other peers announced they're connected to.
func (c *Client) KnownNodes() []*NodeInfo {
	for _, pi := range c.OrderBooks.Peers() {
		if pi.Node != "" && pi.Online {
			c.noteNode(pi.Node, "peer", false)
		}
	}
	cur := c.ConnectedNode()
	c.nodesMtx.Lock()
	out := make([]*NodeInfo, 0, len(c.nodes))
	for _, n := range c.nodes {
		cp := *n
		cp.Connected = n.ID == cur
		out = append(out, &cp)
	}
	c.nodesMtx.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (c *Client) noteNode(id, src string, connected bool) {
	c.nodesMtx.Lock()
	defer c.nodesMtx.Unlock()
	n, found := c.nodes[id]
	if !found {
		if len(c.nodes) >= 256 {
			return
		}
		n = &NodeInfo{ID: id, Source: src}
		c.nodes[id] = n
	}
	if connected {
		n.LastConnected = time.Now().UnixMilli()
	}
}

// monitorConnection tracks the connected mesh node and emits EventConn when
// it changes (connect, disconnect or failover to another node).
func (c *Client) monitorConnection(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var feed <-chan bool
	for {
		node := c.Client.ConnectedTatankaNodePeerID()
		if prev := c.ConnectedNode(); node != prev {
			c.connected.Store(node)
			status := "connected"
			if node == "" {
				status = "disconnected"
				c.log.Warnf("Lost the mesh connection (was node %s)", prev)
			} else {
				c.noteNode(node, "connected", true)
				c.log.Infof("Mesh connection: node %s", node)
				// Tell peers which node we're on now.
				go c.OrderBooks.AnnouncePresence(ctx)
			}
			c.OrderBooks.emit(&Event{Kind: EventConn, Status: status, Node: node})
		}
		if feed == nil && node != "" {
			// The library's state feed is only usable once it runs.
			if f, err := c.Client.ConnectionStateFeed(ctx); err == nil {
				feed = f
			}
		}
		select {
		case <-tick.C:
		case _, ok := <-feed:
			if !ok {
				feed = nil
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	var wg sync.WaitGroup
	if err := c.OrderBooks.SubscribeInbox(ctx); err != nil {
		return nil, fmt.Errorf("error subscribing to inbox: %w", err)
	}
	if err := c.OrderBooks.SubscribePresence(ctx); err != nil {
		return nil, fmt.Errorf("error subscribing to presence: %w", err)
	}
	// Subscribe to the default markets so their books and peers show up
	// without any user action.
	for _, dm := range DefaultMarkets {
		if m, err := ParseMarket(dm.Name); err == nil {
			_ = c.OrderBooks.SubscribeMarket(ctx, m)
		}
	}
	wg.Add(4)
	go func() {
		c.monitorConnection(ctx)
		wg.Done()
	}()
	go func() {
		c.OrderBooks.RunPresence(ctx, DefaultPresenceInterval)
		wg.Done()
	}()
	go func() {
		c.OrderBooks.RunMaintenance(ctx, DefaultRebroadcastInterval)
		wg.Done()
	}()
	go func() {
		if err := c.Client.Run(ctx, c.bonds); err != nil {
			c.log.Errorf("mesh client Run error: %v", err)
		}
		wg.Done()
	}()
	return &wg, nil
}

type TickerPriceUpdate struct {
	Ticker string
	Price  float64
}

func (c *Client) SubscribeToTickerPrices(ctx context.Context) error {
	for _, ticker := range asset.Tickers() {
		ticker := ticker
		err := c.Client.SubscribeToPriceOracle(ctx, ticker, func(price float64) {
			c.emit(ctx, &TickerPriceUpdate{Ticker: ticker, Price: price})
		})
		if err != nil && !errors.Is(err, mc.ErrRedundantSubscription) {
			return fmt.Errorf("error subscribing to %s price: %w", ticker, err)
		}
	}
	return nil
}

type FeeRateUpdate struct {
	NetAssetID uint32
	FeeRate    uint64
}

func (c *Client) SubscribeToFeeRateEstimates(ctx context.Context) error {
	netTickers := make([]string, 0, 10)
	for _, a := range asset.Assets() {
		// Base Network assets don't have a "." in their symbol.
		if len(strings.Split(a.Symbol, ".")) != 1 {
			continue
		}
		netTickers = append(netTickers, a.Info.UnitInfo.Conventional.Unit)
	}
	for _, netTicker := range netTickers {
		netAssetID, ok := c.netTickerMap[netTicker]
		if !ok {
			c.log.Warnf("Unknown network identifier %q", netTicker)
			continue
		}
		err := c.Client.SubscribeToFeeRateOracle(ctx, netTicker, func(feeRate *big.Int) {
			c.emit(ctx, &FeeRateUpdate{NetAssetID: netAssetID, FeeRate: feeRate.Uint64()})
		})
		if err != nil && !errors.Is(err, mc.ErrRedundantSubscription) {
			return fmt.Errorf("error subscribing to %s fee rate: %w", netTicker, err)
		}
	}
	return nil
}

// Next returns a channel of mesh updates (*TickerPriceUpdate, *FeeRateUpdate).
func (c *Client) Next() <-chan any {
	return c.ch
}

func (c *Client) emit(ctx context.Context, thing any) {
	select {
	case c.ch <- thing:
	case <-time.After(time.Second * 10):
		c.log.Errorf("Mesh event channel blocking for > 10 seconds: %v", thing)
	case <-ctx.Done():
	}
}

// deriveInboxPriv derives the static secp256k1 inbox encryption key.
func deriveInboxPriv(seed []byte) *secp256k1.PrivateKey {
	k := blake256.Sum256(append(append([]byte{}, seed...), encode.Uint32Bytes(hdKeyPurposeMeshInbox)...))
	return secp256k1.PrivKeyFromBytes(k[:])
}

func deriveMeshPriv(seed []byte) (libp2pcrypto.PrivKey, error) {
	entropy := blake256.Sum256(append(append([]byte{}, seed...), encode.Uint32Bytes(hdKeyPurposeMesh)...)) // copy: never write into the caller's seed buffer
	priv, _, err := libp2pcrypto.GenerateEd25519Key(bytes.NewReader(entropy[:]))
	return priv, err
}
