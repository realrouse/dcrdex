// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/asset/btc"
	"decred.org/dcrdex/dex"
	dexlbc "decred.org/dcrdex/dex/networks/lbc"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/gcs"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"github.com/btcsuite/btcwallet/waddrmgr"
	btcwallet "github.com/btcsuite/btcwallet/wallet"
	btcwalletdb "github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/jrick/logrotate/rotator"
	lbcbtcec "github.com/lbryio/lbcd/btcec"
	lbcchaincfg "github.com/lbryio/lbcd/chaincfg"
	lbcchainhash "github.com/lbryio/lbcd/chaincfg/chainhash"
	lbcwire "github.com/lbryio/lbcd/wire"
	lbcutil "github.com/lbryio/lbcutil"
	"github.com/lbryio/lbcwallet/chain"
	lbcwaddrmgr "github.com/lbryio/lbcwallet/waddrmgr"
	"github.com/lbryio/lbcwallet/wallet"
	"github.com/lbryio/lbcwallet/wallet/txauthor"
	"github.com/lbryio/lbcwallet/walletdb"
	_ "github.com/lbryio/lbcwallet/walletdb/bdb"
	lbcwtxmgr "github.com/lbryio/lbcwallet/wtxmgr"
	btcneutrino "github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/headerfs"
	neutrino "github.com/realrouse/lbcd-spv/neutrino"
)

const (
	// DefaultM is lbcutil/gcs/builder.DefaultM, used when translating filters.
	DefaultM                uint64 = 784931
	logDirName                     = "logs"
	neutrinoDBName                 = "neutrino.db"
	neutrinoChainFormatFile        = "neutrino.chainformat"
	// neutrinoChainFormat is bumped when Native LBC compact-filter files
	// from older experimental builds must not be reused. "2" is the
	// in-memory retarget look-back fix (lbcd-spv 73add99).
	neutrinoChainFormat = "2"
	// neutrinoStuckHeaderCount is far below a healthy first 2000-header
	// batch. Pre-fix clients disconnected at height 2 and never persisted
	// that batch, so on-disk height stayed at genesis.
	neutrinoStuckHeaderCount = 64
	lbcBlockHeaderSize       = lbcwire.MaxBlockHeaderPayload
	defaultAcctNum           = 0
	dbTimeout                = 20 * time.Second
	p2pPort                  = "9246"
	regtestP2P               = "127.0.0.1:39246"
	foundationFilterPeer     = "s1.lbry.network:9246"
)

var (
	waddrmgrNamespace = []byte("waddrmgr")
	wtxmgrNamespace   = []byte("wtxmgr")
)

// lbcSPVWallet implements btc.BTCWallet with in-process lbcwallet + Neutrino.
type lbcSPVWallet struct {
	dir         string
	chainParams *lbcchaincfg.Params
	btcParams   *chaincfg.Params
	log         dex.Logger

	*wallet.Wallet
	chainClient *NeutrinoClient
	cl          *neutrino.ChainService
	loader      *wallet.Loader
	neutrinoDB  btcwalletdb.DB

	peerManager *btc.SPVPeerManager
}

var _ btc.BTCWallet = (*lbcSPVWallet)(nil)

func openSPVWallet(dir string, cfg *btc.WalletConfig, btcParams *chaincfg.Params, log dex.Logger) btc.BTCWallet {
	var lbcParams *lbcchaincfg.Params
	switch cloneParamsName(btcParams) {
	case dexlbc.MainNetParams.Name:
		lbcParams = &lbcchaincfg.MainNetParams
	case dexlbc.TestNet3Params.Name:
		lbcParams = &lbcchaincfg.TestNet3Params
	case dexlbc.RegressionNetParams.Name:
		lbcParams = &lbcchaincfg.RegressionNetParams
	}
	return &lbcSPVWallet{
		dir:         dir,
		chainParams: lbcParams,
		btcParams:   btcParams,
		log:         log,
	}
}

