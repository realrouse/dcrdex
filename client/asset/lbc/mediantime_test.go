// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lbc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"decred.org/dcrdex/client/asset"
	"decred.org/dcrdex/dex"
)

// fakeLBCWalletRPC answers the few lbcwallet/lbcd JSON-RPC calls that the
// lbcwalletRPC wallet needs to connect and compute the median time. Like real
// lbcd, getblockheader carries no "mediantime" field.
type fakeLBCWalletRPC struct {
	times []int64 // block timestamps, index = height
}

func fakeHash(h int) string { return fmt.Sprintf("%064x", h+1) }

func (f *fakeLBCWalletRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req struct {
		ID     any               `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	json.Unmarshal(b, &req)
	tip := len(f.times) - 1
	var res any
	var rpcErr any
	switch req.Method {
	case "getnetworkinfo":
		res = map[string]any{"version": minNetworkVersion, "protocolversion": 70013, "subversion": "/lbcwallet-fake/"}
	case "getblockchaininfo":
		res = map[string]any{"chain": "regtest", "blocks": tip, "headers": tip, "bestblockhash": fakeHash(tip)}
	case "getbestblockhash":
		res = fakeHash(tip)
	case "getnewaddress", "getrawchangeaddress":
		res = "rlbc1q7au2mpzqa8cdre38hxa8mrhr0gd94vse3u6j5u"
	case "validateaddress", "getaddressinfo":
		res = map[string]any{"isvalid": true, "ismine": true, "address": "rlbc1q7au2mpzqa8cdre38hxa8mrhr0gd94vse3u6j5u"}
	case "listunspent", "listlockunspent", "listtransactions":
		res = []any{}
	case "getbalance":
		res = 0
	case "getblockcount":
		res = tip
	case "getblockhash":
		var h int
		json.Unmarshal(req.Params[0], &h)
		res = fakeHash(h)
	case "getblockheader":
		var hash string
		json.Unmarshal(req.Params[0], &hash)
		h := -1
		for i := range f.times {
			if fakeHash(i) == hash {
				h = i
			}
		}
		if h < 0 {
			rpcErr = map[string]any{"code": -5, "message": "block not found"}
			break
		}
		hdr := map[string]any{
			"hash": hash, "confirmations": tip - h + 1, "height": h, "version": 1,
			"time": f.times[h], "bits": "207fffff", "difficulty": 1,
		}
		if h > 0 {
			hdr["previousblockhash"] = fakeHash(h - 1)
		}
		res = hdr // no "mediantime", same as lbcd
	default:
		rpcErr = map[string]any{"code": -32601, "message": "Method not found"}
	}
	json.NewEncoder(w).Encode(map[string]any{"result": res, "error": rpcErr, "id": req.ID})
}

// TestRPCWalletMedianTime makes sure the lbcwalletRPC wallet computes the median
// time past itself. lbcd's getblockheader has no mediantime, so reading it
// from the header gave 1970 and refunds were never allowed.
func TestRPCWalletMedianTime(t *testing.T) {
	base := time.Now().Add(-24 * time.Hour).Unix()
	f := &fakeLBCWalletRPC{}
	for i := 0; i < 30; i++ {
		f.times = append(f.times, base+int64(i)*150)
	}
	// Make one recent timestamp out of order, which the median must tolerate.
	f.times[25], f.times[26] = f.times[26], f.times[25]
	srv := httptest.NewServer(f)
	defer srv.Close()

	tip := len(f.times) - 1
	window := append([]int64(nil), f.times[tip-10:]...)
	for i := range window { // insertion sort, median of the last 11
		for j := i; j > 0 && window[j] < window[j-1]; j-- {
			window[j], window[j-1] = window[j-1], window[j]
		}
	}
	expMedian := time.Unix(window[5], 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := dex.StdOutLogger("T", dex.LevelWarn)
	w, err := NewWallet(&asset.WalletConfig{
		Type: walletTypeRPC,
		Settings: map[string]string{
			"rpcuser": "user", "rpcpassword": "pass", "nontls": "1",
			"rpcbind": strings.TrimPrefix(srv.URL, "http://"),
		},
		DataDir:     t.TempDir(),
		Emit:        asset.NewWalletEmitter(make(chan asset.WalletNotification, 16), BipID, logger),
		PeersChange: func(uint32, error) {},
	}, logger, dex.Regtest)
	if err != nil {
		t.Fatalf("NewWallet: %v", err)
	}
	cm := dex.NewConnectionMaster(w)
	if err := cm.ConnectOnce(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer cm.Disconnect()

	for _, tt := range []struct {
		lockTime time.Time
		expired  bool
	}{
		{expMedian.Add(-time.Second), true},
		{expMedian, false}, // median must be strictly after the lock time
		{expMedian.Add(time.Second), false},
		{time.Unix(f.times[tip], 0), false}, // tip time alone is not enough
	} {
		expired, err := w.LockTimeExpired(ctx, tt.lockTime)
		if err != nil {
			t.Fatalf("LockTimeExpired(%v): %v", tt.lockTime, err)
		}
		if expired != tt.expired {
			t.Fatalf("LockTimeExpired(%v) = %v, want %v (median %v)", tt.lockTime, expired, tt.expired, expMedian)
		}
	}
}
