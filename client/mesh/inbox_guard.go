// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"crypto/sha256"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// Inbox defenses (P6): a replay cache and a per-sender rate limit.
//
// The stamp window in openInbox bounds replays to MaxInboxMsgAge, but within
// the window a mesh node (which sets the authenticated publisher) could
// re-deliver a captured message. Handlers are written to be idempotent, and
// the replay cache drops exact duplicates (same signature) before they reach
// them. The rate limit stops one sender from flooding our inbox with
// messages that each cost an ECDH, an AES-GCM open and a signature check.
const (
	// InboxRate and InboxBurst bound messages accepted per sender: a token
	// bucket refilled at InboxRate per second up to InboxBurst.
	InboxRate  = 20.0
	InboxBurst = 100.0
	// maxInboxSenders and maxInboxSeen cap the guard's memory.
	maxInboxSenders = 10000
	maxInboxSeen    = 100000
)

type tokenBucket struct {
	tokens float64
	last   time.Time
}

type inboxGuard struct {
	mtx     sync.Mutex
	buckets map[peer.ID]*tokenBucket
	seen    map[[32]byte]int64 // sig hash -> message stamp (ms)
	dropped uint64
}

func newInboxGuard() *inboxGuard {
	return &inboxGuard{buckets: make(map[peer.ID]*tokenBucket), seen: make(map[[32]byte]int64)}
}

// allow reports whether another message from sender may be processed now.
func (g *inboxGuard) allow(sender peer.ID, now time.Time) bool {
	g.mtx.Lock()
	defer g.mtx.Unlock()
	b := g.buckets[sender]
	if b == nil {
		if len(g.buckets) >= maxInboxSenders {
			for id, ob := range g.buckets { // drop idle senders
				if now.Sub(ob.last) > time.Minute {
					delete(g.buckets, id)
				}
			}
			if len(g.buckets) >= maxInboxSenders {
				g.dropped++
				return false
			}
		}
		b = &tokenBucket{tokens: InboxBurst, last: now}
		g.buckets[sender] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * InboxRate
	if b.tokens > InboxBurst {
		b.tokens = InboxBurst
	}
	b.last = now
	if b.tokens < 1 {
		g.dropped++
		return false
	}
	b.tokens--
	return true
}

// fresh records an authenticated message and reports whether it is the first
// time it was seen. Entries older than the accepted stamp window are pruned.
func (g *inboxGuard) fresh(sig []byte, stamp int64, now time.Time) bool {
	h := sha256.Sum256(sig)
	g.mtx.Lock()
	defer g.mtx.Unlock()
	if _, dup := g.seen[h]; dup {
		g.dropped++
		return false
	}
	if len(g.seen) >= maxInboxSeen/2 {
		cutoff := now.Add(-MaxInboxMsgAge - MaxClockSkew).UnixMilli()
		for k, s := range g.seen {
			if s < cutoff {
				delete(g.seen, k)
			}
		}
		if len(g.seen) >= maxInboxSeen {
			// Everything is in the window: refuse rather than forget.
			g.dropped++
			return false
		}
	}
	g.seen[h] = stamp
	return true
}

// InboxDropped is the number of inbox messages dropped by the replay cache
// and rate limit.
func (ob *OrderBooks) InboxDropped() uint64 {
	ob.guard.mtx.Lock()
	defer ob.guard.mtx.Unlock()
	return ob.guard.dropped
}

// receiveInbox is the inbox topic handler body: rate limit, open,
// de-duplicate, dispatch.
func (ob *OrderBooks) receiveInbox(data []byte, sender peer.ID) (*inboxMsg, error) {
	now := ob.now()
	if !ob.guard.allow(sender, now) {
		return nil, errRateLimited
	}
	msg, err := ob.openInbox(data, sender)
	if err != nil {
		return nil, err
	}
	if !ob.guard.fresh(msg.Sig, msg.Stamp, now) {
		return nil, errReplayed
	}
	return msg, nil
}
