// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

// S4 validator bonds: the epoch engine checks other validators' bonds on
// chain and makes our own with the asset wallets.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"decred.org/dcrdex/client/asset"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

type meshBonds struct {
	c   *Core
	dir string
	mtx sync.Mutex
}

// BondConfs looks the bond output up with the asset's wallet (an SPV wallet
// scans block filters from since).
func (m *meshBonds) BondConfs(ctx context.Context, assetID uint32, coinID, bondScript []byte, since time.Time) (uint32, bool, error) {
	w, err := m.c.connectedWallet(assetID)
	if err != nil {
		return 0, false, fmt.Errorf("no %s wallet to check bonds: %w", unbip(assetID), err)
	}
	return w.Wallet.SwapConfirmations(ctx, coinID, bondScript, since)
}

// validatorBondRecord is kept in <appdata>/<net>/mesh/validator-bonds.jsonl
// so the bond can be refunded after its lock (RefundTx is the wallet's
// pre-signed refund transaction).
type validatorBondRecord struct {
	AssetID  uint32 `json:"assetID"`
	CoinID   string `json:"coinID"`
	Amount   uint64 `json:"amount"`
	LockTime int64  `json:"lockTime"`
	Script   string `json:"script"`
	RefundTx string `json:"refundTx"`
	Stamp    int64  `json:"stamp"`
}

// MakeValidatorBond makes a v0 bond transaction and, if broadcast, sends it.
func (m *meshBonds) MakeValidatorBond(ctx context.Context, assetID uint32, amt uint64, lockTime time.Time, bondKey []byte, acct [32]byte, broadcast bool) ([]byte, []byte, error) {
	w, err := m.c.connectedWallet(assetID)
	if err != nil {
		return nil, nil, err
	}
	bonder, ok := w.Wallet.(asset.Bonder)
	if !ok {
		return nil, nil, fmt.Errorf("%s wallet cannot make bonds", unbip(assetID))
	}
	if len(bondKey) != 32 {
		return nil, nil, errors.New("bad bond key")
	}
	priv := secp256k1.PrivKeyFromBytes(bondKey)
	bond, abandon, err := bonder.MakeBondTx(0, amt, 0, lockTime, priv, acct[:])
	if err != nil {
		return nil, nil, fmt.Errorf("make bond tx: %w", err)
	}
	if !broadcast {
		abandon()
		return bond.CoinID, bond.SignedTx, nil
	}
	if _, err := bonder.SendTransaction(bond.SignedTx); err != nil {
		abandon()
		return nil, nil, fmt.Errorf("send bond tx: %w", err)
	}
	rec := &validatorBondRecord{AssetID: assetID, CoinID: hex.EncodeToString(bond.CoinID), Amount: amt, LockTime: lockTime.Unix(),
		Script: hex.EncodeToString(bond.Data), RefundTx: hex.EncodeToString(bond.RedeemTx), Stamp: time.Now().Unix()}
	if b, err := json.Marshal(rec); err == nil {
		m.mtx.Lock()
		if f, err := os.OpenFile(filepath.Join(m.dir, "validator-bonds.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.Write(append(b, '\n'))
			f.Close()
		}
		m.mtx.Unlock()
	}
	m.c.log.Infof("Validator bond sent: %s %d atoms, coin %x, lock until %s", unbip(assetID), amt, bond.CoinID, lockTime.Format(time.RFC3339))
	return bond.CoinID, bond.SignedTx, nil
}
