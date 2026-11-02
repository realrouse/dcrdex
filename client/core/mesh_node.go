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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// Name is the node's display name (random by default, never the
	// computer name). It is shown in this app and the node log; the mesh
	// knows nodes by their ID.
	Name string `json:"name,omitempty"`
	// Bootstrap overrides the mesh nodes the node peers with (empty: the
	// client's bootstrap list).
	Bootstrap []string `json:"bootstrap,omitempty"`
	// PublicIP is a manual public IPv4 (port forward or VPS). Empty: from
	// the router mapping or as seen by the mesh nodes. TATANKA_MESH_NODE_IP
	// overrides it.
	PublicIP string `json:"publicIP,omitempty"`
}

// MeshNodeDefaults are the wizard's suggested settings.
type MeshNodeDefaults struct {
	Port      int      `json:"port"`
	Name      string   `json:"name"`
	Bootstrap []string `json:"bootstrap"`
}

// MeshNodeInfo is the embedded node's setting and state.
type MeshNodeInfo struct {
	// Available is false where the node can't run (not simnet with --mesh).
	Available bool                `json:"available"`
	Enabled   bool                `json:"enabled"`
	Status    *meshpkg.NodeStatus `json:"status,omitempty"`
	Settings  *MeshNodeSettings   `json:"settings,omitempty"`
	Defaults  *MeshNodeDefaults   `json:"defaults,omitempty"`
}

// MeshNodePrecheck is the result of the wizard's checks before the node
// starts: is the port free, and do the bootstrap mesh nodes answer.
type MeshNodePrecheck struct {
	Port      int                       `json:"port"`
	PortOK    bool                      `json:"portOK"`
	PortErr   string                    `json:"portErr,omitempty"`
	Bootstrap []*meshpkg.BootstrapCheck `json:"bootstrap"`
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
	set := c.loadMeshNodeSettings()
	info.Enabled = set.Enabled
	info.Settings = set
	info.Defaults = c.meshNodeDefaults(set)
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

func (c *Core) meshNodeDefaults(set *MeshNodeSettings) *MeshNodeDefaults {
	d := &MeshNodeDefaults{Port: meshpkg.DefaultNodePort, Name: set.Name}
	if set.Port > 0 {
		d.Port = set.Port
	}
	if d.Name == "" {
		c.meshNodeMtx.Lock()
		if c.meshNodeSuggestedName == "" {
			c.meshNodeSuggestedName = meshpkg.RandomName()
		}
		d.Name = c.meshNodeSuggestedName
		c.meshNodeMtx.Unlock()
	}
	d.Bootstrap, _ = c.meshBootstrap()
	return d
}

// MeshNodeConfigForm is the wizard's settings step.
type MeshNodeConfigForm struct {
	Port      int      `json:"port"`
	Name      string   `json:"name"`
	Bootstrap []string `json:"bootstrap"`
	PublicIP  string   `json:"publicIP"`
}

func (c *Core) validMeshNodeForm(f *MeshNodeConfigForm) (*MeshNodeConfigForm, error) {
	out := &MeshNodeConfigForm{Port: f.Port, Name: meshpkg.SanitizeName(f.Name), PublicIP: strings.TrimSpace(f.PublicIP)}
	if out.Port == 0 {
		out.Port = meshpkg.DefaultNodePort
	}
	if out.Port < 1024 || out.Port > 65535 {
		return nil, fmt.Errorf("port %d: use a port from 1024 to 65535", out.Port)
	}
	for _, b := range f.Bootstrap {
		l, err := meshpkg.ParseBootstrapList(b)
		if err != nil {
			return nil, err
		}
		out.Bootstrap = append(out.Bootstrap, l...)
	}
	if out.PublicIP != "" {
		if ip := net.ParseIP(out.PublicIP); ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("public IP %q is not an IPv4 address", out.PublicIP)
		}
	}
	return out, nil
}

