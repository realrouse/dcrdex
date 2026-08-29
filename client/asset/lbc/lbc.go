// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/client/asset/btc"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/config"
	dexbtc "decred.org/dcrdex/dex/networks/btc"
	dexlbc "decred.org/dcrdex/dex/networks/lbc"
	"github.com/btcsuite/btcd/chaincfg"
	lbcchaincfg "github.com/lbryio/lbcd/chaincfg"
	"github.com/lbryio/lbcwallet/wallet"
)

const (
	version = 0
	// BipID is the BIP-0044 / SLIP-0044 coin type for LBRY Credits.
	BipID = 140

	// minNetworkVersion is intentionally low: local/dev lbcd builds report a
	// small version.Numeric(). Operators of released binaries will still pass.
	minNetworkVersion = 0
	walletTypeRPC     = "lbcwalletRPC"
	walletTypeSPV     = "SPV"
	walletTypeLegacy  = ""
)

var (
	rpcWalletDefinition = &asset.WalletDefinition{
		Type:              walletTypeRPC,
		Tab:               "External",
		Description:       "Connect to your lbcwallet (port 9244). The DEX server cannot supply this password.",
		DefaultConfigPath: dexbtc.SystemConfigPath("lbcwallet"),
		ConfigOpts:        nil, // filled in init below after configOpts
		MultiFundingOpts:  btc.MultiFundingOpts,
		GuideLink:         "https://dex.revivel.app/",
	}
	spvWalletDefinition = &asset.WalletDefinition{
		Type:             walletTypeSPV,
		Tab:              "Native",
		Description:      "Built-in light wallet. Does not download the LBRY blockchain.",
		ConfigOpts:       btc.CommonConfigOpts("LBC", true),
		Seeded:           true,
		MultiFundingOpts: btc.MultiFundingOpts,
	}

	configOpts = append([]*asset.ConfigOption{
		{
			Key:         "rpcuser",
			DisplayName: "JSON-RPC Username",
			Description: "rpcuser from YOUR lbcwallet.conf (not the DEX server). " +
				"lbcwallet must be running and unlocked.",
			Required:      true,
			ShowByDefault: true,
		},
		{
			Key:         "rpcpassword",
			DisplayName: "JSON-RPC Password",
			Description: "rpcpass / rpcpassword from YOUR lbcwallet.conf. " +
				"This is the wallet RPC password, not the DEX node's password.",
			NoEcho:        true,
			Required:      true,
			ShowByDefault: true,
		},
		{
			Key:           "rpcbind",
			DisplayName:   "JSON-RPC Address",
			Description:   "Host of lbcwallet (default 127.0.0.1). Use 10.8.0.2 if the wallet is on the WireGuard Windows PC.",
			DefaultValue:  "127.0.0.1",
			ShowByDefault: true,
		},
		{
			Key:           "rpcport",
			DisplayName:   "JSON-RPC Port",
			Description:   "lbcwallet RPC port (9244), not lbcd 9245",
			DefaultValue:  "9244",
			ShowByDefault: true,
		},
		{
			Key:         "nontls",
			DisplayName: "Disable TLS",
			Description: "Only if lbcwallet.conf has noservertls=1. Leave off for stock lbcwallet (HTTPS).",
			IsBoolean:   true,
		},
		{
			Key:         "rpccert",
			DisplayName: "TLS certificate",
			Description: "Path to lbcwallet rpc.cert (default ~/.lbcwallet/rpc.cert)",
		},
	}, []*asset.ConfigOption{
		{
			Key:          "fallbackfee",
			DisplayName:  "Fallback fee rate",
			Description:  "LBC's fallback fee rate. Units: LBC/kB",
			DefaultValue: strconv.FormatFloat(dexlbc.DefaultFee*1000/1e8, 'f', -1, 64),
		},
		{
			Key:         "feeratelimit",
			DisplayName: "Highest acceptable fee rate",
			Description: "This is the highest network fee rate you are willing to " +
				"pay on swap transactions. If feeratelimit is lower than a market's " +
				"maxfeerate, you will not be able to trade on that market with this " +
				"wallet. Units: LBC/kB",
			DefaultValue: strconv.FormatFloat(dexlbc.DefaultFeeRateLimit*1000/1e8, 'f', -1, 64),
		},
		{
			Key:         "txsplit",
			DisplayName: "Pre-split funding inputs",
			Description: "When placing an order, create a \"split\" transaction to fund the order without locking more of the wallet balance than " +
				"necessary. Otherwise, excess funds may be reserved to fund the order until the first swap contract is broadcast " +
				"during match settlement, or the order is canceled. This is an extra transaction for which network mining fees are paid. " +
				"Used only for standing-type orders, e.g. limit orders without immediate time-in-force.",
			IsBoolean:    true,
			DefaultValue: "true",
		},
	}...)
	// WalletInfo defines some general information about an LBC wallet.
	WalletInfo = &asset.WalletInfo{
		Name:              "LBRY Credits",
		SupportedVersions: []uint32{version},
		UnitInfo:          dexlbc.UnitInfo,
		AvailableWallets: []*asset.WalletDefinition{
			spvWalletDefinition,
			rpcWalletDefinition,
		},
		LegacyWalletIndex: 1,
		BlockchainClass:   asset.BlockchainClassUTXO,
	}
)