func createSPVWallet(privPass []byte, seed []byte, bday time.Time, walletDir string, log dex.Logger, extIdx, intIdx uint32, net *lbcchaincfg.Params) error {
	if err := logNeutrino(walletDir, log); err != nil {
		return fmt.Errorf("error initializing lbcwallet+neutrino logging: %w", err)
	}

	loader := wallet.NewLoader(net, walletDir, true, dbTimeout, 250)
	btcw, err := loader.CreateNewWallet(privPass, seed, bday)
	if err != nil {
		return fmt.Errorf("CreateNewWallet error: %w", err)
	}

	errCloser := dex.NewErrorCloser()
	defer errCloser.Done(log)
	errCloser.Add(loader.UnloadWallet)

	if extIdx > 0 || intIdx > 0 {
		if err = extendAddresses(extIdx, intIdx, btcw); err != nil {
			return fmt.Errorf("failed to set starting address indexes: %w", err)
		}
	}

	neutrinoDBPath := filepath.Join(walletDir, neutrinoDBName)
	db, err := btcwalletdb.Create("bdb", neutrinoDBPath, true, dbTimeout)
	if err != nil {
		return fmt.Errorf("unable to create neutrino db at %q: %w", neutrinoDBPath, err)
	}
	if err = db.Close(); err != nil {
		return fmt.Errorf("error closing newly created neutrino database: %w", err)
	}

	if err := loader.UnloadWallet(); err != nil {
		return fmt.Errorf("error unloading wallet: %w", err)
	}

	errCloser.Success()
	return nil
}

