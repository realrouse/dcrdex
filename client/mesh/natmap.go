// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
	natpmp "github.com/jackpal/go-nat-pmp"
)

const (
	portMapLifetime = time.Hour
	portMapDesc     = "bisonw mesh node"
)

// portMapping is a router port mapping made with UPnP IGD or NAT-PMP.
type portMapping struct {
	Method     string // "upnp" or "nat-pmp"
	ExternalIP net.IP
	Port       int
	renew      func() error
	remove     func()
}

// upnpClient is what the UPnP WANIPConnection1/2 and WANPPPConnection1
// clients have in common.
type upnpClient interface {
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	AddPortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, proto string, intPort uint16,
		intClient string, enabled bool, desc string, lease uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, proto string) error
	LocalAddr() net.IP
}

func upnpClients(ctx context.Context) []upnpClient {
	var out []upnpClient
	if cs, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx); err == nil {
		for _, c := range cs {
			out = append(out, c)
		}
	}
	if cs, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx); err == nil {
		for _, c := range cs {
			out = append(out, c)
		}
	}
	if cs, _, err := internetgateway2.NewWANPPPConnection1ClientsCtx(ctx); err == nil {
		for _, c := range cs {
			out = append(out, c)
		}
	}
	return out
}

// mapUPnP asks UPnP routers for port -> port (TCP).
func mapUPnP(ctx context.Context, port int) (*portMapping, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	clients := upnpClients(ctx)
	if len(clients) == 0 {
		return nil, errors.New("no UPnP router found")
	}
	var lastErr error
	for _, c := range clients {
		ipStr, err := c.GetExternalIPAddressCtx(ctx)
		ip := net.ParseIP(ipStr)
		if err != nil || ip == nil {
			lastErr = fmt.Errorf("UPnP external address: %v", err)
			continue
		}
		local := c.LocalAddr()
		if local == nil {
			lastErr = errors.New("UPnP: unknown local address")
			continue
		}
		add := func(ctx context.Context) error {
			return c.AddPortMappingCtx(ctx, "", uint16(port), "TCP", uint16(port), local.String(), true,
				portMapDesc, uint32(portMapLifetime/time.Second))
		}
		if err := add(ctx); err != nil {
			lastErr = fmt.Errorf("UPnP add port mapping: %v", err)
			continue
		}
		return &portMapping{
			Method: "upnp", ExternalIP: ip, Port: port,
			renew: func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				return add(ctx)
			},
			remove: func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = c.DeletePortMappingCtx(ctx, "", uint16(port), "TCP")
			},
		}, nil
	}
	return nil, lastErr
}

// mapNATPMP asks the default gateway for port -> port (TCP) with NAT-PMP.
func mapNATPMP(port int) (*portMapping, error) {
	gw := defaultGateway()
	if gw == nil {
		return nil, errors.New("NAT-PMP: default gateway unknown")
	}
	c := natpmp.NewClientWithTimeout(gw, 3*time.Second)
	ext, err := c.GetExternalAddress()
	if err != nil {
		return nil, fmt.Errorf("NAT-PMP: no answer from %s: %v", gw, err)
	}
	add := func() error {
		res, err := c.AddPortMapping("tcp", port, port, int(portMapLifetime/time.Second))
		if err != nil {
			return err
		}
		if int(res.MappedExternalPort) != port {
			return fmt.Errorf("router mapped external port %d, not %d", res.MappedExternalPort, port)
		}
		return nil
	}
	if err := add(); err != nil {
		return nil, fmt.Errorf("NAT-PMP add port mapping: %v", err)
	}
	ip := net.IP(ext.ExternalIPAddress[:])
	return &portMapping{
		Method: "nat-pmp", ExternalIP: ip, Port: port, renew: add,
		remove: func() { _, _ = c.AddPortMapping("tcp", port, 0, 0) },
	}, nil
}

// mapPort tries UPnP, then NAT-PMP.
func mapPort(ctx context.Context, port int) (*portMapping, error) {
	m, uerr := mapUPnP(ctx, port)
	if uerr == nil {
		return m, nil
	}
	m, perr := mapNATPMP(port)
	if perr == nil {
		return m, nil
	}
	return nil, fmt.Errorf("%v; %v", uerr, perr)
}

// defaultGateway is the IPv4 default gateway, read from the routing table on
// Linux. Elsewhere it guesses <local subnet>.1, which is right for most home
// routers.
func defaultGateway() net.IP {
	if runtime.GOOS == "linux" {
		if f, err := os.Open("/proc/net/route"); err == nil {
			defer f.Close()
			s := bufio.NewScanner(f)
			for s.Scan() {
				fs := strings.Fields(s.Text())
				if len(fs) < 3 || fs[1] != "00000000" {
					continue
				}
				b, err := hex.DecodeString(fs[2])
				if err != nil || len(b) != 4 {
					continue
				}
				ip := make(net.IP, 4)
				binary.LittleEndian.PutUint32(ip, binary.BigEndian.Uint32(b))
				return ip
			}
		}
	}
	conn, err := net.Dial("udp4", "192.0.2.1:9") // no packets are sent
	if err != nil {
		return nil
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr).IP.To4()
	if local == nil || !local.IsPrivate() {
		return nil
	}
	return net.IPv4(local[0], local[1], local[2], 1)
}