// MeshNodePrecheck tests the wizard settings before the node starts: is
// the port free (or already used by our running node), and do the
// bootstrap mesh nodes accept TCP connections.
func (c *Core) MeshNodePrecheck(f *MeshNodeConfigForm) (*MeshNodePrecheck, error) {
	if !c.meshNodeAvailable() {
		return nil, ErrMeshNodeUnavailable
	}
	f, err := c.validMeshNodeForm(f)
	if err != nil {
		return nil, err
	}
	res := &MeshNodePrecheck{Port: f.Port}
	c.meshNodeMtx.Lock()
	n := c.meshNode
	c.meshNodeMtx.Unlock()
	if n != nil && n.Status().Port == f.Port {
		res.PortOK = true // our own node listens there
	} else if err := meshpkg.PortFree(f.Port); err != nil {
		res.PortErr = fmt.Sprintf("port %d is in use by another program on this computer", f.Port)
	} else {
		res.PortOK = true
	}
	boot := f.Bootstrap
	if len(boot) == 0 {
		boot, _ = c.meshBootstrap()
	}
	if len(boot) == 0 {
		return nil, errors.New("no bootstrap mesh nodes")
	}
	res.Bootstrap = make([]*meshpkg.BootstrapCheck, len(boot))
	var wg sync.WaitGroup
	for i, b := range boot {
		wg.Add(1)
		go func(i int, b string) {
			defer wg.Done()
			res.Bootstrap[i] = meshpkg.CheckBootstrapAddr(c.ctx, b, 6*time.Second)
		}(i, b)
	}
	wg.Wait()
	return res, nil
}

// ConfigureMeshNode saves the wizard settings, switches the node on and
// (re)starts it with them.
func (c *Core) ConfigureMeshNode(f *MeshNodeConfigForm) (*MeshNodeInfo, error) {
	if !c.meshNodeAvailable() {
		return nil, ErrMeshNodeUnavailable
	}
	f, err := c.validMeshNodeForm(f)
	if err != nil {
		return nil, err
	}
	s := c.loadMeshNodeSettings()
	changed := s.Port != f.Port || s.PublicIP != f.PublicIP || strings.Join(s.Bootstrap, ",") != strings.Join(f.Bootstrap, ",")
	s.Enabled, s.Port, s.PublicIP, s.Bootstrap = true, f.Port, f.PublicIP, f.Bootstrap
	if f.Name != "" {
		s.Name = f.Name
	} else if s.Name == "" {
		s.Name = c.meshNodeDefaults(s).Name
	}
	if err := c.saveMeshNodeSettings(s); err != nil {
		return nil, err
	}
	c.meshNodeMtx.Lock()
	n := c.meshNode
	c.meshNodeMtx.Unlock()
	if n != nil {
		st := n.Status()
		if changed || st.Error != "" || (!st.Running && !st.Starting) {
			c.stopMeshNode()
		}
	}
	if err := c.startMeshNode(); err != nil {
		return nil, err
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
	set := c.loadMeshNodeSettings()
	bootstrap := set.Bootstrap
	if len(bootstrap) == 0 {
		var err error
		if bootstrap, err = c.meshBootstrap(); err != nil {
			return err
		}
	}
	pubIP := set.PublicIP
	if ip := strings.TrimSpace(os.Getenv("TATANKA_MESH_NODE_IP")); ip != "" {
		pubIP = ip
	}
	if set.Name != "" {
		c.log.Infof("Starting mesh node %q", set.Name)
	}
	cfg := &meshpkg.NodeConfig{
		DataDir:   c.meshNodeDir(),
		Port:      set.Port,
		Bootstrap: bootstrap,
		// Manual public IP (port forward or VPS; env TATANKA_MESH_NODE_IP
		// overrides the setting). Test overrides (simnet): extra
		// whitelisted nodes, and no router port mapping.
		PublicIP:  pubIP,
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