func (w *lbcSPVWallet) Start() (btc.SPVService, error) {
	if err := logNeutrino(w.dir, w.log); err != nil {
		return nil, fmt.Errorf("error initializing lbcwallet+neutrino logging: %v", err)
	}
	if w.chainParams == nil {
		return nil, errors.New("unknown LBC network")
	}

	w.loader = wallet.NewLoader(w.chainParams, w.dir, true, dbTimeout, 250)

	exists, err := w.loader.WalletExists()
	if err != nil {
		return nil, fmt.Errorf("error verifying wallet existence: %v", err)
	}
	if !exists {
		return nil, errors.New("wallet not found")
	}

	w.log.Debug("Starting native LBC wallet...")
	w.Wallet, err = w.loader.OpenExistingWallet()
	if err != nil {
		return nil, fmt.Errorf("couldn't load wallet: %w", err)
	}

	errCloser := dex.NewErrorCloser()
	defer errCloser.Done(w.log)
	errCloser.Add(w.loader.UnloadWallet)

	if err := resetStaleNeutrinoChain(w.dir, w.log); err != nil {
		return nil, fmt.Errorf("error preparing neutrino chain data: %w", err)
	}

	neutrinoDBPath := filepath.Join(w.dir, neutrinoDBName)
	w.neutrinoDB, err = btcwalletdb.Create("bdb", neutrinoDBPath, true, dbTimeout)
	if err != nil {
		return nil, fmt.Errorf("unable to create neutrino db at %q: %v", neutrinoDBPath, err)
	}
	errCloser.Add(w.neutrinoDB.Close)

	w.log.Debug("Starting neutrino chain service...")
	w.cl, err = neutrino.NewChainService(neutrino.Config{
		DataDir:          w.dir,
		Database:         w.neutrinoDB,
		ChainParams:      *w.chainParams,
		PersistToDisk:    true,
		DisableDNSSeed:   true, // LBRY DNS seeds are mostly non-CF lbrycrd
		BroadcastTimeout: 6 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("couldn't create Neutrino ChainService: %w", err)
	}
	errCloser.Add(w.cl.Stop)

	w.chainClient = NewNeutrinoClient(w.chainParams, w.cl)

	var defaultPeers []string
	switch w.chainParams.Net {
	case lbcchaincfg.MainNetParams.Net:
		defaultPeers = []string{foundationFilterPeer}
	case lbcchaincfg.RegressionNetParams.Net:
		defaultPeers = []string{regtestP2P}
	}
	peerManager := btc.NewSPVPeerManager(&spvService{w.cl}, defaultPeers, w.dir, w.log, p2pPort)
	w.peerManager = peerManager

	if err = w.chainClient.Start(); err != nil {
		return nil, fmt.Errorf("couldn't start Neutrino client: %v", err)
	}

	w.log.Info("Synchronizing wallet with network...")
	w.SynchronizeRPC(w.chainClient)

	errCloser.Success()

	w.peerManager.ConnectToInitialWalletPeers()

	return &spvService{w.cl}, nil
}

func (w *lbcSPVWallet) Birthday() time.Time {
	return w.Manager.Birthday()
}

func (w *lbcSPVWallet) PublishTransaction(btcTx *wire.MsgTx, label string) error {
	lbcTx, err := convertMsgTxToLBC(btcTx)
	if err != nil {
		return err
	}
	return w.Wallet.PublishTransaction(lbcTx, label)
}

func (w *lbcSPVWallet) CalculateAccountBalances(account uint32, confirms int32) (btcwallet.Balances, error) {
	bals, err := w.Wallet.CalculateAccountBalances(account, confirms)
	if err != nil {
		return btcwallet.Balances{}, err
	}
	return btcwallet.Balances{
		Total:          btcutil.Amount(bals.Total),
		Spendable:      btcutil.Amount(bals.Spendable),
		ImmatureReward: btcutil.Amount(bals.ImmatureReward),
	}, nil
}

func (w *lbcSPVWallet) ListSinceBlock(start, end, syncHeight int32) ([]btcjson.ListTransactionsResult, error) {
	res, err := w.Wallet.ListSinceBlock(defaultAcctName, start, end, syncHeight)
	if err != nil {
		return nil, err
	}
	btcRes := make([]btcjson.ListTransactionsResult, len(res))
	for i, r := range res {
		btcRes[i] = btcjson.ListTransactionsResult{
			Abandoned:         r.Abandoned,
			Account:           r.Account,
			Address:           r.Address,
			Amount:            r.Amount,
			BIP125Replaceable: r.BIP125Replaceable,
			BlockHash:         r.BlockHash,
			BlockTime:         r.BlockTime,
			Category:          r.Category,
			Confirmations:     r.Confirmations,
			Generated:         r.Generated,
			InvolvesWatchOnly: r.InvolvesWatchOnly,
			Time:              r.Time,
			TimeReceived:      r.TimeReceived,
			Trusted:           r.Trusted,
			TxID:              r.TxID,
			Vout:              r.Vout,
			WalletConflicts:   r.WalletConflicts,
			Comment:           r.Comment,
			OtherAccount:      r.OtherAccount,
		}
	}
	return btcRes, nil
}

const defaultAcctName = "default"

func (w *lbcSPVWallet) GetTransactions(startBlock, endBlock int32, accountName string, cancel <-chan struct{}) (*btcwallet.GetTransactionsResult, error) {
	startID := wallet.NewBlockIdentifierFromHeight(startBlock)
	endID := wallet.NewBlockIdentifierFromHeight(endBlock)
	lbcGTR, err := w.Wallet.GetTransactions(startID, endID, accountName, cancel)
	if err != nil {
		return nil, err
	}

	convertTxs := func(txs []wallet.TransactionSummary) []btcwallet.TransactionSummary {
		transactions := make([]btcwallet.TransactionSummary, len(txs))
		for i, tx := range txs {
			txHash := hashToBTC(*tx.Hash)
			inputs := make([]btcwallet.TransactionSummaryInput, len(tx.MyInputs))
			for k, in := range tx.MyInputs {
				inputs[k] = btcwallet.TransactionSummaryInput{
					Index:           in.Index,
					PreviousAccount: in.PreviousAccount,
					PreviousAmount:  btcutil.Amount(in.PreviousAmount),
				}
			}
			outputs := make([]btcwallet.TransactionSummaryOutput, len(tx.MyOutputs))
			for k, out := range tx.MyOutputs {
				outputs[k] = btcwallet.TransactionSummaryOutput{
					Index:    out.Index,
					Account:  out.Account,
					Internal: out.Internal,
				}
			}
			transactions[i] = btcwallet.TransactionSummary{
				Hash:        &txHash,
				Transaction: tx.Transaction,
				MyInputs:    inputs,
				MyOutputs:   outputs,
				Fee:         btcutil.Amount(tx.Fee),
				Timestamp:   tx.Timestamp,
				Label:       tx.Label,
			}
		}
		return transactions
	}

	btcGTR := &btcwallet.GetTransactionsResult{
		MinedTransactions:   make([]btcwallet.Block, len(lbcGTR.MinedTransactions)),
		UnminedTransactions: convertTxs(lbcGTR.UnminedTransactions),
	}
	for i, block := range lbcGTR.MinedTransactions {
		blockHash := hashToBTC(*block.Hash)
		btcGTR.MinedTransactions[i] = btcwallet.Block{
			Hash:         &blockHash,
			Height:       block.Height,
			Timestamp:    block.Timestamp,
			Transactions: convertTxs(block.Transactions),
		}
	}
	return btcGTR, nil
}

func (w *lbcSPVWallet) ListUnspent(minconf, maxconf int32, acctName string) ([]*btcjson.ListUnspentResult, error) {
	uns, err := w.Wallet.ListUnspent(minconf, maxconf, acctName)
	if err != nil {
		return nil, err
	}
	outs := make([]*btcjson.ListUnspentResult, 0, len(uns))
	for _, u := range uns {
		if acctName != "" && u.Account != acctName {
			continue
		}
		outs = append(outs, &btcjson.ListUnspentResult{
			TxID:          u.TxID,
			Vout:          u.Vout,
			Address:       u.Address,
			Account:       u.Account,
			ScriptPubKey:  u.ScriptPubKey,
			RedeemScript:  u.RedeemScript,
			Amount:        u.Amount,
			Confirmations: u.Confirmations,
			Spendable:     u.Spendable,
		})
	}
	return outs, nil
}

func (w *lbcSPVWallet) FetchInputInfo(prevOut *wire.OutPoint) (*wire.MsgTx, *wire.TxOut, *psbt.Bip32Derivation, int64, error) {
	op := lbcwire.OutPoint{Hash: hashToLBC(prevOut.Hash), Index: prevOut.Index}
	lbcTx, lbcOut, _, confs, err := w.Wallet.FetchInputInfo(&op)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	var btcTx *wire.MsgTx
	if lbcTx != nil {
		btcTx, err = convertMsgTxToBTC(lbcTx)
		if err != nil {
			return nil, nil, nil, 0, err
		}
	}
	btcOut := &wire.TxOut{Value: lbcOut.Value, PkScript: lbcOut.PkScript}
	return btcTx, btcOut, nil, confs, nil
}

func (w *lbcSPVWallet) ResetLockedOutpoints() {
	w.Wallet.ResetLockedOutpoints()
}

func (w *lbcSPVWallet) LockOutpoint(op wire.OutPoint) {
	w.Wallet.LockOutpoint(lbcwire.OutPoint{Hash: hashToLBC(op.Hash), Index: op.Index})
}

func (w *lbcSPVWallet) UnlockOutpoint(op wire.OutPoint) {
	w.Wallet.UnlockOutpoint(lbcwire.OutPoint{Hash: hashToLBC(op.Hash), Index: op.Index})
}

func (w *lbcSPVWallet) LockedOutpoints() []btcjson.TransactionInput {
	locks := w.Wallet.LockedOutpoints()
	locked := make([]btcjson.TransactionInput, len(locks))
	for i, lock := range locks {
		locked[i] = btcjson.TransactionInput{Txid: lock.Txid, Vout: lock.Vout}
	}
	return locked
}

func (w *lbcSPVWallet) NewChangeAddress(account uint32, _ waddrmgr.KeyScope) (btcutil.Address, error) {
	addr, err := w.Wallet.NewChangeAddress(account, lbcwaddrmgr.KeyScopeBIP0084)
	if err != nil {
		return nil, err
	}
	return w.addrLBC2BTC(addr)
}

func (w *lbcSPVWallet) NewAddress(account uint32, _ waddrmgr.KeyScope) (btcutil.Address, error) {
	addr, err := w.Wallet.NewAddress(account, lbcwaddrmgr.KeyScopeBIP0084)
	if err != nil {
		return nil, err
	}
	return w.addrLBC2BTC(addr)
}

func (w *lbcSPVWallet) PrivKeyForAddress(a btcutil.Address) (*btcec.PrivateKey, error) {
	lbcAddr, err := w.addrBTC2LBC(a)
	if err != nil {
		return nil, err
	}
	lbcKey, err := w.Wallet.PrivKeyForAddress(lbcAddr)
	if err != nil {
		return nil, err
	}
	priv, _ := btcec.PrivKeyFromBytes(lbcKey.Serialize())
	return priv, nil
}

func (w *lbcSPVWallet) SendOutputs(outputs []*wire.TxOut, _ *waddrmgr.KeyScope, account uint32, minconf int32,
	satPerKb btcutil.Amount, _ btcwallet.CoinSelectionStrategy, label string) (*wire.MsgTx, error) {

	lbcOuts := make([]*lbcwire.TxOut, len(outputs))
	for i, op := range outputs {
		lbcOuts[i] = &lbcwire.TxOut{Value: op.Value, PkScript: op.PkScript}
	}
	scope := lbcwaddrmgr.KeyScopeBIP0084
	lbcTx, err := w.Wallet.SendOutputs(lbcOuts, &scope, account, minconf,
		lbcutil.Amount(satPerKb), wallet.CoinSelectionRandom, label)
	if err != nil {
		return nil, err
	}
	return convertMsgTxToBTC(lbcTx)
}

func (w *lbcSPVWallet) HaveAddress(a btcutil.Address) (bool, error) {
	lbcAddr, err := w.addrBTC2LBC(a)
	if err != nil {
		return false, err
	}
	return w.Wallet.HaveAddress(lbcAddr)
}

func (w *lbcSPVWallet) Stop() {
	w.log.Info("Unloading wallet")
	if err := w.loader.UnloadWallet(); err != nil {
		w.log.Errorf("UnloadWallet error: %v", err)
	}
	if w.chainClient != nil {
		w.log.Trace("Stopping neutrino client chain interface")
		w.chainClient.Stop()
		w.chainClient.WaitForShutdown()
	}
	w.log.Trace("Stopping neutrino chain sync service")
	if w.cl != nil {
		if err := w.cl.Stop(); err != nil {
			w.log.Errorf("error stopping neutrino chain service: %v", err)
		}
	}
	w.log.Trace("Stopping neutrino DB.")
	if w.neutrinoDB != nil {
		if err := w.neutrinoDB.Close(); err != nil {
			w.log.Errorf("wallet db close error: %v", err)
		}
	}
	w.log.Info("SPV wallet closed")
}

func (w *lbcSPVWallet) AccountProperties(_ waddrmgr.KeyScope, acct uint32) (*waddrmgr.AccountProperties, error) {
	scope := lbcwaddrmgr.KeyScopeBIP0084
	props, err := w.Wallet.AccountProperties(scope, acct)
	if err != nil {
		return nil, err
	}
	return &waddrmgr.AccountProperties{
		AccountNumber:        props.AccountNumber,
		AccountName:          props.AccountName,
		ExternalKeyCount:     props.ExternalKeyCount,
		InternalKeyCount:     props.InternalKeyCount,
		ImportedKeyCount:     props.ImportedKeyCount,
		MasterKeyFingerprint: props.MasterKeyFingerprint,
		KeyScope: waddrmgr.KeyScope{
			Purpose: scope.Purpose,
			Coin:    scope.Coin,
		},
	}, nil
}

func (w *lbcSPVWallet) RescanAsync() error {
	w.log.Info("Stopping wallet and chain client...")
	w.Wallet.Stop()
	w.Wallet.WaitForShutdown()
	w.chainClient.WaitForShutdown()

	w.ForceRescan()

	w.log.Info("Starting wallet...")
	w.Wallet.Start()

	if err := w.chainClient.Start(); err != nil {
		return fmt.Errorf("couldn't start Neutrino client: %v", err)
	}

	w.log.Info("Synchronizing wallet with network...")
	w.Wallet.SynchronizeRPC(w.chainClient)
	return nil
}

func (w *lbcSPVWallet) ForceRescan() {
	w.log.Info("Dropping transaction history to perform full rescan...")
	if err := w.dropTransactionHistory(); err != nil {
		w.log.Errorf("Failed to drop wallet transaction history: %v", err)
	}

	err := walletdb.Update(w.Database(), func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(waddrmgrNamespace)
		return w.Manager.SetSyncedTo(ns, nil)
	})
	if err != nil {
		w.log.Errorf("Failed to reset wallet manager sync height: %v", err)
	}
}