func init() {
	rpcWalletDefinition.ConfigOpts = configOpts
	asset.Register(BipID, &Driver{})
}

// Driver implements asset.Driver.
type Driver struct{}

var _ asset.Driver = (*Driver)(nil)
var _ asset.Creator = (*Driver)(nil)

// Open creates the LBC exchange wallet.
func (d *Driver) Open(cfg *asset.WalletConfig, logger dex.Logger, network dex.Network) (asset.Wallet, error) {
	return NewWallet(cfg, logger, network)
}

// Exists reports whether a Native SPV wallet already exists. Part of Creator.
func (d *Driver) Exists(walletType, dataDir string, _ map[string]string, net dex.Network) (bool, error) {
	if walletType != walletTypeSPV {
		return false, fmt.Errorf("no LBC wallet of type %q available", walletType)
	}
	chainParams, err := parseChainParams(net)
	if err != nil {
		return false, err
	}
	walletDir := filepath.Join(dataDir, chainParams.Name)
	loader := wallet.NewLoader(chainParams, walletDir, true, dbTimeout, 250)
	return loader.WalletExists()
}

// Create creates a Native SPV wallet. Part of Creator.
func (d *Driver) Create(params *asset.CreateWalletParams) error {
	if params.Type != walletTypeSPV {
		return fmt.Errorf("SPV is the only seeded wallet type. required = %q, requested = %q", walletTypeSPV, params.Type)
	}
	if len(params.Seed) == 0 {
		return errors.New("wallet seed cannot be empty")
	}
	if len(params.DataDir) == 0 {
		return errors.New("must specify wallet data directory")
	}
	chainParams, err := parseChainParams(params.Net)
	if err != nil {
		return fmt.Errorf("error parsing chain: %w", err)
	}

	recoveryCfg := new(btc.RecoveryCfg)
	if err := config.Unmapify(params.Settings, recoveryCfg); err != nil {
		return err
	}

	bday := btc.DefaultWalletBirthday
	if params.Birthday != 0 {
		bday = time.Unix(int64(params.Birthday), 0)
	}

	walletDir := filepath.Join(params.DataDir, chainParams.Name)
	return createSPVWallet(params.Pass, params.Seed, bday, walletDir,
		params.Logger, recoveryCfg.NumExternalAddresses, recoveryCfg.NumInternalAddresses, chainParams)
}

// DecodeCoinID creates a human-readable representation of a coin ID for LBC.
func (d *Driver) DecodeCoinID(coinID []byte) (string, error) {
	return (&btc.Driver{}).DecodeCoinID(coinID)
}

