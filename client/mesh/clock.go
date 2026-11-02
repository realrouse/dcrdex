// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S0: clock checking. Epoch consensus needs the nodes' clocks to agree to
// within EpochParams.MaxClockSkewMs. Each node estimates its own offset from
// the mesh from the stamps of signed validator messages (heartbeats and
// attestations): offset sample = their stamp - our receive time. Our skew is
// minus the median over peers of each peer's median sample. If an SNTP server
// is configured its measurement is used instead.

import (
	"encoding/binary"
	"errors"
	"net"
	"sort"
	"sync"
	"time"
)

const clockSamplesPerPeer = 9

type clockEstimator struct {
	mtx     sync.Mutex
	samples map[string][]int64 // peer -> recent (their stamp - our time) ms
	seen    map[string]int64   // peer -> our time of last sample
	ntp     *int64             // SNTP measured skew (our clock - true), ms
	ntpAt   int64
}

func newClockEstimator() *clockEstimator {
	return &clockEstimator{samples: make(map[string][]int64), seen: make(map[string]int64)}
}

// add records a stamp from peer received at our local time nowMs.
func (c *clockEstimator) add(peer string, stamp, nowMs int64) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	s := append(c.samples[peer], stamp-nowMs)
	if len(s) > clockSamplesPerPeer {
		s = s[len(s)-clockSamplesPerPeer:]
	}
	c.samples[peer] = s
	c.seen[peer] = nowMs
}

func median(v []int64) int64 {
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// skew is our estimated clock error in ms (positive: our clock is ahead) and
// the number of peers it is based on. Peers not heard from within maxAgeMs
// are ignored. SNTP takes precedence if measured within the last 30 min.
func (c *clockEstimator) skew(nowMs, maxAgeMs int64) (skewMs int64, peers int, source string) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.ntp != nil && nowMs-c.ntpAt < 30*60*1000 {
		return *c.ntp, 1, "ntp"
	}
	var meds []int64
	for p, s := range c.samples {
		if nowMs-c.seen[p] > maxAgeMs || len(s) == 0 {
			continue
		}
		meds = append(meds, median(s))
	}
	if len(meds) == 0 {
		return 0, 0, "none"
	}
	return -median(meds), len(meds), "peers"
}

func (c *clockEstimator) setNTP(skewMs, nowMs int64) {
	c.mtx.Lock()
	c.ntp, c.ntpAt = &skewMs, nowMs
	c.mtx.Unlock()
}

// sntpSkew queries an SNTP server (host:port) and returns our clock error in
// ms (positive: local clock ahead). now is the local clock.
func sntpSkew(server string, now func() time.Time) (int64, error) {
	conn, err := net.DialTimeout("udp", server, 3*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	req := make([]byte, 48)
	req[0] = 0x1b // LI 0, version 3, client mode
	t1 := now()
	if _, err := conn.Write(req); err != nil {
		return 0, err
	}
	resp := make([]byte, 48)
	if _, err := conn.Read(resp); err != nil {
		return 0, err
	}
	t4 := now()
	ntpTime := func(b []byte) time.Time {
		const ntpEpochOffset = 2208988800
		sec := binary.BigEndian.Uint32(b[0:4])
		frac := binary.BigEndian.Uint32(b[4:8])
		ns := (int64(frac) * 1e9) >> 32
		return time.Unix(int64(sec)-ntpEpochOffset, ns)
	}
	if resp[0]&0x7 != 4 && resp[0]&0x7 != 5 {
		return 0, errors.New("not an SNTP server response")
	}
	t2, t3 := ntpTime(resp[32:40]), ntpTime(resp[40:48])
	// offset = ((t2 - t1) + (t3 - t4)) / 2 is (true - local).
	off := (t2.Sub(t1) + t3.Sub(t4)) / 2
	return -off.Milliseconds(), nil
}