func (w *lbcSPVWallet) dropTransactionHistory() error {
	w.log.Info("Dropping wallet transaction history")
	return walletdb.Update(w.Database(), func(tx walletdb.ReadWriteTx) error {
		err := tx.DeleteTopLevelBucket(wtxmgrNamespace)
		if err != nil && err != walletdb.ErrBucketNotFound {
			return err
		}
		ns, err := tx.CreateTopLevelBucket(wtxmgrNamespace)
		if err != nil {
			return err
		}
		if err = lbcwtxmgr.Create(ns); err != nil {
			return err
		}

		ns = tx.ReadWriteBucket(waddrmgrNamespace)
		birthdayBlock, err := lbcwaddrmgr.FetchBirthdayBlock(ns)
		if err != nil {
			startBlock, err2 := lbcwaddrmgr.FetchStartBlock(ns)
			if err2 != nil {
				return err2
			}
			return lbcwaddrmgr.PutSyncedTo(ns, startBlock)
		}
		if err := lbcwaddrmgr.DeleteBirthdayBlock(ns); err != nil {
			return err
		}
		if err := lbcwaddrmgr.PutSyncedTo(ns, &birthdayBlock); err != nil {
			return err
		}
		return lbcwaddrmgr.PutBirthdayBlock(ns, birthdayBlock)
	})
}

