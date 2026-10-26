// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"crypto/rand"
	"fmt"
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