// Info returns basic information about the wallet and asset.
func (d *Driver) Info() *asset.WalletInfo {
	return WalletInfo
}

// MinLotSize calculates the minimum lot size for a given fee rate.
func (d *Driver) MinLotSize(maxFeeRate uint64) uint64 {
	return dexbtc.MinLotSize(maxFeeRate, true)
}

func toSatoshi(v float64) uint64 {
	return uint64(math.Round(v * 1e8))
}

func parseCloneParams(network dex.Network) (*chaincfg.Params, error) {
	switch network {
	case dex.Mainnet:
		return dexlbc.MainNetParams, nil
	case dex.Testnet:
		return dexlbc.TestNet3Params, nil
	case dex.Regtest:
		return dexlbc.RegressionNetParams, nil
	default:
		return nil, fmt.Errorf("unknown network ID %v", network)
	}
}

func parseChainParams(net dex.Network) (*lbcchaincfg.Params, error) {
	switch net {
	case dex.Mainnet:
		return &lbcchaincfg.MainNetParams, nil
	case dex.Testnet:
		return &lbcchaincfg.TestNet3Params, nil
	case dex.Regtest:
		return &lbcchaincfg.RegressionNetParams, nil
	}
	return nil, fmt.Errorf("unknown network ID %v", net)
}

func baseCloneCFG(cfg *asset.WalletConfig, logger dex.Logger, network dex.Network, params *chaincfg.Params) *btc.BTCCloneCFG {
	return &btc.BTCCloneCFG{
		WalletCFG:           cfg,
		MinNetworkVersion:   minNetworkVersion,
		MinProtocolVersion:  70013, // lbcd maxProtocolVersion
		WalletInfo:          WalletInfo,
		Symbol:              "lbc",
		Logger:              logger,
		Network:             network,
		ChainParams:         params,
		DefaultFallbackFee:  dexlbc.DefaultFee,
		DefaultFeeRateLimit: dexlbc.DefaultFeeRateLimit,
		BlockDeserializer:   dexlbc.DeserializeBlock,
		AssetID:             BipID,
		FeeEstimator: func(ctx context.Context, cl btc.RawRequester, confTarget uint64) (uint64, error) {
			return dexlbc.DefaultFee, nil
		},
	}
}

// NewWallet is the exported constructor by which the DEX will import the
// exchange wallet. Native SPV needs no local lbcd. External RPC still talks to
// lbcwallet on port 9244.
func NewWallet(cfg *asset.WalletConfig, logger dex.Logger, network dex.Network) (asset.Wallet, error) {
	params, err := parseCloneParams(network)
	if err != nil {
		return nil, err
	}

	switch cfg.Type {
	case walletTypeSPV:
		cloneCFG := baseCloneCFG(cfg, logger, network, params)
		cloneCFG.Segwit = true
		cloneCFG.InitTxSize = dexbtc.InitTxSizeSegwit
		cloneCFG.InitTxSizeBase = dexbtc.InitTxSizeBaseSegwit
		return btc.OpenSPVWallet(cloneCFG, openSPVWallet)
	case walletTypeRPC, walletTypeLegacy:
		return newRPCWallet(cfg, logger, network, params)
	default:
		return nil, fmt.Errorf("unknown wallet type %q", cfg.Type)
	}
}