func (w *lbcSPVWallet) txDetails(txHash *lbcchainhash.Hash) (*lbcwtxmgr.TxDetails, error) {
	details, err := wallet.UnstableAPI(w.Wallet).TxDetails(txHash)
	if err != nil {
		return nil, err
	}
	if details == nil {
		return nil, btc.WalletTransactionNotFound
	}
	return details, nil
}

func (w *lbcSPVWallet) WalletTransaction(txHash *chainhash.Hash) (*wtxmgr.TxDetails, error) {
	h := hashToLBC(*txHash)
	txDetails, err := w.txDetails(&h)
	if err != nil {
		return nil, err
	}
	btcTx, err := convertMsgTxToBTC(&txDetails.MsgTx)
	if err != nil {
		return nil, err
	}
	credits := make([]wtxmgr.CreditRecord, len(txDetails.Credits))
	for i, c := range txDetails.Credits {
		credits[i] = wtxmgr.CreditRecord{
			Amount: btcutil.Amount(c.Amount),
			Index:  c.Index,
			Spent:  c.Spent,
			Change: c.Change,
		}
	}
	debits := make([]wtxmgr.DebitRecord, len(txDetails.Debits))
	for i, d := range txDetails.Debits {
		debits[i] = wtxmgr.DebitRecord{
			Amount: btcutil.Amount(d.Amount),
			Index:  d.Index,
		}
	}
	return &wtxmgr.TxDetails{
		TxRecord: wtxmgr.TxRecord{
			MsgTx:        *btcTx,
			Hash:         hashToBTC(txDetails.TxRecord.Hash),
			Received:     txDetails.TxRecord.Received,
			SerializedTx: txDetails.TxRecord.SerializedTx,
		},
		Block: wtxmgr.BlockMeta{
			Block: wtxmgr.Block{
				Hash:   hashToBTC(txDetails.Block.Hash),
				Height: txDetails.Block.Height,
			},
			Time: txDetails.Block.Time,
		},
		Credits: credits,
		Debits:  debits,
	}, nil
}

