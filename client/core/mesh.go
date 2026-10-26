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
	return &Mesh{}

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
	if err := os.WriteFile(path, enc, 0o600); err != nil {
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
