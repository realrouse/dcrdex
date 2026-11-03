// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// bisonary-release signs and verifies bisonary release files (S0).
//
//	bisonary-release sign -key dev.key -name NAME -validators id1,id2,... [-names v1,v2,...] [-bonds 1,1,...]
//	    [-epochms 10000 -windowms 5000 -proposems 300 -fallbackms 1500 -quorum 2/3 -floor 1/2 -clockms 2000] -out release.json
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
