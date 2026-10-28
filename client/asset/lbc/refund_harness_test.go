//go:build harness

package lbc

// TestLockTimeRefund reproduces the silent-counterparty refund path that core
// uses (core/trade.go: ContractLockTimeExpired gates Refund) against a running
// LBC regtest harness. A swap is sent from wallet "beta" (RPC) or a fresh
// Native SPV wallet to a gamma address with a short lock time. Gamma never
// redeems. Blocks are mined every few seconds and ContractLockTimeExpired is
// polled. If it reports expired, Refund is broadcast and confirmed.
//
// Env:
//	MT_WALLET   rpc | spv (default rpc)
//	MT_HARNESS  harness root (default ~/dextest/lbc)
//	MT_STUCK    seconds after the node's own median time passes the lock time
//	            before declaring the refund stuck (default 120)
//
// MT_WALLET=spv uses a fresh Native SPV wallet, which always connects to the
// harness alpha node at 127.0.0.1:39246 (the built-in regtest peer).
//
//	go test -v -count=1 -tags=harness -run 'TestLockTimeRefund|TestNormalSwapRedeem'

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/config"
	dexlbc "decred.org/dcrdex/dex/networks/lbc"
)

func mtHarness() string {
	if d := os.Getenv("MT_HARNESS"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "dextest", "lbc")
}

func mtCtl(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = filepath.Join(mtHarness(), "harness-ctl")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func mtNodeMedianTime(t *testing.T) (int64, int64) {
	var res struct {
		Blocks     int64 `json:"blocks"`
		MedianTime int64 `json:"mediantime"`
	}
	if err := json.Unmarshal([]byte(mtCtl(t, "./alpha-node", "getblockchaininfo")), &res); err != nil {
		t.Fatal(err)
	}
	return res.Blocks, res.MedianTime
}

func mtMine(t *testing.T) { mtCtl(t, "./mine-alpha", "1") }

func mtWallet(ctx context.Context, t *testing.T, node, walletType, dir string) (asset.Wallet, *dex.ConnectionMaster) {
	t.Helper()
	settings := map[string]string{}
	if walletType == walletTypeRPC {
		var err error
		settings, err = config.Parse(filepath.Join(mtHarness(), node, node+".conf"))
		if err != nil {
			t.Fatal(err)
		}
		settings["nontls"] = "1"
	}
	logger := dex.StdOutLogger("MT."+node, dex.LevelInfo)
	if walletType == walletTypeSPV {
		seed := make([]byte, 32)
		rand.Read(seed)
		if err := (&Driver{}).Create(&asset.CreateWalletParams{
			Type: walletTypeSPV, Seed: seed, Pass: []byte("abc"), Settings: settings,
			DataDir: dir, Net: dex.Regtest, Logger: logger,
		}); err != nil {
			t.Fatalf("create spv: %v", err)
		}
	}
	w, err := NewWallet(&asset.WalletConfig{
		Type: walletType, Settings: settings, DataDir: dir,
		Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 128), BipID, logger),
		PeersChange: func(uint32, error) {},
	}, logger, dex.Regtest)
	if err != nil {
		t.Fatalf("NewWallet %s: %v", node, err)
	}
	cm := dex.NewConnectionMaster(w)
	if err := cm.ConnectOnce(ctx); err != nil {
		t.Fatalf("connect %s: %v", node, err)
	}
	if err := w.(asset.Authenticator).Unlock([]byte("abc")); err != nil {
		t.Fatalf("unlock %s: %v", node, err)
	}
	return w, cm
}