func (w *lbcSPVWallet) SyncedTo() waddrmgr.BlockStamp {
	bs := w.Manager.SyncedTo()
	return waddrmgr.BlockStamp{
		Height:    bs.Height,
		Hash:      hashToBTC(bs.Hash),
		Timestamp: bs.Timestamp,
	}
}

func (w *lbcSPVWallet) SignTx(btcTx *wire.MsgTx) error {
	lbcTx, err := convertMsgTxToLBC(btcTx)
	if err != nil {
		return err
	}
	var prevPkScripts [][]byte
	var inputValues []lbcutil.Amount
	for _, txIn := range btcTx.TxIn {
		_, txOut, _, _, err := w.FetchInputInfo(&txIn.PreviousOutPoint)
		if err != nil {
			return err
		}
		inputValues = append(inputValues, lbcutil.Amount(txOut.Value))
		prevPkScripts = append(prevPkScripts, txOut.PkScript)
		txIn.SignatureScript = nil
		txIn.Witness = nil
	}
	err = txauthor.AddAllInputScripts(lbcTx, prevPkScripts, inputValues, &secretSource{w.Wallet, w.chainParams})
	if err != nil {
		return err
	}
	if len(lbcTx.TxIn) != len(btcTx.TxIn) {
		return fmt.Errorf("txin count mismatch")
	}
	for i, txIn := range btcTx.TxIn {
		lbcIn := lbcTx.TxIn[i]
		txIn.SignatureScript = lbcIn.SignatureScript
		txIn.Witness = make(wire.TxWitness, len(lbcIn.Witness))
		copy(txIn.Witness, lbcIn.Witness)
	}
	return nil
}

