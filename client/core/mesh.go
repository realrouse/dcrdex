package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	meshpkg "decred.org/dcrdex/client/mesh"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/encrypt"
)

func (c *Core) coreMesh() *Mesh {
	c.meshMtx.RLock()
	mesh, meshCM := c.mesh, c.meshCM
	c.meshMtx.RUnlock()
	if mesh == nil || !meshCM.On() {
		return nil
	}
	mkts := make(map[string]*MeshMarket, len(meshpkg.DefaultMarkets))
	for _, d := range meshpkg.DefaultMarkets {
		if m, err := meshpkg.ParseMarket(d.Name); err == nil {
			mkts[d.Name] = &MeshMarket{BaseID: m.Base, QuoteID: m.Quote, LotSize: d.LotSize}
		}
	}
	return &Mesh{Markets: mkts}

}

// meshSwapStoreKey loads (or creates) the 32-byte swap store key, kept on disk
// encrypted with the app crypter.
func meshSwapStoreKey(path string, crypter encrypt.Crypter) ([]byte, error) {
	if enc, err := os.ReadFile(path); err == nil {
		key, err := crypter.Decrypt(enc)
		if err != nil {
			return nil, fmt.Errorf("decrypting swap store key: %w", err)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := encode.RandomBytes(32)
	enc, err := crypter.Encrypt(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Write atomically: a crash mid-write must not leave a truncated key
	// file that would make every stored secret unreadable.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return nil, err
	}
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		f.Close()
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return key, nil
}

func (c *Core) connectMesh(ctx context.Context, crypter encrypt.Crypter) {
	if c.net != dex.Simnet || !c.cfg.Mesh {
		return
	}
	c.meshMtx.RLock()
	mesh, meshCM := c.mesh, c.meshCM
	c.meshMtx.RUnlock()
	var err error
	if err = meshCM.ConnectOnce(c.ctx); err != nil {
		c.log.Errorf("error connecting mesh: %v", err)
		return
	}
	defer func() {
		if err != nil {
			c.log.Error("Failed to initialize mesh subscriptions. Closing connection: %v", err)
			meshCM.Disconnect()
		}
	}()

	c.log.Infof("Connected to Mesh")

	// Mesh activity goes to the UI as data notifications. The sink must not
	// block; notify never does.
	mesh.SetEventSink(func(e *meshpkg.Event) { c.notify(newMeshNote(e)) })

	go func() {
		for {
			select {
			case u := <-mesh.Next():
				c.handleMeshUpdate(u)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Swap parameters need our receiving address for the asset we get.
	mesh.SetAddressFunc(func(assetID uint32) (string, error) {
		if _, err := c.connectedWallet(assetID); err != nil {
			return "", err
		}
		return c.NewDepositAddress(assetID)
	})

	// Atomic swaps for agreed matches (P3), persisted next to the DB.
	// The secrets in it are encrypted with a key that is itself encrypted
	// with the app crypter, so they are only readable after login.
	meshDir := filepath.Join(filepath.Dir(c.cfg.DBPath), "mesh")
	// Trade (price) history for the mesh markets' candles. Loaded before
	// the swaps, so completed swaps aren't re-signed.
	if n, err := mesh.SetTradeStore(filepath.Join(meshDir, "trades.json")); err != nil {
		c.log.Errorf("Error loading mesh trade history: %v", err)
	} else if n > 0 {
		c.log.Infof("Loaded %d mesh trades", n)
	}
	var store *meshpkg.SwapStore
	key, serr := meshSwapStoreKey(filepath.Join(meshDir, "swapstore.key"), crypter)
	if serr == nil {
		store, serr = meshpkg.NewSwapStore(filepath.Join(meshDir, "swaps"), key)
	}
	if serr != nil {
		// Never run swaps without persistence.
		c.log.Errorf("Mesh swap store error, swaps disabled: %v", serr)
		return
	}
	mesh.EnableSwaps(meshpkg.DefaultSwapConfig, func(assetID uint32) (meshpkg.SwapWallet, error) {
		return &meshSwapWallet{c: c, assetID: assetID}, nil
	}, store)
	if n, err := mesh.LoadSwaps(); err != nil {
		c.log.Errorf("Error loading mesh swaps: %v", err)
	} else if n > 0 {
		c.log.Infof("Resuming %d mesh swaps", n)
	}
	// Login only unlocks wallets with active DEX trades. Resumed mesh swaps
	// need theirs unlocked too, while the crypter is still open.
	c.unlockMeshSwapWallets(crypter, mesh.Swaps())
	// Mesh orders and matches arrive without a password prompt, so unlock
	// the other wallets now too (simnet prototype; see PLAN.md P5 notes).
	c.unlockMeshWallets(crypter)
	go mesh.RunSwaps(ctx, 3*time.Second)

	// SIMNET-ONLY debug API for driving mesh order books from test scripts.
	if addr := os.Getenv("TATANKA_MESH_DEBUG_ADDR"); addr != "" {
		srv := meshpkg.NewDebugServer(mesh.OrderBooks, func() string { return mesh.PeerID().String() })
		go func() {
			c.log.Infof("Mesh debug API listening on %s", addr)
			if err := srv.ListenAndServe(ctx, addr); err != nil {
				c.log.Errorf("Mesh debug API error: %v", err)
			}
		}()
	}

	// Probes for mesh node reachability may name private addresses on
	// simnet (local test setups).
	mesh.OrderBooks.SetReachAllowPrivate(c.net == dex.Simnet)
	// The optional embedded mesh node (mesh_node.go).
	c.startMeshNodeIfEnabled()

	if err = mesh.SubscribeToFeeRateEstimates(ctx); err != nil {
		c.log.Errorf("error subscribing to mesh fee rate estimates: %v", err)
		return
	}
	if err = mesh.SubscribeToTickerPrices(ctx); err != nil {
		c.log.Error("error subscribing to mesh fiat rates: %v", err)
		return
	}
}

func (c *Core) handleMeshUpdate(u any) {
	switch u := u.(type) {
	case *meshpkg.TickerPriceUpdate:
		c.log.Debugf("Mesh price update: %s = %f", u.Ticker, u.Price)
	case *meshpkg.FeeRateUpdate:
		c.log.Debugf("Mesh fee rate update: asset %d = %d", u.NetAssetID, u.FeeRate)
	default:
		c.log.Warnf("Unknown mesh update type %T", u)
	}
}

// unlockMeshSwapWallets connects and unlocks the wallets of both assets of
// every unfinished mesh swap.
func (c *Core) unlockMeshSwapWallets(crypter encrypt.Crypter, recs []*meshpkg.SwapRecord) {
	need := make(map[uint32]bool)
	for _, r := range recs {
		if r.Active() {
			need[r.Match.Market.Base] = true
			need[r.Match.Market.Quote] = true
		}
	}
	for assetID := range need {
		w, found := c.wallet(assetID)
		if !found {
			c.log.Errorf("No %s wallet for a resumed mesh swap", dex.BipIDSymbol(assetID))
			continue
		}
		if err := c.connectAndUnlock(crypter, w); err != nil {
			c.log.Errorf("Error unlocking %s wallet for resumed mesh swaps: %v", dex.BipIDSymbol(assetID), err)
		}
	}
}

// unlockMeshWallets connects and unlocks every configured wallet so mesh
// swaps on any market can proceed after login.
func (c *Core) unlockMeshWallets(crypter encrypt.Crypter) {
	for _, w := range c.xcWallets() {
		if w.unlocked() {
			continue
		}
		if err := c.connectAndUnlock(crypter, w); err != nil {
			c.log.Errorf("Error unlocking %s wallet for mesh swaps: %v", w.Symbol, err)
		}
	}
}

// meshDisplayName is the name announced to mesh peers: --meshname if set,
// else a random adjective-animal-NNNN name kept in <data dir>/mesh/name.
// Never the host or user name.
func (c *Core) meshDisplayName() string {
	if n := meshpkg.SanitizeName(c.cfg.MeshName); n != "" {
		return n
	}
	name, err := meshpkg.LoadOrCreateName(filepath.Join(filepath.Dir(c.cfg.DBPath), "mesh", "name"))
	if err != nil {
		c.log.Errorf("Error saving the random mesh name: %v", err)
	}
	return name
}
