// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// The bisoncraft/mesh codec limits a whole publish (topic, framing and
// data) to 1 KiB (codec.MaxMessageSize). Epoch proposals, records and sync
// batches are larger, so send splits them into fragments that fit, and the
// receiver reassembles them. Integrity comes from the fragment ID (a hash of
// the whole message) and, as for any epoch message, from the signatures
// inside it.
const (
	meshMaxPublish = 1024
	// meshPublishOverhead is a conservative allowance for the topic and
	// protobuf framing around the data.
	meshPublishOverhead = 160
	// epochFragChunk raw bytes per fragment (base64 in JSON: ~4/3).
	epochFragChunk = 480
	// epochMaxFrags bounds one reassembled message (~2 MB).
	epochMaxFrags = 4096
	// epochMaxFragBufs bounds the incomplete messages kept per peer.
	epochMaxFragBufs = 16
	// epochFragTTLMs drops incomplete messages.
	epochFragTTLMs = 60_000
	epMsgFrag      = "frag"
)

type epochFrag struct {
	ID   string `json:"id"` // hex sha256(whole)[:12]
	I    int    `json:"i"`
	N    int    `json:"n"`
	Data []byte `json:"d"`
}

type fragBuf struct {
	n     int
	got   int
	parts [][]byte
	first int64
}

func fitsPublish(topic string, data []byte) bool {
	return len(data)+len(topic)+meshPublishOverhead <= meshMaxPublish
}

// fragment returns b itself if it fits in one publish, or fragment messages.
func fragment(topic string, b []byte) ([][]byte, error) {
	if fitsPublish(topic, b) {
		return [][]byte{b}, nil
	}
	n := (len(b) + epochFragChunk - 1) / epochFragChunk
	if n > epochMaxFrags {
		return nil, fmt.Errorf("epoch message of %d bytes is too large", len(b))
	}
	h := sha256.Sum256(b)
	id := hex.EncodeToString(h[:12])
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		end := (i + 1) * epochFragChunk
		if end > len(b) {
			end = len(b)
		}
		fb, err := json.Marshal(&epochMsg{Type: epMsgFrag, Frag: &epochFrag{ID: id, I: i, N: n, Data: b[i*epochFragChunk : end]}})
		if err != nil {
			return nil, err
		}
		if !fitsPublish(topic, fb) {
			return nil, fmt.Errorf("epoch fragment of %d bytes does not fit a mesh publish", len(fb))
		}
		out = append(out, fb)
	}
	return out, nil
}

// addFragLocked stores a fragment from peer and returns the whole message
// when all fragments are in.
func (e *EpochEngine) addFragLocked(from string, f *epochFrag, nowMs int64) []byte {
	if f.N < 2 || f.N > epochMaxFrags || f.I < 0 || f.I >= f.N || len(f.Data) == 0 || len(f.Data) > epochFragChunk || len(f.ID) != 24 {
		return nil
	}
	key := from + "/" + f.ID
	fb := e.frags[key]
	if fb == nil {
		// Bound buffers per peer: drop that peer's oldest.
		var mine []string
		for k := range e.frags {
			if len(k) > len(from) && k[:len(from)] == from && k[len(from)] == '/' {
				mine = append(mine, k)
			}
		}
		if len(mine) >= epochMaxFragBufs {
			sort.Slice(mine, func(i, j int) bool { return e.frags[mine[i]].first < e.frags[mine[j]].first })
			delete(e.frags, mine[0])
		}
		fb = &fragBuf{n: f.N, parts: make([][]byte, f.N), first: nowMs}
		e.frags[key] = fb
	}
	if fb.n != f.N || fb.parts[f.I] != nil {
		return nil
	}
	fb.parts[f.I] = f.Data
	fb.got++
	if fb.got < fb.n {
		return nil
	}
	delete(e.frags, key)
	whole := bytes.Join(fb.parts, nil)
	h := sha256.Sum256(whole)
	if hex.EncodeToString(h[:12]) != f.ID {
		e.log.Debugf("Dropping epoch message from %s: fragment hash mismatch", from)
		return nil
	}
	return whole
}

func (e *EpochEngine) expireFragsLocked(nowMs int64) {
	for k, fb := range e.frags {
		if nowMs-fb.first > epochFragTTLMs {
			delete(e.frags, k)
		}
	}
}
