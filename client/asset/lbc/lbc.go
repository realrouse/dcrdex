// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"context"
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
	dexbtc "decred.org/dcrdex/dex/networks/btc"
	dexlbc "decred.org/dcrdex/dex/networks/lbc"
	"github.com/btcsuite/btcd/chaincfg"
)

const (
	version = 0
	// BipID is the BIP-0044 / SLIP-0044 coin type for LBRY Credits.
	BipID = 140

	// minNetworkVersion is intentionally low: local/dev lbcd builds report a
	// small version.Numeric(). Operators of released binaries will still pass.
	minNetworkVersion = 0
	walletTypeRPC     = "lbcwalletRPC"
)

var (
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
		AvailableWallets: []*asset.WalletDefinition{{
			Type:              walletTypeRPC,
			Tab:               "External",
			Description:       "Connect to your lbcwallet (port 9244). The DEX server cannot supply this password.",
			DefaultConfigPath: dexbtc.SystemConfigPath("lbcwallet"),
			ConfigOpts:        configOpts,
			GuideLink:         "https://dex.revivel.app/",
		}},
		BlockchainClass: asset.BlockchainClassUTXO,
	}
)

func init() {
	asset.Register(BipID, &Driver{})
}

// Driver implements asset.Driver.
type Driver struct{}

// Open creates the LBC exchange wallet.
func (d *Driver) Open(cfg *asset.WalletConfig, logger dex.Logger, network dex.Network) (asset.Wallet, error) {
	return NewWallet(cfg, logger, network)
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
	return dexbtc.MinLotSize(maxFeeRate, false)
}

func toSatoshi(v float64) uint64 {
	return uint64(math.Round(v * 1e8))
}

// NewWallet is the exported constructor by which the DEX will import the
// exchange wallet. Connect to lbcwallet's legacy JSON-RPC (default port 9244).
// lbcwallet must be connected to an lbcd node for chain RPCs (passthrough).
func NewWallet(cfg *asset.WalletConfig, logger dex.Logger, network dex.Network) (asset.Wallet, error) {
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

	var params *chaincfg.Params
	switch network {
	case dex.Mainnet:
		params = dexlbc.MainNetParams
	case dex.Testnet:
		params = dexlbc.TestNet3Params
	case dex.Regtest:
		params = dexlbc.RegressionNetParams
	default:
		return nil, fmt.Errorf("unknown network ID %v", network)
	}

	// Wallet RPC ports (lbcwallet), not node ports.
	ports := dexbtc.NetPorts{
		Mainnet: "9244",
		Testnet: "19244",
		Simnet:  "29244",
	}

	// w is closed over by BalanceFunc / FeeEstimator (same pattern as ZCL).
	var w *btc.ExchangeWalletFullNode
	cloneCFG := &btc.BTCCloneCFG{
		WalletCFG:           cfg,
		MinNetworkVersion:   minNetworkVersion,
		MinProtocolVersion:  70013, // lbcd maxProtocolVersion
		WalletInfo:          WalletInfo,
		Symbol:              "lbc",
		Logger:              logger,
		Network:             network,
		ChainParams:         params,
		Ports:               ports,
		RPCUseTLS:           useTLS,
		RPCTLSCert:          tlsCert,
		DefaultFallbackFee:  dexlbc.DefaultFee,
		DefaultFeeRateLimit: dexlbc.DefaultFeeRateLimit,
		// lbcwallet has no getwalletinfo / getbalances; use getbalance.
		BalanceFunc: func(ctx context.Context, locked uint64) (*asset.Balance, error) {
			var bal float64
			// minconf=0 to include unconfirmed; account "" is default.
			if err := w.CallRPC("getbalance", []any{"*", 0}, &bal); err != nil {
				// Fallback: no-arg getbalance
				if err2 := w.CallRPC("getbalance", nil, &bal); err2 != nil {
					return nil, fmt.Errorf("getbalance: %v (fallback: %v)", err, err2)
				}
			}
			return &asset.Balance{
				Available: toSatoshi(bal) - locked,
				Locked:    locked,
				Other:     make(map[asset.BalanceCategory]asset.CustomBalance),
			}, nil
		},
		// Non-segwit for lbcwallet RPC compatibility: getrawchangeaddress takes
		// (account, addresstype); dcrdex would pass "bech32" as account. Legacy
		// P2SH swap contracts still work on LBC mainnet (SegWit is optional).
		// Follow-up: add AccountFirstChangeAddr support and enable Segwit.
		Segwit:                   false,
		InitTxSize:               dexbtc.InitTxSize,
		InitTxSizeBase:           dexbtc.InitTxSizeBase,
		OmitAddressType:          true,
		LegacySignTxRPC:          true,
		LegacyValidateAddressRPC: true,
		SingularWallet:           true,
		OptionalWalletInfo:       true, // lbcwallet has no getwalletinfo
		UnlockSpends:             true, // lbcwallet may not auto-unlock spent coins
		BlockDeserializer:        dexlbc.DeserializeBlock,
		AssetID:                  BipID,
		FeeEstimator: func(ctx context.Context, cl btc.RawRequester, confTarget uint64) (uint64, error) {
			// Prefer estimatesmartfee if lbcd provides it via passthrough.
			// Fall back to DefaultFee.
			return dexlbc.DefaultFee, nil
		},
	}

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
