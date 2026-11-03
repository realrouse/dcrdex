// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import "testing"

// simnet2sParams are the epoch parameters of the S3-S8 simnet releases.
var simnet2sParams = EpochParams{EpochMs: 2000, SignWindowMs: 1000, ProposeDelayMs: 200, FallbackMs: 300,
	QuorumNum: 2, QuorumDen: 3, FloorNum: 1, FloorDen: 2, MaxClockSkewMs: 2000, ActiveEpochs: 3,
	ActiveWindowEpochs: 30, ActiveLagEpochs: 5, ActivePct: 80, ActiveMinEpochs: 10, OmissionGraceMs: 500, MaxOrdersPerEpoch: 500}

// TestEpochOneDown2s: with the simnet 2 s parameters and S8 re-gossip on or
// off, any one of 4 validators down leaves the other 3 finalizing (the
// skipped-epoch hold of the re-gossip must not cost epochs when every
// remaining signer is needed).
func TestEpochOneDown2s(t *testing.T) {
	for _, rg := range []bool{false, true} {
		for down := 0; down < 4; down++ {
			s := newEpochSim(t, 4, 2, simnet2sParams)
			for _, n := range s.all() {
				n.e.cfg.NoRegossip = !rg
			}
			s.epochs(40)
			s.net.setDown(s.vals[down].id, true)
			obs := s.vals[(down+1)%4].e
			from := lastEpoch(obs)
			s.epochs(20)
			fin := len(obs.Records(from+1, 1e6))
			t.Logf("re-gossip %v, v%d down: %d of 20 finalized", rg, down, fin)
			if fin < 18 {
				t.Fatalf("re-gossip %v, v%d down: only %d of 20 finalized", rg, down, fin)
			}
		}
	}
}
