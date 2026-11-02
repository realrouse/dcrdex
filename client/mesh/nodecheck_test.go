// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

const testPeerID = "12D3KooWBN56x8EFKUB7Nxz9ydxJm6ubGAEkEjhxq6bHdGVGhLrb"

func TestCheckBootstrapAddr(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	ctx := context.Background()
	ok := CheckBootstrapAddr(ctx, fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", port, testPeerID), 2*time.Second)
	if !ok.OK || ok.ID != testPeerID {
		t.Fatalf("open port: %+v", ok)
	}
	// A closed port.
	l2, _ := net.Listen("tcp4", "127.0.0.1:0")
	closed := l2.Addr().(*net.TCPAddr).Port
	l2.Close()
	if r := CheckBootstrapAddr(ctx, fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", closed, testPeerID), 2*time.Second); r.OK || r.Err == "" {
		t.Fatalf("closed port: %+v", r)
	}
	if r := CheckBootstrapAddr(ctx, fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port), time.Second); r.OK {
		t.Fatalf("no peer ID accepted: %+v", r)
	}
	if r := CheckBootstrapAddr(ctx, "junk", time.Second); r.OK {
		t.Fatalf("junk accepted")
	}
	if err := PortFree(port); err == nil {
		t.Fatalf("port in use reported free")
	}
	if err := PortFree(closed); err != nil {
		t.Fatalf("free port: %v", err)
	}
	if err := PortFree(70000); err == nil {
		t.Fatalf("bad port accepted")
	}
}