func newRPCWallet(cfg *asset.WalletConfig, logger dex.Logger, network dex.Network, params *chaincfg.Params) (asset.Wallet, error) {
	if cfg.Settings == nil {
		cfg.Settings = make(map[string]string)
	}
	dexbtc.AliasRPCPassword(cfg.Settings)
	if cfg.Settings["rpcuser"] == "" || cfg.Settings["rpcpassword"] == "" {
		return nil, fmt.Errorf("lbcwallet RPC user/password are required. Copy rpcuser and rpcpass from lbcwallet.conf on the machine running lbcwallet. The DEX server does not have your wallet password")
	}
	if err := pingLBCWalletRPC(cfg.Settings); err != nil {
		return nil, err
	}
	useTLS, tlsCert, err := lbcRPCUseTLS(cfg.Settings)
	if err != nil {
		return nil, err
	}

	// Wallet RPC ports (lbcwallet), not node ports.
	ports := dexbtc.NetPorts{
		Mainnet: "9244",
		Testnet: "19244",
		Simnet:  "29244",
	}

	// w is closed over by BalanceFunc (same pattern as ZCL).
	var w *btc.ExchangeWalletFullNode
	cloneCFG := baseCloneCFG(cfg, logger, network, params)
	cloneCFG.Ports = ports
	cloneCFG.RPCUseTLS = useTLS
	cloneCFG.RPCTLSCert = tlsCert
	cloneCFG.BalanceFunc = func(ctx context.Context, locked uint64) (*asset.Balance, error) {
		var bal float64
		if err := w.CallRPC("getbalance", []any{"*", 0}, &bal); err != nil {
			if err2 := w.CallRPC("getbalance", nil, &bal); err2 != nil {
				return nil, fmt.Errorf("getbalance: %v (fallback: %v)", err, err2)
			}
		}
		return &asset.Balance{
			Available: toSatoshi(bal) - locked,
			Locked:    locked,
			Other:     make(map[asset.BalanceCategory]asset.CustomBalance),
		}, nil
	}
	// lbcwallet getrawchangeaddress is (account, addresstype). AccountFirstAddrRPC
	// sends ("default", "bech32") instead of Bitcoin Core's ("bech32").
	cloneCFG.Segwit = true
	cloneCFG.InitTxSize = dexbtc.InitTxSizeSegwit
	cloneCFG.InitTxSizeBase = dexbtc.InitTxSizeBaseSegwit
	cloneCFG.AccountFirstAddrRPC = true
	cloneCFG.OmitAddressType = true // skip fundrawtransaction ChangeType
	cloneCFG.LegacySignTxRPC = true
	cloneCFG.LegacyValidateAddressRPC = true
	cloneCFG.SingularWallet = true
	cloneCFG.OptionalWalletInfo = true
	cloneCFG.UnlockSpends = true

	w, err = btc.BTCCloneWallet(cloneCFG)
	return w, err
}

func pingLBCWalletRPC(settings map[string]string) error {
	host := strings.TrimSpace(settings["rpcbind"])
	if host == "" {
		host = "127.0.0.1"
	}
	port := strings.TrimSpace(settings["rpcport"])
	if port == "" {
		port = "9244"
	}
	addr := host
	if _, _, err := net.SplitHostPort(host); err != nil {
		addr = net.JoinHostPort(host, port)
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("lbcwallet is not accepting RPC at %s (%v). A conf file is not enough — start the lbcwallet process. Use port 9244 (wallet), not 9245 (lbcd). If the wallet is on another PC, set JSON-RPC Address to that host (e.g. 10.8.0.2). Add noservertls=1 to lbcwallet.conf because bisonw uses HTTP", addr, err)
	}
	_ = c.Close()
	return nil
}

func lbcRPCUseTLS(settings map[string]string) (bool, []byte, error) {
	switch strings.ToLower(strings.TrimSpace(settings["nontls"])) {
	case "1", "true", "yes":
		return false, nil, nil
	}
	certPath := settings["rpccert"]
	if certPath == "" {
		certPath = filepath.Join(filepath.Dir(dexbtc.SystemConfigPath("lbcwallet")), "rpc.cert")
	}
	pem, err := os.ReadFile(certPath)
	if err != nil {
		return false, nil, fmt.Errorf("lbcwallet RPC uses TLS by default; cannot read %s: %w. Start lbcwallet so it writes rpc.cert, or add noservertls=1 to lbcwallet.conf and enable Disable TLS here", certPath, err)
	}
	return true, pem, nil
}
