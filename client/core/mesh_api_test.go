package core

import (
	"testing"

	meshpkg "decred.org/dcrdex/client/mesh"
)

func TestMeshBootstrap(t *testing.T) {
	const a1 = "/ip4/127.0.0.1/tcp/15701/p2p/12D3KooWGTFZhKFBEptGGN8U5LzT4YhtfJqAwQJoCYX971i39bnV"
	const a2 = "/ip4/203.0.113.7/tcp/15701/p2p/12D3KooWCq2jdiDQxQZ1eJYJHqPYvuPTzEMUmwfxoaS86LiW7bPK"
	c := &Core{cfg: &Config{}}

	t.Setenv("TATANKA_SIMNET_ADDR", "")
	if _, err := c.meshBootstrap(); err == nil {
		t.Fatal("no error without any bootstrap address")
	}

	t.Setenv("TATANKA_SIMNET_ADDR", a1+", "+a2)
	got, err := c.meshBootstrap()
	if err != nil || len(got) != 2 || got[0] != a1 || got[1] != a2 {
		t.Fatalf("env list: %v %v", got, err)
	}

	// The config flag wins over the environment.
	c.cfg.MeshBootstrap = []string{a2}
	got, err = c.meshBootstrap()
	if err != nil || len(got) != 1 || got[0] != a2 {
		t.Fatalf("config list: %v %v", got, err)
	}

	c.cfg.MeshBootstrap = []string{"/ip4/127.0.0.1/tcp/1"}
	if _, err := c.meshBootstrap(); err == nil {
		t.Fatal("address without /p2p accepted")
	}
}

func TestMeshDisabled(t *testing.T) {
	c := &Core{cfg: &Config{AppVersion: "1.0"}}
	st := c.MeshStatus()
	if st.Enabled || st.ProtocolVersion != meshpkg.ProtocolVersion || st.AppVersion != "1.0" {
		t.Fatalf("bad disabled status %+v", st)
	}
	if _, err := c.MeshBook("dcr_btc"); err != ErrMeshDisabled {
		t.Fatalf("book: %v", err)
	}
	if _, err := c.MeshPostOrder(&MeshOrderForm{Market: "dcr_btc", Lots: 1, Rate: 1}); err != ErrMeshDisabled {
		t.Fatalf("order: %v", err)
	}
	if _, err := c.MeshTakeOrder("dcr_btc", "00", 1); err != ErrMeshDisabled {
		t.Fatalf("take: %v", err)
	}
	if err := c.MeshCancelOrder("00"); err != ErrMeshDisabled {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := c.MeshActivity(); err != ErrMeshDisabled {
		t.Fatalf("activity: %v", err)
	}
}