func TestLockTimeRefund(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	walletType := walletTypeRPC
	if os.Getenv("MT_WALLET") == "spv" {
		walletType = walletTypeSPV
	}
	stuckAfter := 120 * time.Second
	if s, _ := strconv.Atoi(os.Getenv("MT_STUCK")); s > 0 {
		stuckAfter = time.Duration(s) * time.Second
	}
	tmp := t.TempDir()

	// Silent counterparty: gamma only provides an address and never redeems.
	gamma, gcm := mtWallet(ctx, t, "gamma", walletTypeRPC, filepath.Join(tmp, "gamma"))
	defer gcm.Disconnect()
	recipient, err := gamma.DepositAddress()
	if err != nil {
		t.Fatal(err)
	}

	var swapper asset.Wallet
	var scm *dex.ConnectionMaster
	if walletType == walletTypeRPC {
		swapper, scm = mtWallet(ctx, t, "beta", walletTypeRPC, filepath.Join(tmp, "beta"))
	} else {
		swapper, scm = mtWallet(ctx, t, "spv", walletTypeSPV, filepath.Join(tmp, "spv"))
		// Fund the SPV wallet from alpha.
		waitSynced := func() {
			for i := 0; i < 120; i++ {
				ss, err := swapper.SyncStatus()
				if err == nil && ss.Synced {
					return
				}
				time.Sleep(time.Second)
			}
			t.Fatal("spv wallet did not sync")
		}
		waitSynced()
		addr, err := swapper.DepositAddress()
		if err != nil {
			t.Fatal(err)
		}
		mtCtl(t, "./alpha", "walletpassphrase", "abc", "600")
		t.Logf("funding spv %s: %s", addr, mtCtl(t, "./alpha", "sendtoaddress", addr, "20"))
		time.Sleep(3 * time.Second)
		mtMine(t)
		for i := 0; ; i++ {
			bal, err := swapper.Balance()
			if err == nil && bal.Available > 0 {
				t.Logf("spv balance %d", bal.Available)
				break
			}
			if i > 60 {
				t.Fatalf("spv wallet not funded: %v %v", bal, err)
			}
			time.Sleep(time.Second)
		}
	}
	defer scm.Disconnect()

	const lotSize = 3e8 // 3 LBC: picks a legacy P2PKH coin (lbcwallet signrawtransaction cannot sign P2WPKH change)
	utxos, _, _, err := swapper.FundOrder(&asset.Order{
		Value: lotSize, MaxSwapCount: 1, MaxFeeRate: 100, FeeSuggestion: dexlbc.DefaultFee,
	})
	if err != nil {
		t.Fatalf("FundOrder: %v", err)
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	secretHash := sha256.Sum256(secret)
	lockTime := time.Now().Add(30 * time.Second).Truncate(time.Second)
	receipts, _, _, err := swapper.Swap(ctx, &asset.Swaps{
		Inputs: utxos,
		Contracts: []*asset.Contract{{
			Address: recipient, Value: lotSize, SecretHash: secretHash[:], LockTime: uint64(lockTime.Unix()),
		}},
		FeeRate: dexlbc.DefaultFee,
	})
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	r := receipts[0]
	t.Logf("RESULT swap tx/coin %s, contract locktime %s (%d)", r.Coin(), lockTime.UTC().Format(time.RFC3339), lockTime.Unix())
	time.Sleep(2 * time.Second)
	mtMine(t)

	// Poll exactly like core does before refunding.
	var nodePassed time.Time
	for {
		time.Sleep(4 * time.Second)
		mtMine(t)
		time.Sleep(2 * time.Second)
		expired, expiry, err := swapper.ContractLockTimeExpired(ctx, r.Contract())
		h, nodeMTP := mtNodeMedianTime(t)
		t.Logf("height %d node mediantime %d (%s) locktime %d: ContractLockTimeExpired=%v err=%v expiry=%s",
			h, nodeMTP, time.Unix(nodeMTP, 0).UTC().Format(time.RFC3339), lockTime.Unix(), expired, err, expiry.UTC().Format(time.RFC3339))
		if expired {
			break
		}
		if nodeMTP > lockTime.Unix() && nodePassed.IsZero() {
			nodePassed = time.Now()
		}
		if !nodePassed.IsZero() && time.Since(nodePassed) > stuckAfter {
			t.Logf("RESULT STUCK: node median time passed the lock time %v ago, but the wallet still says not expired. core would never refund.", time.Since(nodePassed).Round(time.Second))
			// Show the contract is refundable by consensus: only the gate blocks it.
			coinID, err := swapper.Refund(ctx, r.Coin().ID(), r.Contract(), dexlbc.DefaultFee)
			if err != nil {
				t.Logf("RESULT forced Refund (bypassing the gate) error: %v", err)
			} else {
				c, _ := asset.DecodeCoinID(BipID, coinID)
				t.Logf("RESULT forced Refund (bypassing the gate, not what core does) accepted: %s", c)
				time.Sleep(3 * time.Second)
				mtMine(t)
			}
			t.Fatalf("refund gate never opened (STUCK)")
		}
	}
	waited := time.Since(lockTime).Round(time.Second)
	coinID, err := swapper.Refund(ctx, r.Coin().ID(), r.Contract(), dexlbc.DefaultFee)
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	c, _ := asset.DecodeCoinID(BipID, coinID)
	t.Logf("RESULT gate opened %v after lock time; refund tx %s", waited, c)
	txid := strings.Split(c, ":")[0]
	var tx struct {
		Confirmations int64 `json:"confirmations"`
		Vin           []struct {
			Txid string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
	}
	for i := 0; i < 10 && tx.Confirmations < 1; i++ {
		time.Sleep(3 * time.Second) // let the refund relay to the mining node
		mtMine(t)
		cmd := exec.Command("./alpha-node", "getrawtransaction", txid, "1")
		cmd.Dir = filepath.Join(mtHarness(), "harness-ctl")
		if out, err := cmd.Output(); err == nil {
			json.Unmarshal(out, &tx)
		}
	}
	if tx.Confirmations < 1 || len(tx.Vin) != 1 || !strings.HasPrefix(r.Coin().String(), tx.Vin[0].Txid) {
		t.Fatalf("refund tx not confirmed or wrong input: %+v", tx)
	}
	t.Logf("RESULT REFUNDED: tx %s confirmed (%d conf), spends %s:%d", txid, tx.Confirmations, tx.Vin[0].Txid, tx.Vin[0].Vout)
}

// TestNormalSwapRedeem checks that a normal swap still completes: the swapper
// (beta RPC or Native SPV) sends a contract with a 1h lock time, the gate stays
// closed, gamma redeems with the secret, and the swapper finds the redemption
// and the secret (what core does to finish its side).
func TestNormalSwapRedeem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	walletType := walletTypeRPC
	if os.Getenv("MT_WALLET") == "spv" {
		walletType = walletTypeSPV
	}
	tmp := t.TempDir()
	gamma, gcm := mtWallet(ctx, t, "gamma", walletTypeRPC, filepath.Join(tmp, "gamma"))
	defer gcm.Disconnect()
	recipient, err := gamma.DepositAddress()
	if err != nil {
		t.Fatal(err)
	}
	var swapper asset.Wallet
	var scm *dex.ConnectionMaster
	if walletType == walletTypeRPC {
		swapper, scm = mtWallet(ctx, t, "beta", walletTypeRPC, filepath.Join(tmp, "beta"))
	} else {
		swapper, scm = mtWallet(ctx, t, "spv", walletTypeSPV, filepath.Join(tmp, "spv"))
		for i := 0; ; i++ {
			ss, err := swapper.SyncStatus()
			if err == nil && ss.Synced {
				break
			}
			if i > 120 {
				t.Fatal("spv wallet did not sync")
			}
			time.Sleep(time.Second)
		}
		addr, _ := swapper.DepositAddress()
		mtCtl(t, "./alpha", "walletpassphrase", "abc", "600")
		t.Logf("funding spv %s: %s", addr, mtCtl(t, "./alpha", "sendtoaddress", addr, "20"))
		time.Sleep(3 * time.Second)
		mtMine(t)
		for i := 0; ; i++ {
			bal, err := swapper.Balance()
			if err == nil && bal.Available > 0 {
				break
			}
			if i > 60 {
				t.Fatalf("spv wallet not funded")
			}
			time.Sleep(time.Second)
		}
	}
	defer scm.Disconnect()

	const lotSize = 3e8
	utxos, _, _, err := swapper.FundOrder(&asset.Order{
		Value: lotSize, MaxSwapCount: 1, MaxFeeRate: 100, FeeSuggestion: dexlbc.DefaultFee,
	})
	if err != nil {
		t.Fatalf("FundOrder: %v", err)
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	secretHash := sha256.Sum256(secret)
	lockTime := time.Now().Add(time.Hour).Truncate(time.Second)
	receipts, _, _, err := swapper.Swap(ctx, &asset.Swaps{
		Inputs: utxos,
		Contracts: []*asset.Contract{{
			Address: recipient, Value: lotSize, SecretHash: secretHash[:], LockTime: uint64(lockTime.Unix()),
		}},
		FeeRate: dexlbc.DefaultFee,
	})
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	r := receipts[0]
	t.Logf("RESULT swap %s locktime %s", r.Coin(), lockTime.UTC().Format(time.RFC3339))
	time.Sleep(3 * time.Second)
	mtMine(t)
	time.Sleep(3 * time.Second)

	expired, _, err := swapper.ContractLockTimeExpired(ctx, r.Contract())
	if err != nil || expired {
		t.Fatalf("lock time gate must stay closed before the lock time: expired=%v err=%v", expired, err)
	}
	t.Logf("RESULT ContractLockTimeExpired before lock time = %v (correct)", expired)

	// The swap may need a few seconds (lbcd inventory trickle) to reach
	// gamma's node, so retry the audit, mining in between.
	var audit *asset.AuditInfo
	for i := 0; ; i++ {
		audit, err = gamma.AuditContract(r.Coin().ID(), r.Contract(), nil, false)
		if err == nil {
			break
		}
		if i == 20 {
			t.Fatalf("AuditContract: %v", err)
		}
		time.Sleep(3 * time.Second)
		mtMine(t)
	}
	_, redeemCoin, _, err := gamma.Redeem(ctx, &asset.RedeemForm{
		Redemptions:   []*asset.Redemption{{Spends: audit, Secret: secret}},
		FeeSuggestion: dexlbc.DefaultFee,
	})
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	t.Logf("RESULT gamma redeemed with %s", redeemCoin)
	time.Sleep(3 * time.Second)
	mtMine(t)

	fctx, fcancel := context.WithTimeout(ctx, 90*time.Second)
	defer fcancel()
	// lbcd relays with an inventory trickle, so the redeem may reach the
	// miner (alpha) only after the block above. A Native SPV wallet can't
	// search the mempool and FindRedemption blocks until a block with the
	// redeem arrives, so keep mining while it searches.
	go func() {
		for {
			select {
			case <-fctx.Done():
				return
			case <-time.After(3 * time.Second):
				cmd := exec.Command("./mine-alpha", "1")
				cmd.Dir = filepath.Join(mtHarness(), "harness-ctl")
				_ = cmd.Run() // no t.Fatal off the test goroutine
			}
		}
	}()
	var found dex.Bytes
	var foundSecret dex.Bytes
	for found == nil {
		found, foundSecret, err = swapper.FindRedemption(fctx, r.Coin().ID(), r.Contract())
		if err != nil {
			if fctx.Err() != nil {
				t.Fatalf("FindRedemption: %v", err)
			}
			mtMine(t)
			time.Sleep(3 * time.Second)
		}
	}
	if !bytes.Equal(foundSecret, secret) {
		t.Fatalf("wrong secret found")
	}
	fc, _ := asset.DecodeCoinID(BipID, found)
	t.Logf("RESULT REDEEMED: swapper found redemption %s with the right secret", fc)
}
