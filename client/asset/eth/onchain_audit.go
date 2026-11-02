// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"fmt"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
	dexeth "decred.org/dcrdex/dex/networks/eth"
	"github.com/ethereum/go-ethereum/common"
)

// AuditContractOnChain audits a counterparty's swap contract from chain data
// only. It is for peer-to-peer (Tatanka mesh) swaps, where no server has
// checked the counterparty's initiation transaction, so the counterparty's
// tx bytes cannot be trusted. The value, recipient, secret hash and lock time
// are read from the swap contract's on-chain state for the contract's locator
// (for version 1 the locator commits to all of them). It returns
// asset.CoinNotFoundError while the swap is not initiated on chain.
func (w *assetWallet) AuditContractOnChain(ctx context.Context, coinID, contract dex.Bytes) (*asset.AuditInfo, error) {
	ver, locator, err := dexeth.DecodeContractData(contract)
	if err != nil {
		return nil, fmt.Errorf("AuditContractOnChain: failed to decode contract data: %w", err)
	}
	if len(coinID) != common.HashLength {
		return nil, fmt.Errorf("AuditContractOnChain: coin ID is %d bytes, want %d", len(coinID), common.HashLength)
	}
	ctx, cancel := context.WithTimeout(ctx, onChainDataFetchTimeout)
	defer cancel()
	status, vec, err := w.statusAndVector(ctx, locator, ver)
	if err != nil {
		return nil, fmt.Errorf("AuditContractOnChain: error reading swap state: %w", err)
	}
	if status.Step == dexeth.SSNone {
		return nil, asset.CoinNotFoundError
	}
	var txHash common.Hash
	copy(txHash[:], coinID)
	return &asset.AuditInfo{
		Recipient:  vec.To.String(),
		Expiration: time.Unix(int64(vec.LockTime), 0),
		Coin:       &coin{txHash: txHash, value: w.atomize(vec.Value)},
		Contract:   contract,
		SecretHash: vec.SecretHash[:],
	}, nil
}