func (w *lbcSPVWallet) BlockNotifications(ctx context.Context) <-chan *btc.BlockNotification {
	cl := w.NtfnServer.TransactionNotifications()
	ch := make(chan *btc.BlockNotification, 1)
	go func() {
		defer cl.Done()
		for {
			select {
			case note := <-cl.C:
				if len(note.AttachedBlocks) > 0 {
					lastBlock := note.AttachedBlocks[len(note.AttachedBlocks)-1]
					select {
					case ch <- &btc.BlockNotification{
						Hash:   hashToBTC(*lastBlock.Hash),
						Height: lastBlock.Height,
					}:
					default:
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

func (w *lbcSPVWallet) Peers() ([]*asset.WalletPeer, error) {
	return w.peerManager.Peers()
}

func (w *lbcSPVWallet) AddPeer(addr string) error {
	return w.peerManager.AddPeer(addr)
}

func (w *lbcSPVWallet) RemovePeer(addr string) error {
	return w.peerManager.RemovePeer(addr)
}

func (w *lbcSPVWallet) TotalReceivedForAddr(btcAddr btcutil.Address, minConf int32) (btcutil.Amount, error) {
	lbcAddr, err := w.addrBTC2LBC(btcAddr)
	if err != nil {
		return 0, err
	}
	amt, err := w.Wallet.TotalReceivedForAddr(lbcAddr, minConf)
	if err != nil {
		return 0, err
	}
	return btcutil.Amount(amt), nil
}

type secretSource struct {
	w           *wallet.Wallet
	chainParams *lbcchaincfg.Params
}

func (s *secretSource) ChainParams() *lbcchaincfg.Params { return s.chainParams }

func (s *secretSource) GetKey(addr lbcutil.Address) (*lbcbtcec.PrivateKey, bool, error) {
	ma, err := s.w.AddressInfo(addr)
	if err != nil {
		return nil, false, err
	}
	mpka, ok := ma.(lbcwaddrmgr.ManagedPubKeyAddress)
	if !ok {
		return nil, false, fmt.Errorf("managed address type for %v is `%T` but want waddrmgr.ManagedPubKeyAddress", addr, ma)
	}
	privKey, err := mpka.PrivKey()
	if err != nil {
		return nil, false, err
	}
	return privKey, ma.Compressed(), nil
}

func (s *secretSource) GetScript(addr lbcutil.Address) ([]byte, error) {
	ma, err := s.w.AddressInfo(addr)
	if err != nil {
		return nil, err
	}
	msa, ok := ma.(lbcwaddrmgr.ManagedScriptAddress)
	if !ok {
		return nil, fmt.Errorf("managed address type for %v is `%T` but want waddrmgr.ManagedScriptAddress", addr, ma)
	}
	return msa.Script()
}

type spvService struct {
	*neutrino.ChainService
}

var _ btc.SPVService = (*spvService)(nil)

func (s *spvService) GetBlockHash(height int64) (*chainhash.Hash, error) {
	h, err := s.ChainService.GetBlockHash(height)
	if err != nil {
		return nil, err
	}
	return hashPtrToBTC(h), nil
}

func (s *spvService) BestBlock() (*headerfs.BlockStamp, error) {
	bs, err := s.ChainService.BestBlock()
	if err != nil {
		return nil, err
	}
	return &headerfs.BlockStamp{
		Height:    bs.Height,
		Hash:      hashToBTC(bs.Hash),
		Timestamp: bs.Timestamp,
	}, nil
}

func (s *spvService) Peers() []btc.SPVPeer {
	rawPeers := s.ChainService.Peers()
	peers := make([]btc.SPVPeer, len(rawPeers))
	for i, p := range rawPeers {
		peers[i] = p
	}
	return peers
}

func (s *spvService) AddPeer(addr string) error {
	return s.ChainService.ConnectNode(addr, true)
}

func (s *spvService) RemovePeer(addr string) error {
	return s.ChainService.RemoveNodeByAddr(addr)
}

func (s *spvService) GetBlockHeight(h *chainhash.Hash) (int32, error) {
	lh := hashToLBC(*h)
	return s.ChainService.GetBlockHeight(&lh)
}

func (s *spvService) GetBlockHeader(h *chainhash.Hash) (*wire.BlockHeader, error) {
	lh := hashToLBC(*h)
	hdr, err := s.ChainService.GetBlockHeader(&lh)
	if err != nil {
		return nil, err
	}
	return headerToBTC(hdr), nil
}

func (s *spvService) GetCFilter(blockHash chainhash.Hash, filterType wire.FilterType, _ ...btcneutrino.QueryOption) (*gcs.Filter, error) {
	f, err := s.ChainService.GetCFilter(hashToLBC(blockHash), lbcwire.GCSFilterRegular)
	if err != nil {
		return nil, err
	}
	b, err := f.Bytes()
	if err != nil {
		return nil, err
	}
	return gcs.FromBytes(f.N(), f.P(), DefaultM, b)
}

func (s *spvService) GetBlock(blockHash chainhash.Hash, _ ...btcneutrino.QueryOption) (*btcutil.Block, error) {
	blk, err := s.ChainService.GetBlock(hashToLBC(blockHash))
	if err != nil {
		return nil, err
	}
	raw, err := blk.Bytes()
	if err != nil {
		return nil, err
	}
	msg, err := dexlbc.DeserializeBlock(raw)
	if err != nil {
		return nil, err
	}
	return btcutil.NewBlock(msg), nil
}

func extendAddresses(extIdx, intIdx uint32, w *wallet.Wallet) error {
	scopedKeyManager, err := w.Manager.FetchScopedKeyManager(lbcwaddrmgr.KeyScopeBIP0084)
	if err != nil {
		return err
	}
	return walletdb.Update(w.Database(), func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(waddrmgrNamespace)
		if extIdx > 0 {
			if err := scopedKeyManager.ExtendAddresses(ns, defaultAcctNum, 0, extIdx); err != nil {
				return err
			}
		}
		if intIdx > 0 {
			return scopedKeyManager.ExtendAddresses(ns, defaultAcctNum, 1, intIdx)
		}
		return nil
	})
}

var loggingInited uint32

func logRotator(dir string) (*rotator.Rotator, error) {
	const maxLogRolls = 8
	logDir := filepath.Join(dir, logDirName)
	if err := os.MkdirAll(logDir, 0o744); err != nil {
		return nil, fmt.Errorf("error creating log directory: %w", err)
	}
	return rotator.New(filepath.Join(logDir, "neutrino.log"), 32*1024, false, maxLogRolls)
}

func logNeutrino(walletDir string, baseLogger dex.Logger) error {
	if !atomic.CompareAndSwapUint32(&loggingInited, 0, 1) {
		return nil
	}
	logSpinner, err := logRotator(walletDir)
	if err != nil {
		return fmt.Errorf("error initializing log rotator: %w", err)
	}
	backendLog := btclog.NewBackend(logSpinner)
	logger := func(name string, lvl btclog.Level) btclog.Logger {
		l := backendLog.Logger(name)
		l.SetLevel(lvl)
		return l
	}
	neutrino.UseLogger(logger("NTRNO", btclog.LevelDebug))
	wallet.UseLogger(logger("LBCW", btclog.LevelInfo))
	lbcwtxmgr.UseLogger(logger("TXMGR", btclog.LevelInfo))
	chain.UseLogger(logger("CHAIN", btclog.LevelInfo))
	nlog = logger("NCLNT", btclog.LevelInfo)
	_ = baseLogger
	return nil
}
