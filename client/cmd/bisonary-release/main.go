// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// bisonary-release signs and verifies bisonary release files (S0).
//
//	bisonary-release sign -key dev.key -name NAME -validators id1,id2,... [-names v1,v2,...] [-bonds 1,1,...]
//	    [-epochms 10000 -windowms 5000 -proposems 300 -fallbackms 1500 -quorum 2/3 -floor 1/2 -clockms 2000] -out release.json
//	    [-s4 -bondbtc 0:10000 -bondlbc 0:100000000000000 -bonddcr 0:1000000000 -bondage 2419200 -bondminlock 15724800
//	     -bondlockleft 604800 -bondconfs 2 -bondswitch 3/4 -bonddeadline 0 -bondpower 1 -bondmaxclaims 8]
//	    [-s6 -ubondbtc 10000 -ubondlbc 1000000000000 -ubonddcr 50000000 -ubondage 3600 -ubondminlock 2592000
//	     -ubondlockleft 172800 -ubondconfs 2 -ubondlots 5 -ubondorders 10 -ubondmaxtier 100 -ubondmaxclaims 8]
//	    [-s7 -tradeconfs 1 -trademaxproofs 16 -tradeage 86400 -tradelockleft 120]
//	bisonary-release verify [-pubkey HEX] release.json
//	bisonary-release pubkey -key dev.key
//
// The key file holds the hex ed25519 seed. Parameters default to
// mesh.DefaultEpochParams.
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"decred.org/dcrdex/client/mesh"
)

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}

func loadKey(path string) ed25519.PrivateKey {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal("read key: %v", err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		fatal("bad key file")
	}
	return ed25519.NewKeyFromSeed(seed)
}

