package core

import (
	"context"
	"os"

	meshpkg "decred.org/dcrdex/client/mesh"
	"decred.org/dcrdex/dex"
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

func (c *Core) connectMesh(ctx context.Context) {
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
