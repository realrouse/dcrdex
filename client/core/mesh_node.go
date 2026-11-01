// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

// The optional embedded mesh node ("Help the network: run a mesh node").
// Simnet with --mesh only for now. Off by default; the setting is kept in
// <data dir>/mesh/node.json.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	meshpkg "decred.org/dcrdex/client/mesh"
	"decred.org/dcrdex/dex"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// MeshNodeSettings is the persisted node setting.
type MeshNodeSettings struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port,omitempty"`
}

// MeshNodeInfo is the embedded node's setting and state.
type MeshNodeInfo struct {
	// Available is false where the node can't run (not simnet with --mesh).
	Available bool                `json:"available"`
	Enabled   bool                `json:"enabled"`
	Status    *meshpkg.NodeStatus `json:"status,omitempty"`
}

// ErrMeshNodeUnavailable is returned where the node can't run.
var ErrMeshNodeUnavailable = errors.New("running a mesh node is only available on simnet with --mesh for now")

func (c *Core) meshNodeAvailable() bool {
	return c.net == dex.Simnet && c.cfg.Mesh
}

func (c *Core) meshNodeDir() string {
	return filepath.Join(filepath.Dir(c.cfg.DBPath), "mesh", "node")
}

func (c *Core) meshNodeSettingsPath() string {
	return filepath.Join(filepath.Dir(c.cfg.DBPath), "mesh", "node.json")
}

func (c *Core) loadMeshNodeSettings() *MeshNodeSettings {
	s := new(MeshNodeSettings)
	if b, err := os.ReadFile(c.meshNodeSettingsPath()); err == nil {
		if err := json.Unmarshal(b, s); err != nil {
			c.log.Errorf("Bad mesh node settings file: %v", err)
		}
	}
	return s
}

func (c *Core) saveMeshNodeSettings(s *MeshNodeSettings) error {
	path := c.meshNodeSettingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MeshNodeStatus returns the embedded mesh node's setting and state.
func (c *Core) MeshNodeStatus() *MeshNodeInfo {
	info := &MeshNodeInfo{Available: c.meshNodeAvailable()}
	if !info.Available {
		return info
	}
	info.Enabled = c.loadMeshNodeSettings().Enabled
	c.meshNodeMtx.Lock()
	n := c.meshNode
	c.meshNodeMtx.Unlock()
	if n != nil {
		info.Status = n.Status()
		// Peer names from presence, for the probe answers.
		if m, err := c.meshClient(); err == nil {
			for _, r := range info.Status.ReachResults {
				if pi, ok := m.OrderBooks.Peer(r.Peer); ok && pi != nil && pi.Name != "" {
					r.Name = pi.Name
				}
			}
		}
	}
	return info
}

// SetMeshNode turns the embedded mesh node on or off, and saves the setting.
func (c *Core) SetMeshNode(enabled bool) (*MeshNodeInfo, error) {
	if !c.meshNodeAvailable() {
		return nil, ErrMeshNodeUnavailable
	}
	s := c.loadMeshNodeSettings()
	s.Enabled = enabled
	if err := c.saveMeshNodeSettings(s); err != nil {
		return nil, err
	}
	if enabled {
		if err := c.startMeshNode(); err != nil {
			return nil, err
		}
	} else {
		c.stopMeshNode()
	}
	return c.MeshNodeStatus(), nil
}

// MeshNodeCheck starts a reachability check. The result shows up in
// MeshNodeStatus (Reach goes "checking", then the verdict).
func (c *Core) MeshNodeCheck() (*MeshNodeInfo, error) {
	if !c.meshNodeAvailable() {
		return nil, ErrMeshNodeUnavailable
	}
	c.meshNodeMtx.Lock()
	n := c.meshNode
	c.meshNodeMtx.Unlock()
	if n == nil {
		return nil, errors.New("the mesh node is not running")
	}
	go n.CheckReach(c.ctx, c.meshProbe())
	time.Sleep(100 * time.Millisecond) // let it flip to "checking"
	return c.MeshNodeStatus(), nil
}

// meshProbe returns the mesh client's reachability probe, nil if the client
// isn't connected.
func (c *Core) meshProbe() meshpkg.ProbeFunc {
	m, err := c.meshClient()
	if err != nil || m.ConnectedNode() == "" {
		return nil
	}
	return func(ctx context.Context, id peer.ID, addrs []ma.Multiaddr) ([]*meshpkg.ReachResult, error) {
		return m.OrderBooks.ProbeReach(ctx, id, addrs)
	}
}

// startMeshNodeIfEnabled starts the node at login if the setting is on.
func (c *Core) startMeshNodeIfEnabled() {
	if !c.meshNodeAvailable() || !c.loadMeshNodeSettings().Enabled {
		return
	}
	if err := c.startMeshNode(); err != nil {
		c.log.Errorf("Error starting mesh node: %v", err)
	}
}

func (c *Core) startMeshNode() error {
	c.meshNodeMtx.Lock()
	defer c.meshNodeMtx.Unlock()
	if c.meshNode != nil {
		return nil
	}
	bootstrap, err := c.meshBootstrap()
	if err != nil {
		return err
	}
	cfg := &meshpkg.NodeConfig{
		DataDir:   c.meshNodeDir(),
		Port:      c.loadMeshNodeSettings().Port,
		Bootstrap: bootstrap,
		// Test overrides (simnet): a manual public IP (port forward or
		// VPS), extra whitelisted nodes, and no router port mapping.
		PublicIP:  strings.TrimSpace(os.Getenv("TATANKA_MESH_NODE_IP")),
		NoPortMap: os.Getenv("TATANKA_MESH_NODE_NOPORTMAP") == "1",
		Log:       c.log.SubLogger("MNODE"),
	}
	// Extra node peers (multiaddrs with /p2p/<ID>), e.g. a private test
	// federation: they are dialed and whitelisted like the bootstrap nodes.
	if ps := os.Getenv("TATANKA_MESH_NODE_PEERS"); ps != "" {
		extra, err := meshpkg.ParseBootstrapList(ps)
		if err != nil {
			return err
		}
		cfg.Bootstrap = append(append([]string(nil), cfg.Bootstrap...), extra...)
	}
	if wl := os.Getenv("TATANKA_MESH_NODE_WHITELIST"); wl != "" {
		for _, s := range strings.Split(wl, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.Whitelist = append(cfg.Whitelist, s)
			}
		}
	}
	n, err := meshpkg.NewNode(cfg)
	if err != nil {
		return err
	}
	port, err := n.Start(c.ctx)
	if err != nil {
		return err
	}
	c.meshNode = n
	if s := c.loadMeshNodeSettings(); s.Port != port {
		s.Port = port
		if err := c.saveMeshNodeSettings(s); err != nil {
			c.log.Errorf("Error saving mesh node port: %v", err)
		}
	}
	// Check reachability once the node is up and the mesh client is
	// connected (it carries the probe).
	go func() {
		for i := 0; i < 60; i++ {
			select {
			case <-time.After(2 * time.Second):
			case <-c.ctx.Done():
				return
			}
			st := n.Status()
			if !st.Running && !st.Starting {
				return
			}
			if st.Running && c.meshProbe() != nil {
				n.CheckReach(c.ctx, c.meshProbe())
				return
			}
		}
		n.CheckReach(c.ctx, c.meshProbe())
	}()
	return nil
}

func (c *Core) stopMeshNode() {
	c.meshNodeMtx.Lock()
	n := c.meshNode
	c.meshNode = nil
	c.meshNodeMtx.Unlock()
	if n != nil {
		n.Stop()
	}
}
