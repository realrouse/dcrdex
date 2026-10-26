// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/libp2p/go-libp2p/core/peer"
)

// SIMNET DEBUG ONLY (P6 test campaign). A tamper mode makes this client
// behave like a malicious counterparty that builds a wrong swap contract
// while announcing the agreed terms, so the honest side's audit can be tested
// against real on-chain contracts. Only reachable through the debug API.
const (
	TamperNone       = ""
	TamperValue      = "value"      // lock half the agreed amount
	TamperSecretHash = "secrethash" // contract commits to a different secret hash
	TamperLockTime   = "locktime"   // lock 60s shorter than agreed
	TamperRecipient  = "recipient"  // pay our own address instead of the peer's
)

// SetSwapTamper sets the debug tamper mode for contracts created from now on.
func (ob *OrderBooks) SetSwapTamper(mode string) error {
	switch mode {
	case TamperNone, TamperValue, TamperSecretHash, TamperLockTime, TamperRecipient:
	default:
		return fmt.Errorf("unknown tamper mode %q", mode)
	}
	sw := ob.getSwapper()
	if sw == nil {
		return fmt.Errorf("swaps not enabled")
	}
	sw.tamper.Store(mode)
	return nil
}

// tamperContract applies the debug tamper mode to a contract we are about to
// create for asset sendAsset.
func (ob *OrderBooks) tamperContract(sw *swapper, sc *SwapContract, sendAsset uint32) {
	mode, _ := sw.tamper.Load().(string)
	switch mode {
	case TamperValue:
		sc.Value /= 2
	case TamperSecretHash:
		h := make([]byte, 32)
		_, _ = rand.Read(h)
		sc.SecretHash = h
	case TamperLockTime:
		sc.LockTime -= 60
	case TamperRecipient:
		if a := ob.receiveAddr(sendAsset); a != "" {
			sc.Address = a
		}
	default:
		return
	}
	ob.log.Warnf("DEBUG TAMPER %q: creating a wrong contract on purpose", mode)
}

// advCapture holds the last sealed inbox message sent to each peer, only
// when enabled by the debug server, so floods and replays can be injected.
type advCapture struct {
	mtx  sync.Mutex
	on   bool
	last map[peer.ID][]byte
}

func (c *advCapture) record(to peer.ID, b []byte) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if !c.on {
		return
	}
	if c.last == nil {
		c.last = make(map[peer.ID][]byte)
	}
	if len(c.last) < 1000 {
		c.last[to] = b
	} else if _, ok := c.last[to]; ok {
		c.last[to] = b
	}
}

// EnableAdversary turns on capture of sent inbox messages (debug only).
func (ob *OrderBooks) EnableAdversary() {
	ob.adv.mtx.Lock()
	ob.adv.on = true
	ob.adv.mtx.Unlock()
}

// FloodResult reports what Flood published.
type FloodResult struct {
	Sent   int    `json:"sent"`
	Failed int    `json:"failed"`
	Err    string `json:"lastErr,omitempty"`
}

// Flood publishes n messages to peer to's inbox: "replay" re-sends the last
// real message we sent them, "junk" random envelopes of size bytes, as fast
// as possible. Debug only.
func (ob *OrderBooks) Flood(ctx context.Context, to peer.ID, mode string, n, size int) (*FloodResult, error) {
	var msg []byte
	switch mode {
	case "replay":
		ob.adv.mtx.Lock()
		msg = ob.adv.last[to]
		ob.adv.mtx.Unlock()
		if msg == nil {
			return nil, fmt.Errorf("nothing sent to %s yet", to)
		}
	case "junk":
		if size <= 0 {
			size = 512
		}
	default:
		return nil, fmt.Errorf("unknown flood mode %q", mode)
	}
	res := new(FloodResult)
	for i := 0; i < n; i++ {
		b := msg
		if mode == "junk" {
			b = make([]byte, size)
			_, _ = rand.Read(b)
			b[0] = inboxVersion
		}
		if err := ob.ps.Broadcast(ctx, InboxTopic(to), b); err != nil {
			res.Failed++
			res.Err = err.Error()
			continue
		}
		res.Sent++
	}
	ob.log.Warnf("DEBUG FLOOD %s x%d to %s: %+v", mode, n, to, res)
	return res, nil
}
