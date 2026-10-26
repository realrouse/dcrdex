// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import (
	"context"
	"errors"
	"time"

	"decred.org/dcrdex/client/asset"
	meshpkg "decred.org/dcrdex/client/mesh"
)

// meshSwapWallet adapts a core wallet to the mesh swap engine
// (meshpkg.SwapWallet). SIMNET-ONLY for now (P3).
type meshSwapWallet struct {
	c       *Core
	assetID uint32
}

var _ meshpkg.SwapWallet = (*meshSwapWallet)(nil)

// Fallback fee rates when the wallet has no estimate (regtest/simnet).
const meshFallbackFeeRate = 10

func (w *meshSwapWallet) wallet() (*xcWallet, error) {
	xw, err := w.c.connectedWallet(w.assetID)
	if err != nil {
		return nil, err
	}
	if !xw.unlocked() {
		return nil, errors.New("wallet locked")
	}
	return xw, nil
}

func (w *meshSwapWallet) feeRate() uint64 {
	if r := w.c.feeSuggestionAny(w.assetID); r > 0 {
		return r
	}
	return meshFallbackFeeRate
}

func (w *meshSwapWallet) coin(coinID []byte) *meshpkg.SwapCoin {
	txid, err := asset.DecodeCoinID(w.assetID, coinID)
	if err != nil {
		txid = ""
	}
	return &meshpkg.SwapCoin{CoinID: coinID, TxID: txid}
}

func (w *meshSwapWallet) Initiate(ctx context.Context, sc *meshpkg.SwapContract) (*meshpkg.SwapCoin, error) {
	xw, err := w.wallet()
	if err != nil {
		return nil, err
	}
	feeRate := w.feeRate()
	coins, _, _, err := xw.FundOrder(&asset.Order{
		Value:         sc.Value,
		MaxSwapCount:  1,
		MaxFeeRate:    feeRate,
		FeeSuggestion: feeRate,
	})
	if err != nil {
		return nil, err
	}
	receipts, _, fees, err := xw.Swap(ctx, &asset.Swaps{
		Inputs: coins,
		Contracts: []*asset.Contract{{
			Address:    sc.Address,
			Value:      sc.Value,
			SecretHash: sc.SecretHash,
			LockTime:   uint64(sc.LockTime),
		}},
		FeeRate: feeRate,
	})
	if err != nil {
		if rerr := xw.ReturnCoins(coins); rerr != nil {
			w.c.log.Warnf("Error returning coins after failed mesh swap: %v", rerr)
		}
		return nil, err
	}
	if len(receipts) != 1 {
		return nil, errors.New("expected 1 swap receipt")
	}
	rc := receipts[0]
	w.c.log.Infof("Mesh swap: sent %d %s atoms in contract %s (fees %d)", sc.Value, xw.Symbol, rc.Coin(), fees)
	sw := w.coin(rc.Coin().ID())
	sw.Contract = rc.Contract()
	return sw, nil
}

func (w *meshSwapWallet) Audit(_ context.Context, coinID, contract []byte) (*meshpkg.SwapAudit, error) {
	xw, err := w.wallet()
	if err != nil {
		return nil, err
	}
	ai, err := xw.AuditContract(coinID, contract, nil, false)
	if err != nil {
		return nil, err
	}
	return &meshpkg.SwapAudit{Recipient: ai.Recipient, Value: ai.Coin.Value(), SecretHash: ai.SecretHash, Expiration: ai.Expiration.Unix()}, nil
}

func (w *meshSwapWallet) Confirmations(ctx context.Context, coinID, contract []byte) (uint32, bool, error) {
	xw, err := w.wallet()
	if err != nil {
		return 0, false, err
	}
	return xw.SwapConfirmations(ctx, coinID, contract, time.Now().Add(-6*time.Hour))
}

func (w *meshSwapWallet) Redeem(ctx context.Context, coinID, contract, secret []byte) (*meshpkg.SwapCoin, error) {
	xw, err := w.wallet()
	if err != nil {
		return nil, err
	}
	ai, err := xw.AuditContract(coinID, contract, nil, false)
	if err != nil {
		return nil, err
	}
	_, out, fees, err := xw.Redeem(ctx, &asset.RedeemForm{
		Redemptions:   []*asset.Redemption{{Spends: ai, Secret: secret}},
		FeeSuggestion: w.feeRate(),
	})
	if err != nil {
		return nil, err
	}
	w.c.log.Infof("Mesh swap: redeemed %s contract %s -> %s (fees %d)", xw.Symbol, ai.Coin, out, fees)
	return w.coin(out.ID()), nil
}

func (w *meshSwapWallet) FindRedemption(ctx context.Context, coinID, contract []byte) (*meshpkg.SwapCoin, []byte, error) {
	xw, err := w.wallet()
	if err != nil {
		return nil, nil, err
	}
	redeemCoin, secret, err := xw.FindRedemption(ctx, coinID, contract)
	if err != nil {
		return nil, nil, err
	}
	return w.coin(redeemCoin), secret, nil
}

func (w *meshSwapWallet) Refund(ctx context.Context, coinID, contract []byte) (*meshpkg.SwapCoin, error) {
	xw, err := w.wallet()
	if err != nil {
		return nil, err
	}
	refundCoin, err := xw.Refund(ctx, coinID, contract, w.feeRate())
	if err != nil {
		return nil, err
	}
	w.c.log.Infof("Mesh swap: refunded %s contract -> %x", xw.Symbol, refundCoin)
	return w.coin(refundCoin), nil
}

func (w *meshSwapWallet) LockTimeExpired(ctx context.Context, lockTime int64) (bool, error) {
	xw, err := w.wallet()
	if err != nil {
		return false, err
	}
	return xw.LockTimeExpired(ctx, time.Unix(lockTime, 0))
}