func frac(s string) (int64, int64) {
	p := strings.Split(s, "/")
	if len(p) != 2 {
		fatal("bad fraction %q", s)
	}
	n, err1 := strconv.ParseInt(p[0], 10, 64)
	d, err2 := strconv.ParseInt(p[1], 10, 64)
	if err1 != nil || err2 != nil {
		fatal("bad fraction %q", s)
	}
	return n, d
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: bisonary-release sign|verify|pubkey ...")
	}
	switch os.Args[1] {
	case "pubkey":
		fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
		key := fs.String("key", "", "key file")
		_ = fs.Parse(os.Args[2:])
		fmt.Println(hex.EncodeToString(loadKey(*key).Public().(ed25519.PublicKey)))
	case "sign":
		d := mesh.DefaultEpochParams
		fs := flag.NewFlagSet("sign", flag.ExitOnError)
		key := fs.String("key", "", "key file")
		name := fs.String("name", "bisonary-simnet", "release name")
		network := fs.String("network", "simnet", "network")
		vals := fs.String("validators", "", "comma-separated validator peer IDs")
		names := fs.String("names", "", "comma-separated validator names")
		bonds := fs.String("bonds", "", "comma-separated bond powers (default 1 each)")
		epochMs := fs.Int64("epochms", d.EpochMs, "epoch length ms")
		windowMs := fs.Int64("windowms", d.SignWindowMs, "signing window ms")
		proposeMs := fs.Int64("proposems", d.ProposeDelayMs, "proposal delay ms")
		fallbackMs := fs.Int64("fallbackms", d.FallbackMs, "fallback proposer delay ms")
		quorum := fs.String("quorum", fmt.Sprintf("%d/%d", d.QuorumNum, d.QuorumDen), "quorum of active power")
		floor := fs.String("floor", fmt.Sprintf("%d/%d", d.FloorNum, d.FloorDen), "floor: active power must be at least this share of bonded power")
		clockMs := fs.Int64("clockms", d.MaxClockSkewMs, "max clock skew ms")
		activeEpochs := fs.Int64("activeepochs", d.ActiveEpochs, "epochs a validator stays active without messages")
		omissionMs := fs.Int64("omissionms", d.OmissionGraceMs, "omission grace ms")
		activeWindow := fs.Int64("activewindow", d.ActiveWindowEpochs, "S3 active-set window in epochs")
		activeLag := fs.Int64("activelag", d.ActiveLagEpochs, "S3 active-set window ends this many epochs back")
		activePct := fs.Int64("activepct", d.ActivePct, "S3 percent of window epochs a validator must have signed")
		activeMin := fs.Int64("activemin", -1, "S3 fewer counted epochs than this in the window: all validators active (default window/10)")
		// S4 bond rules (defaults: mesh.DefaultBondRules, proposed).
		db := mesh.DefaultBondRules
		s4 := fs.Bool("s4", false, "include S4 validator bond rules")
		ladder := func(a mesh.BondAsset) string {
			var p []string
			for _, st := range a.Steps {
				p = append(p, fmt.Sprintf("%d:%d", st.AfterSecs, st.Amount))
			}
			return strings.Join(p, ",")
		}
		bondBTC := fs.String("bondbtc", ladder(db.Assets[0]), "BTC bond ladder afterSecs:atoms,... ('' or 'off': no BTC bonds)")
		bondLBC := fs.String("bondlbc", ladder(db.Assets[1]), "LBC bond ladder")
		bondDCR := fs.String("bonddcr", ladder(db.Assets[2]), "DCR bond ladder")
		bondAge := fs.Int64("bondage", db.MinAgeSecs, "bond age (s) before it counts")
		bondMinLock := fs.Int64("bondminlock", db.MinLockSecs, "lock (s) a bond must have left at admission")
		bondLockLeft := fs.Int64("bondlockleft", db.MinLockLeftSecs, "a bond stops counting with less lock (s) left")
		bondConfs := fs.Uint("bondconfs", uint(db.MinConfs), "confirmations before a validator signs an admission")
		bondSwitch := fs.String("bondswitch", fmt.Sprintf("%d/%d", db.SwitchNum, db.SwitchDen), "share of seeded validators that turns the bond switch on")
		bondDeadline := fs.Int64("bonddeadline", 0, "unix s: bond switch on at this time without signals (0: none)")
		bondPower := fs.Uint64("bondpower", db.BondPower, "power of a bonded validator")
		bondMax := fs.Int("bondmaxclaims", db.MaxClaimsPerEpoch, "bond admissions per proposal")
		// S6 trading bond rules (defaults: mesh.DefaultUserBondRules, proposed).
		du := mesh.DefaultUserBondRules
		s6 := fs.Bool("s6", false, "include S6 trading bond rules (bonds required to trade)")
		ubondBTC := fs.String("ubondbtc", fmt.Sprint(du.Assets[0].TierAmount), "BTC atoms per trading tier ('' or 'off': no BTC)")
		ubondLBC := fs.String("ubondlbc", fmt.Sprint(du.Assets[1].TierAmount), "LBC atoms per trading tier")
		ubondDCR := fs.String("ubonddcr", fmt.Sprint(du.Assets[2].TierAmount), "DCR atoms per trading tier")
		ubondAge := fs.Int64("ubondage", du.MinAgeSecs, "trading bond age (s) before it counts")
		ubondMinLock := fs.Int64("ubondminlock", du.MinLockSecs, "lock (s) a trading bond must have left at admission")
		ubondLockLeft := fs.Int64("ubondlockleft", du.MinLockLeftSecs, "a trading bond stops counting with less lock (s) left")
		ubondConfs := fs.Uint("ubondconfs", uint(du.MinConfs), "confirmations before a validator signs a trading bond admission")
		ubondLots := fs.Uint64("ubondlots", du.LotsPerTier, "max lots per order per tier")
		ubondOrders := fs.Uint64("ubondorders", du.OrdersPerTier, "max resting orders per tier")
		ubondMaxTier := fs.Uint64("ubondmaxtier", du.MaxTier, "tier cap")
		ubondMax := fs.Int("ubondmaxclaims", du.MaxClaimsPerEpoch, "trading bond admissions per proposal")
		// S7 verified price history (defaults: mesh.DefaultPriceRules, proposed).
		dp := mesh.DefaultPriceRules
		s7 := fs.Bool("s7", false, "include S7 price history rules (price history only from trades verified on chain)")
		tradeConfs := fs.Uint("tradeconfs", uint(dp.MinConfs), "confirmations a validator needs on each swap contract")
		tradeMax := fs.Int("trademaxproofs", dp.MaxProofsPerEpoch, "trade admissions per proposal")
		tradeAge := fs.Int64("tradeage", dp.MaxAgeSecs, "s after the match within which a trade can be admitted")
		tradeLockLeft := fs.Int64("tradelockleft", dp.MinLockLeftSecs, "s of swap lock a proposer needs left to include a trade")
		out := fs.String("out", "release.json", "output file")
		_ = fs.Parse(os.Args[2:])
		r := &mesh.Release{Version: mesh.ReleaseVersion, Name: *name, Network: *network, Issued: time.Now().Unix(),
			Protocol: mesh.ProtocolVersion, MinProtocol: mesh.MinProtocolVersion, Params: d}
		r.Params.EpochMs, r.Params.SignWindowMs, r.Params.ProposeDelayMs, r.Params.FallbackMs = *epochMs, *windowMs, *proposeMs, *fallbackMs
		r.Params.QuorumNum, r.Params.QuorumDen = frac(*quorum)
		r.Params.FloorNum, r.Params.FloorDen = frac(*floor)
		r.Params.MaxClockSkewMs, r.Params.ActiveEpochs, r.Params.OmissionGraceMs = *clockMs, *activeEpochs, *omissionMs
		r.Params.ActiveWindowEpochs, r.Params.ActiveLagEpochs, r.Params.ActivePct = *activeWindow, *activeLag, *activePct
		r.Params.ActiveMinEpochs = *activeMin
		if *activeMin < 0 {
			r.Params.ActiveMinEpochs = max(1, *activeWindow/10)
		}
		if *s4 {
			br := &mesh.BondRules{MinAgeSecs: *bondAge, MinLockSecs: *bondMinLock, MinLockLeftSecs: *bondLockLeft,
				MinConfs: uint32(*bondConfs), BondPower: *bondPower, MaxClaimsPerEpoch: *bondMax, SwitchDeadline: *bondDeadline}
			br.SwitchNum, br.SwitchDen = frac(*bondSwitch)
			for _, a := range []struct {
				id  uint32
				sym string
				l   string
			}{{mesh.BondAssetBTC, "btc", *bondBTC}, {mesh.BondAssetLBC, "lbc", *bondLBC}, {mesh.BondAssetDCR, "dcr", *bondDCR}} {
				if a.l == "" || a.l == "off" {
					continue
				}
				ba := mesh.BondAsset{AssetID: a.id, Symbol: a.sym}
				for _, st := range strings.Split(a.l, ",") {
					kv := strings.Split(st, ":")
					if len(kv) != 2 {
						fatal("bad ladder step %q", st)
					}
					after, err1 := strconv.ParseInt(kv[0], 10, 64)
					amt, err2 := strconv.ParseUint(kv[1], 10, 64)
					if err1 != nil || err2 != nil {
						fatal("bad ladder step %q", st)
					}
					ba.Steps = append(ba.Steps, mesh.BondStep{AfterSecs: after, Amount: amt})
				}
				br.Assets = append(br.Assets, ba)
			}
			r.Bonds = br
		}
		if *s6 {
			ur := &mesh.UserBondRules{MinAgeSecs: *ubondAge, MinLockSecs: *ubondMinLock, MinLockLeftSecs: *ubondLockLeft,
				MinConfs: uint32(*ubondConfs), LotsPerTier: *ubondLots, OrdersPerTier: *ubondOrders, MaxTier: *ubondMaxTier,
				MaxClaimsPerEpoch: *ubondMax}
			for _, a := range []struct {
				id  uint32
				sym string
				v   string
			}{{mesh.BondAssetBTC, "btc", *ubondBTC}, {mesh.BondAssetLBC, "lbc", *ubondLBC}, {mesh.BondAssetDCR, "dcr", *ubondDCR}} {
				if a.v == "" || a.v == "off" {
					continue
				}
				amt, err := strconv.ParseUint(a.v, 10, 64)
				if err != nil {
					fatal("bad tier amount %q", a.v)
				}
				ur.Assets = append(ur.Assets, mesh.UserBondAsset{AssetID: a.id, Symbol: a.sym, TierAmount: amt})
			}
			r.UserBonds = ur
		}
		if *s7 {
			r.PriceHistory = &mesh.PriceRules{MinConfs: uint32(*tradeConfs), MaxProofsPerEpoch: *tradeMax, MaxAgeSecs: *tradeAge, MinLockLeftSecs: *tradeLockLeft}
		}
		ids := strings.Split(*vals, ",")
		nm := strings.Split(*names, ",")
		bs := strings.Split(*bonds, ",")
		for i, id := range ids {
			v := mesh.ReleaseValidator{PeerID: strings.TrimSpace(id), Bond: 1}
			if i < len(nm) && *names != "" {
				v.Name = nm[i]
			}
			if i < len(bs) && *bonds != "" {
				b, err := strconv.ParseUint(bs[i], 10, 64)
				if err != nil {
					fatal("bad bond %q", bs[i])
				}
				v.Bond = b
			}
			r.Validators = append(r.Validators, v)
		}
		b, err := mesh.SignRelease(r, loadKey(*key))
		if err != nil {
			fatal("sign: %v", err)
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fatal("write: %v", err)
		}
		fmt.Printf("wrote %s (%d validators)\n", *out, len(r.Validators))
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		pub := fs.String("pubkey", mesh.DevReleasePubKey, "trusted release key")
		_ = fs.Parse(os.Args[2:])
		r, err := mesh.LoadReleaseWithKey(fs.Arg(0), *pub)
		if err != nil {
			fatal("REJECTED: %v", err)
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		h := r.Hash()
		fmt.Printf("OK release %q hash %x\n%s\n", r.Name, h, b)
	default:
		fatal("unknown command %q", os.Args[1])
	}
}
