// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"github.com/bisoncraft/mesh/bond"
	mc "github.com/bisoncraft/mesh/client"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/hdkeychain/v3"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
)

const hdKeyPurposeMesh uint32 = hdkeychain.HardenedKeyStart + 0x6d657368 // ASCII "mesh"

type Client struct {
	*mc.Client
	log          dex.Logger
	ch           chan any
	netTickerMap map[string]uint32
	bonds        []*bond.BondParams
	*OrderBooks
}

type ClientConfig struct {
	Seed          []byte
	BootstrapPeer string
	Bonds         []*bond.BondParams
	Log           dex.Logger
}

func NewClient(cfg *ClientConfig) (*Client, error) {
	defer encode.ClearBytes(cfg.Seed)

	meshPriv, err := deriveMeshPriv(cfg.Seed)
	if err != nil {
		return nil, fmt.Errorf("error deriving mesh private key: %w", err)
	}
	cl, err := mc.NewClient(&mc.Config{
		PrivateKey:      meshPriv,
		RemotePeerAddrs: []string{cfg.BootstrapPeer},
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
	}
	c.OrderBooks = NewOrderBooks(cl, meshPriv, cfg.Log.SubLogger("BOOK"))
	return c, nil
}

func (c *Client) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	var wg sync.WaitGroup
	wg.Add(2)
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

func deriveMeshPriv(seed []byte) (libp2pcrypto.PrivKey, error) {
	entropy := blake256.Sum256(append(seed, encode.Uint32Bytes(hdKeyPurposeMesh)...))
	priv, _, err := libp2pcrypto.GenerateEd25519Key(bytes.NewReader(entropy[:]))
	return priv, err
}
