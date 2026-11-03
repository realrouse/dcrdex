// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S1: the epoch engine. Validators (bisonw run with a release that lists
// their mesh peer ID, and validator mode on) take turns proposing the
// orders of each epoch, check each other's proposals, run the deterministic
// matching (epoch_state.go) and sign the result. A record is final when the
// signers hold at least the quorum of the active validators' power and more
// than the floor of all bonded power. Traders run the same engine without
// signing: they submit orders, follow the chain of records and start swaps
// only for finalized matches (S2, negotiate.go). An epoch that does not
// finalize is skipped; its orders stay in the pool and roll into the next
// proposal.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	mc "github.com/bisoncraft/mesh/client"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
)

const (
	maxRecordsInMemory = 100_000
	maxSyncChunk       = 50
	maxEpochFlags      = 100
)

type poolOrder struct {
	ord  *MeshOrder
	id   OrderID
	recv int64 // local ms
}

type poolCancel struct {
	c    *MeshCancel
	recv int64
}

// epochResult is a proposal applied to the finalized state.
type epochResult struct {
	state   *epochState
	matches []*EpochMatch
	fp      EpochHash
	makers  map[OrderID]*MeshOrder // maker orders of the matches
}

type epochCandidate struct {
	prop *EpochProposal
	hash EpochHash
	recv int64
	res  *epochResult // nil until Prev is our last finalized fingerprint
	bad  string       // S3: wrong active set or bad credits (never signed)
}

// EpochFinal is passed to the finalization callback.
type EpochFinal struct {
	Record *EpochRecord
	// Makers are the maker orders of the record's matches.
	Makers map[OrderID]*MeshOrder
	// Synced is true for records received by catch-up sync or loaded from
	// disk rather than finalized live.
	Synced bool
}

// EpochFlag is a misbehavior flag (proposer omission, equivocation,
// conflicting record, state divergence).
type EpochFlag struct {
	Epoch  uint64 `json:"epoch"`
	Kind   string `json:"kind"`
	Peer   string `json:"peer"`
	Detail string `json:"detail"`
	Stamp  int64  `json:"stamp"`
}

// EpochConfig is the local (not network-wide) epoch engine configuration.
type EpochConfig struct {
	// Validator: sign and propose if our peer ID is in the release.
	Validator bool
	// StorePath is the file finalized records are appended to ("": none).
	StorePath string
	// NTPServer (host:port) is optional; the clock is otherwise checked
	// against the validators' signed stamps.
	NTPServer string
	// TickMs is the engine's timer resolution (default 50).
	TickMs int64
	// SkipStartListen (in-memory tests only) signs right after start,
	// without the S5 listen period for another instance of our key.
	SkipStartListen bool
}

// EpochEngine runs epoch consensus for one node.
type EpochEngine struct {
	rel     *Release
	p       EpochParams
	cfg     EpochConfig
	ps      pubSub
	priv    libp2pcrypto.PrivKey
	self    string
	log     dex.Logger
	now     func() time.Time
	genesis EpochHash
	signer  bool // validator mode and in the release
	clock   *clockEstimator
	onFinal func(*EpochFinal)

	mtx       sync.Mutex
	state     *epochState
	last      *EpochRecord
	lastFP    EpochHash
	records   []*EpochRecord
	byEpoch   map[uint64]*EpochRecord
	pool      map[OrderID]*poolOrder
	cpool     map[OrderID]*poolCancel
	cands     map[uint64]map[EpochHash]*epochCandidate
	atts      map[uint64]map[EpochHash]map[string]*EpochAttestation
	signed    map[uint64]EpochHash
	proposed  map[uint64]bool
	heard     map[string]int64
	hbSent    uint64
	flags     []*EpochFlag
	omissions map[string]int
	propBy    map[string]EpochHash // proposer|epoch -> proposal hash (equivocation)
	finalized int
	skipped   uint64
	started   uint64
	lastFinAt int64
	clockBad  bool
	syncAt    int64
	recvAt    int64 // last epoch-topic message from another peer
	resubAt   int64 // last epoch-topic resubscribe
	served    map[string]int64
	frags     map[string]*fragBuf // incomplete fragmented messages (epoch_frag.go)
	store     *os.File
	// S3 active set (epoch_active.go)
	chainStart    int                     // index in records of the current chain's first record
	lastCredits   map[string]*EpochCredit // known signatures of the last record
	activeCache   *ActiveSet
	activeCacheFP EpochHash
	// S4 bonds (bonds.go, epoch_bonds.go)
	ledger      *bondLedger
	valsCache   *valSet
	valsCacheFP EpochHash
	sigPool     map[string]*SwitchSignal
	bondPool    map[string]*bondPoolEntry
	ownClaims   map[string]*BondClaim
	ownSignal   *SwitchSignal
	latched     bool
	latchSigs   []*SwitchSignal
	bondChain   BondChain
	bondMaker   BondMaker
	bondIndex   uint32
	s4SentAt    uint64
	peerNames   map[string]string
	// S5 equivocation proofs and bans (equivocation.go)
	proofs      map[string]*proofEntry // verified proofs we hold, by validator
	firstSigs   map[string]*firstSig   // validator|epoch -> first signature seen
	penalty     PenaltyHook
	badProofs   int
	guardEpoch  uint64
	guardFP     EpochHash
	startedAt   int64
	sentMsgs    map[string]int64 // our own signed messages (key -> stamp)
	dupAt       int64            // last message from another instance of our key
	dupFlagAt   int64
	dupCount    int
	credChecks  int // peers' proposals on our tip whose credits we checked since start
	credCheckAt uint64
	s5SentAt    uint64
	// debug (simnet adversary tests)
	omitOwner    string
	equivocate   int
	equivocated  int
	forgeAgainst string
	forgeMode    string
}

// NewEpochEngine creates an engine. Call Run to start it.
func NewEpochEngine(rel *Release, cfg EpochConfig, ps pubSub, priv libp2pcrypto.PrivKey, now func() time.Time, log dex.Logger) (*EpochEngine, error) {
	pid, _, err := ownerOf(priv)
	if err != nil {
		return nil, err
	}
	if cfg.TickMs <= 0 {
		cfg.TickMs = 50
	}
	if now == nil {
		now = time.Now
	}
	e := &EpochEngine{
		rel: rel, p: rel.Params, cfg: cfg, ps: ps, priv: priv, self: pid.String(), log: log, now: now,
		genesis:     GenesisHash(rel),
		clock:       newClockEstimator(),
		state:       newEpochState(),
		byEpoch:     make(map[uint64]*EpochRecord),
		pool:        make(map[OrderID]*poolOrder),
		cpool:       make(map[OrderID]*poolCancel),
		cands:       make(map[uint64]map[EpochHash]*epochCandidate),
		atts:        make(map[uint64]map[EpochHash]map[string]*EpochAttestation),
		signed:      make(map[uint64]EpochHash),
		proposed:    make(map[uint64]bool),
		heard:       make(map[string]int64),
		omissions:   make(map[string]int),
		propBy:      make(map[string]EpochHash),
		served:      make(map[string]int64),
		frags:       make(map[string]*fragBuf),
		lastCredits: make(map[string]*EpochCredit),
		ledger:      newBondLedger(rel),
		sigPool:     make(map[string]*SwitchSignal),
		bondPool:    make(map[string]*bondPoolEntry),
		ownClaims:   make(map[string]*BondClaim),
		peerNames:   make(map[string]string),
		proofs:      make(map[string]*proofEntry),
		firstSigs:   make(map[string]*firstSig),
		sentMsgs:    make(map[string]int64),
	}
	e.lastFP = e.genesis
	// S4: with bond rules a node that is not seeded can become a validator
	// by bonding; it signs only while it is in an epoch's validator set.
	e.signer = cfg.Validator && (rel.IsValidator(e.self) || rel.Bonds != nil)
	if cfg.Validator && !e.signer {
		log.Warnf("Validator mode requested but our peer ID %s is not in release %q; running as a client", e.self, rel.Name)
	} else if e.signer && !rel.IsValidator(e.self) {
		log.Infof("Validator mode: not seeded in release %q; we sign once a bond of ours counts (S4)", rel.Name)
	}
	e.loadS5Files()
	if cfg.StorePath != "" {
		if err := e.loadStore(); err != nil {
			return nil, err
		}
	}
	e.loadBondFiles()
	return e, nil
}

// SetFinalHandler sets the callback for finalized records.
func (e *EpochEngine) SetFinalHandler(f func(*EpochFinal)) { e.onFinal = f }

// Release is the engine's release.
func (e *EpochEngine) Release() *Release { return e.rel }

// IsSigner is true for a validator that signs.
func (e *EpochEngine) IsSigner() bool { return e.signer }

func (e *EpochEngine) nowMs() int64 { return e.now().UnixMilli() }

// Run starts the engine and runs its timer loop until ctx ends.
func (e *EpochEngine) Run(ctx context.Context) error {
	e.start(ctx)
	tick := time.NewTicker(time.Duration(e.cfg.TickMs) * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			e.tick(ctx)
		case <-ctx.Done():
			if e.store != nil {
				e.store.Close()
			}
			return nil
		}
	}
}

// start subscribes to the epoch topic and asks for the record chain.
func (e *EpochEngine) start(ctx context.Context) {
	e.mtx.Lock()
	e.startedAt = e.nowMs()
	e.mtx.Unlock()
	e.mtx.Lock()
	e.recvAt = e.nowMs()
	e.mtx.Unlock()
	e.subscribe(ctx)
	e.mtx.Lock()
	e.started = e.p.EpochOf(e.nowMs())
	e.mtx.Unlock()
	role := "client"
	if e.signer {
		role = "validator"
	}
	e.log.Infof("Epoch consensus started as %s: release %q, %d validators, epoch %d ms, window %d ms, quorum %d/%d, floor %d/%d, clock limit %d ms",
		role, e.rel.Name, len(e.rel.Validators), e.p.EpochMs, e.p.SignWindowMs, e.p.QuorumNum, e.p.QuorumDen, e.p.FloorNum, e.p.FloorDen, e.p.MaxClockSkewMs)
	go e.requestSync(ctx, true)
	if e.cfg.NTPServer != "" {
		go e.ntpLoop(ctx)
	}
}

// subscribe registers the epoch topic handler.
func (e *EpochEngine) subscribe(ctx context.Context) {
	err := e.ps.Subscribe(ctx, EpochTopic, func(ev mc.TopicEvent) {
		if ev.Type != mc.TopicEventData {
			if ev.Type == mc.TopicEventPeerSubscribed {
				go e.requestSync(ctx, true)
			}
			return
		}
		if ev.Peer.String() == e.self {
			if e.signer {
				e.ownKeyMsg(ev.Data) // S5: another instance of our key?
			}
			return
		}
		e.mtx.Lock()
		e.recvAt = e.nowMs()
		e.mtx.Unlock()
		e.handleMsg(ctx, ev.Peer.String(), ev.Data)
	})
	if err != nil && !errors.Is(err, mc.ErrRedundantSubscription) {
		e.log.Warnf("Error subscribing to %s: %v", EpochTopic, err)
	}
}

// epochResubscribeMs is the minimum quiet time on the epoch topic before the
// subscription is renewed (see maybeResubscribe).
func (e *EpochEngine) epochResubscribeMs() int64 {
	if ms := 5 * e.p.EpochMs; ms > 30_000 {
		return ms
	}
	return 30_000
}

// maybeResubscribe renews the epoch topic subscription when nothing arrived
// on it for a while. A subscription made while the mesh connection is still
// being set up can be lost by the mesh node (the client registers it, but it
// never reaches the node), leaving a restarted validator deaf to the chain:
// it cannot sync, sign or finalize, though its own broadcasts still go out.
// Validators heartbeat every epoch, so a quiet topic means we are cut off.
func (e *EpochEngine) maybeResubscribe(ctx context.Context, nowMs int64) {
	gap := e.epochResubscribeMs()
	e.mtx.Lock()
	if nowMs-e.recvAt < gap || nowMs-e.resubAt < gap {
		e.mtx.Unlock()
		return
	}
	e.resubAt = nowMs
	quiet := nowMs - e.recvAt
	e.mtx.Unlock()
	type unsubscriber interface {
		Unsubscribe(ctx context.Context, topic string) error
	}
	u, ok := e.ps.(unsubscriber)
	if !ok {
		return
	}
	e.log.Infof("No epoch messages for %d s; renewing the %s subscription", quiet/1000, EpochTopic)
	go func() {
		if err := u.Unsubscribe(ctx, EpochTopic); err != nil {
			e.log.Debugf("Error unsubscribing from %s: %v", EpochTopic, err)
		}
		e.subscribe(ctx)
		e.requestSync(ctx, true)
	}()
}

func (e *EpochEngine) ntpLoop(ctx context.Context) {
	for {
		if skew, err := sntpSkew(e.cfg.NTPServer, e.now); err != nil {
			e.log.Warnf("SNTP %s: %v", e.cfg.NTPServer, err)
		} else {
			e.clock.setNTP(skew, e.nowMs())
		}
		select {
		case <-time.After(10 * time.Minute):
		case <-ctx.Done():
			return
		}
	}
}

func (e *EpochEngine) send(ctx context.Context, m *epochMsg) {
	b, err := json.Marshal(m)
	if err != nil {
		e.log.Errorf("Error encoding epoch %s: %v", m.Type, err)
		return
	}
	parts, err := fragment(EpochTopic, b)
	if err != nil {
		e.log.Errorf("Error sending epoch %s: %v", m.Type, err)
		return
	}
	for _, part := range parts {
		if err := e.ps.Broadcast(ctx, EpochTopic, part); err != nil {
			e.log.Debugf("Error broadcasting epoch %s: %v", m.Type, err)
			return
		}
	}
}

// ClockSkew is our estimated clock error in ms, the number of peers it is
// based on (or 1 for SNTP), and whether it is within the limit.
func (e *EpochEngine) ClockSkew() (skewMs int64, peers int, ok bool) {
	skew, n, _ := e.clock.skew(e.nowMs(), e.p.ActiveEpochs*e.p.EpochMs)
	return skew, n, abs64(skew) <= e.p.MaxClockSkewMs
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (e *EpochEngine) clockOK() bool {
	skew, _, ok := e.ClockSkew()
	e.mtx.Lock()
	changed := ok == e.clockBad
	e.clockBad = !ok
	e.mtx.Unlock()
	if changed {
		if ok {
			e.log.Infof("Clock skew %d ms is back within the %d ms limit; signing resumed", skew, e.p.MaxClockSkewMs)
		} else {
			e.log.Warnf("CLOCK SKEW: our clock is %d ms off the mesh (limit %d ms). Not signing orders, proposals or attestations until the clock is fixed.", skew, e.p.MaxClockSkewMs)
		}
	}
	return ok
}

// SubmitOrder adds an own order to the pool and broadcasts it.
func (e *EpochEngine) SubmitOrder(ctx context.Context, o *MeshOrder) error {
	if !e.clockOK() {
		skew, _, _ := e.ClockSkew()
		return fmt.Errorf("clock is %d ms off the mesh (limit %d ms): fix the system clock before trading", skew, e.p.MaxClockSkewMs)
	}
	if err := e.admitOrder(o); err != nil {
		return err
	}
	e.send(ctx, &epochMsg{Type: epMsgOrder, Order: o})
	return nil
}

// SubmitCancel adds an own cancel to the pool and broadcasts it.
func (e *EpochEngine) SubmitCancel(ctx context.Context, c *MeshCancel) {
	e.mtx.Lock()
	e.cpool[c.OrderID] = &poolCancel{c: c, recv: e.nowMs()}
	e.mtx.Unlock()
	e.send(ctx, &epochMsg{Type: epMsgCancel, Cancel: c})
}

func (e *EpochEngine) admitOrder(o *MeshOrder) error {
	now := e.now()
	nowMs := now.UnixMilli()
	if err := o.Validate(now); err != nil {
		return err
	}
	if abs64(o.Stamp-nowMs) > e.p.MaxClockSkewMs {
		return fmt.Errorf("order stamp is %d ms off our clock (limit %d ms)", o.Stamp-nowMs, e.p.MaxClockSkewMs)
	}
	id := o.ID()
	e.mtx.Lock()
	defer e.mtx.Unlock()
	if _, seen := e.state.seen[id]; seen {
		return nil
	}
	if _, found := e.pool[id]; !found {
		e.pool[id] = &poolOrder{ord: o, id: id, recv: nowMs}
	}
	return nil
}

func (e *EpochEngine) handleMsg(ctx context.Context, from string, b []byte) {
	var m epochMsg
	if err := json.Unmarshal(b, &m); err != nil {
		return
	}
	if m.Type == epMsgFrag {
		if m.Frag == nil {
			return
		}
		e.mtx.Lock()
		whole := e.addFragLocked(from, m.Frag, e.nowMs())
		e.mtx.Unlock()
		if whole == nil {
			return
		}
		m = epochMsg{}
		if err := json.Unmarshal(whole, &m); err != nil || m.Type == epMsgFrag {
			return
		}
	}
	switch m.Type {
	case epMsgOrder:
		if m.Order != nil {
			if err := e.admitOrder(m.Order); err != nil {
				e.log.Debugf("Epoch pool rejected order from %s: %v", m.Order.Owner, err)
			}
		}
	case epMsgCancel:
		if m.Cancel != nil && m.Cancel.Validate(e.now()) == nil {
			e.mtx.Lock()
			e.cpool[m.Cancel.OrderID] = &poolCancel{c: m.Cancel, recv: e.nowMs()}
			e.mtx.Unlock()
		}
	case epMsgHB:
		if h := m.HB; h != nil && e.isValidatorNow(h.Validator) && verifySig(h.Validator, h.PubKey, h.msg(), h.Sig) == nil {
			e.heardFrom(h.Validator, h.Stamp)
		}
	case epMsgSignal:
		if m.Signal != nil {
			e.handleSignal(m.Signal)
		}
	case epMsgBond:
		if m.Bond != nil {
			e.mtx.Lock()
			e.addClaimLocked(m.Bond)
			e.mtx.Unlock()
		}
	case epMsgProof:
		if m.Proof != nil {
			e.handleProof(from, m.Proof)
		}
	case epMsgProp:
		if m.Prop != nil {
			e.handleProposal(ctx, m.Prop)
		}
	case epMsgAtt:
		if m.Att != nil {
			e.handleAttestation(ctx, m.Att)
		}
	case epMsgRec:
		if m.Rec != nil {
			e.handleRecord(ctx, m.Rec, false)
		}
	case epMsgSyncReq:
		e.serveSync(ctx, m.To, m.SyncFrom)
	case epMsgRecs:
		if m.To != "" && m.To != e.self {
			return
		}
		for _, r := range m.Recs {
			e.handleRecord(ctx, r, true)
		}
		if len(m.Recs) == maxSyncChunk {
			go e.requestSync(ctx, true)
		}
	}
}

// isValidatorNow: id is in the validator set of the current epoch.
func (e *EpochEngine) isValidatorNow(id string) bool {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	return e.valsLocked(e.p.EpochOf(e.nowMs())).IsValidator(id)
}

func (e *EpochEngine) heardFrom(v string, stamp int64) {
	nowMs := e.nowMs()
	e.clock.add(v, stamp, nowMs)
	e.mtx.Lock()
	e.heard[v] = nowMs
	e.mtx.Unlock()
}

// tick runs heartbeats, proposals, sync and cleanup.
func (e *EpochEngine) tick(ctx context.Context) {
	nowMs := e.nowMs()
	cur := e.p.EpochOf(nowMs)
	e.s4Gossip(ctx, cur)
	e.s5Gossip(ctx, cur)
	e.bondChecks(ctx, nowMs)
	e.mtx.Lock()
	member := e.isMemberLocked(cur)
	var retry []*epochCandidate
	if member && cur > 0 {
		// S4: a proposal we could not sign yet (bond not verified) is
		// retried within its window.
		for _, c := range e.cands[cur-1] {
			if c.res != nil && (len(c.prop.Bonds) > 0 || e.latched) && nowMs <= e.p.EpochEnd(cur-1)+e.p.SignWindowMs {
				if _, did := e.signed[cur-1]; !did {
					retry = append(retry, c)
				}
			}
		}
	}
	e.mtx.Unlock()
	for _, c := range retry {
		e.considerSign(ctx, c)
		e.tryFinalize(ctx, c.prop.Epoch)
	}
	if member {
		e.mtx.Lock()
		sendHB := e.hbSent != cur
		e.hbSent = cur
		e.mtx.Unlock()
		if sendHB {
			h := &EpochHeartbeat{Epoch: cur, Validator: e.self, Stamp: nowMs}
			pid, pubB, _ := ownerOf(e.priv)
			h.Validator, h.PubKey = pid.String(), pubB
			if sig, err := e.priv.Sign(h.msg()); err == nil {
				h.Sig = sig
				e.mtx.Lock()
				e.noteSentLocked(hbKey(h.Stamp), h.Stamp)
				e.mtx.Unlock()
				e.send(ctx, &epochMsg{Type: epMsgHB, HB: h})
			}
		}
		if cur > 0 {
			e.maybePropose(ctx, cur-1, nowMs)
		}
	}
	e.mtx.Lock()
	var lastEpoch uint64
	if e.last != nil {
		lastEpoch = e.last.Epoch
	}
	stale := e.last == nil || cur > lastEpoch+3
	needSync := stale && nowMs-e.syncAt > e.p.EpochMs*2
	if e.signer || stale {
		defer e.maybeResubscribe(ctx, nowMs)
	}
	for n := range e.cands {
		if n+10 < cur {
			delete(e.cands, n)
			delete(e.atts, n)
		}
	}
	for n := range e.signed {
		if n+100 < cur {
			delete(e.signed, n)
			delete(e.proposed, n)
		}
	}
	for id, po := range e.pool {
		if po.ord.Expiry <= nowMs {
			delete(e.pool, id)
		}
	}
	e.expireFragsLocked(nowMs)
	for id, pc := range e.cpool {
		if nowMs-pc.recv > MaxOrderLifetime.Milliseconds() {
			delete(e.cpool, id)
		}
	}
	e.mtx.Unlock()
	if needSync {
		e.requestSync(ctx, false)
	}
}

func (e *EpochEngine) lastEpochLocked() (uint64, bool) {
	if e.last == nil {
		return 0, false
	}
	return e.last.Epoch, true
}

func (e *EpochEngine) maybePropose(ctx context.Context, n uint64, nowMs int64) {
	end := e.p.EpochEnd(n)
	if nowMs < end+e.p.ProposeDelayMs || nowMs > end+e.p.SignWindowMs-e.p.EpochMs/50 {
		return
	}
	e.mtx.Lock()
	// S3: the proposer rotates over the epoch's active set.
	active := e.activeForLocked(n)
	rank := ProposerRankIn(active.Active, n, e.self)
	if rank < 0 || nowMs < end+e.p.ProposeDelayMs+int64(rank)*e.p.FallbackMs {
		e.mtx.Unlock()
		return
	}
	if le, ok := e.lastEpochLocked(); e.proposed[n] || (ok && n <= le) {
		e.mtx.Unlock()
		return
	}
	if e.proposeHoldLocked(nowMs) != "" { // S5: not while our key may run twice
		e.mtx.Unlock()
		return
	}
	if _, did := e.signed[n]; did {
		e.mtx.Unlock()
		return
	}
	// A backup proposer stays quiet if a lower-ranked proposal arrived,
	// unless we found that proposal invalid (S5: e.g. a forged proof), so a
	// bad proposal cannot cost the network its epoch.
	for _, c := range e.cands[n] {
		if c.prop.Rank < rank && c.bad == "" {
			e.mtx.Unlock()
			return
		}
	}
	e.proposed[n] = true
	e.mtx.Unlock()
	if !e.clockOK() {
		return
	}
	e.mtx.Lock()
	active = e.activeForLocked(n) // our chain may have moved while unlocked
	if rank = ProposerRankIn(active.Active, n, e.self); rank < 0 {
		e.mtx.Unlock()
		return
	}
	prop := &EpochProposal{Epoch: n, Prev: e.lastFP, Rank: rank, Active: active.Active, Credits: e.creditsLocked(), Stamp: nowMs}
	var ords []*poolOrder
	for _, po := range e.pool {
		if _, seen := e.state.seen[po.id]; seen || po.ord.Stamp >= end || po.ord.Expiry <= end {
			continue
		}
		if e.omitOwner != "" && po.ord.Owner == e.omitOwner {
			continue
		}
		ords = append(ords, po)
	}
	sort.Slice(ords, func(i, j int) bool {
		if ords[i].ord.Stamp != ords[j].ord.Stamp {
			return ords[i].ord.Stamp < ords[j].ord.Stamp
		}
		return ords[i].id.String() < ords[j].id.String()
	})
	if len(ords) > e.p.MaxOrdersPerEpoch {
		ords = ords[:e.p.MaxOrdersPerEpoch]
	}
	for _, po := range ords {
		prop.Orders = append(prop.Orders, po.ord)
	}
	var cids []OrderID
	for id, pc := range e.cpool {
		if _, resting := e.state.resting[id]; resting && pc.c.Stamp < end {
			cids = append(cids, id)
		}
	}
	sort.Slice(cids, func(i, j int) bool { return cids[i].String() < cids[j].String() })
	for _, id := range cids {
		if len(prop.Cancels) >= e.p.MaxOrdersPerEpoch {
			break
		}
		prop.Cancels = append(prop.Cancels, e.cpool[id].c)
	}
	e.s4ProposalLocked(prop)
	e.s5ProposalLocked(prop)
	e.noteSentLocked(propKey(prop.Epoch, prop.Stamp), prop.Stamp)
	e.mtx.Unlock()
	if err := prop.sign(e.priv); err != nil {
		e.log.Errorf("Error signing epoch proposal: %v", err)
		return
	}
	if len(prop.Signals) > 0 || len(prop.Bonds) > 0 {
		e.log.Infof("Proposing epoch %d with %d bond switch signal(s) and %d bond admission(s)", n, len(prop.Signals), len(prop.Bonds))
	}
	e.log.Debugf("Proposing epoch %d (rank %d): %d orders, %d cancels, %d active", n, rank, len(prop.Orders), len(prop.Cancels), len(prop.Active))
	e.send(ctx, &epochMsg{Type: epMsgProp, Prop: prop})
	e.handleProposal(ctx, prop)
}

func (e *EpochEngine) addFlagLocked(f *EpochFlag) {
	f.Stamp = e.nowMs()
	e.flags = append(e.flags, f)
	if len(e.flags) > maxEpochFlags {
		e.flags = e.flags[len(e.flags)-maxEpochFlags:]
	}
	e.log.Warnf("EPOCH FLAG %s: epoch %d peer %s: %s", f.Kind, f.Epoch, e.rel.ValidatorName(f.Peer), f.Detail)
}

// computeLocked applies a candidate to a copy of the finalized state.
func (e *EpochEngine) computeLocked(c *epochCandidate) {
	if c.res != nil || c.bad != "" || c.prop.Prev != e.lastFP {
		return
	}
	if err := c.prop.verify(e.valsLocked(c.prop.Epoch)); err != nil {
		c.bad = err.Error()
		e.addFlagLocked(&EpochFlag{Epoch: c.prop.Epoch, Kind: "proposal", Peer: c.prop.Proposer, Detail: c.bad})
		return
	}
	if err := e.checkActiveSetLocked(c.prop); err != nil {
		c.bad = err.Error()
		e.addFlagLocked(&EpochFlag{Epoch: c.prop.Epoch, Kind: "active-set", Peer: c.prop.Proposer, Detail: c.bad})
		return
	}
	if err := e.checkS4Locked(c.prop); err != nil {
		c.bad = err.Error()
		e.addFlagLocked(&EpochFlag{Epoch: c.prop.Epoch, Kind: "bonds", Peer: c.prop.Proposer, Detail: c.bad})
		return
	}
	if err := e.checkS5Locked(c.prop); err != nil {
		c.bad = err.Error()
		e.addFlagLocked(&EpochFlag{Epoch: c.prop.Epoch, Kind: "bad-proof", Peer: c.prop.Proposer, Detail: c.bad})
		return
	}
	st := e.state.clone()
	makers := make(map[OrderID]*MeshOrder)
	// Record maker orders before applying (fills may remove them).
	resting := make(map[OrderID]*MeshOrder, len(st.resting))
	for id, r := range st.resting {
		resting[id] = r.ord
	}
	for _, o := range c.prop.Orders {
		resting[o.ID()] = o
	}
	matches := applyEpoch(st, c.prop, &e.p)
	for _, m := range matches {
		makers[m.MakerOrderID] = resting[m.MakerOrderID]
	}
	sh := st.hash()
	c.res = &epochResult{state: st, matches: matches, makers: makers,
		fp: epochFingerprint(c.prop.Epoch, c.prop.Prev, c.hash, c.prop.Active, matches, sh)}
}

func (e *EpochEngine) handleProposal(ctx context.Context, p *EpochProposal) {
	// Membership is checked against our chain in computeLocked; here only
	// the signature and that the proposer is a validator of the epoch on
	// our chain (or seeded, if we are behind).
	e.mtx.Lock()
	vs := e.valsLocked(p.Epoch)
	e.mtx.Unlock()
	if !vs.IsValidator(p.Proposer) && (!e.rel.IsValidator(p.Proposer) || e.isBanned(p.Proposer)) {
		e.log.Debugf("Epoch %d proposal from non-validator %s", p.Epoch, p.Proposer)
		return
	}
	if h := p.Hash(); verifySig(p.Proposer, p.PubKey, h[:], p.Sig) != nil {
		e.log.Debugf("Bad epoch %d proposal signature from %s", p.Epoch, p.Proposer)
		return
	}
	nowMs := e.nowMs()
	h := p.Hash()
	e.mtx.Lock()
	if le, ok := e.lastEpochLocked(); ok && p.Epoch <= le {
		e.mtx.Unlock()
		return
	}
	key := fmt.Sprintf("%s|%d", p.Proposer, p.Epoch)
	if prev, found := e.propBy[key]; found && prev != h {
		e.addFlagLocked(&EpochFlag{Epoch: p.Epoch, Kind: "equivocation", Peer: p.Proposer, Detail: "two different proposals for the same epoch"})
	}
	e.propBy[key] = h
	if e.cands[p.Epoch] == nil {
		e.cands[p.Epoch] = make(map[EpochHash]*epochCandidate)
	}
	c := e.cands[p.Epoch][h]
	if c == nil {
		c = &epochCandidate{prop: p, hash: h, recv: nowMs}
		e.cands[p.Epoch][h] = c
		if p.Proposer != e.self {
			e.heard[p.Proposer] = nowMs
		}
		e.checkOmissionsLocked(p)
		e.checkCreditsLocked(p)
		if e.signer && p.Prev == e.lastFP && p.Proposer != e.self {
			if p.Epoch > e.p.EpochOf(e.startedAt) && p.Epoch != e.credCheckAt {
				e.credChecks++ // S5 listen period: a peer's signer list checked
				e.credCheckAt = p.Epoch
			}
		}
		if e.signer && e.last != nil && p.Prev == e.lastFP && p.Proposer != e.self {
			for _, cr := range p.Credits {
				if cr.Validator == e.self {
					e.ownSigsLocked([]*EpochAttestation{{Epoch: e.last.Epoch, Fingerprint: e.lastFP, Validator: cr.Validator, Stamp: cr.Stamp, Sig: cr.Sig}})
				}
			}
		}
	}
	e.computeLocked(c)
	behind := c.res == nil && c.bad == ""
	e.mtx.Unlock()
	if behind {
		go e.requestSync(ctx, false)
	}
	e.considerSign(ctx, c)
	e.tryFinalize(ctx, p.Epoch)
}

// checkOmissionsLocked flags a proposal that leaves out orders we had well
// before the epoch end.
func (e *EpochEngine) checkOmissionsLocked(p *EpochProposal) {
	if !e.signer || p.Proposer == e.self {
		return
	}
	end := e.p.EpochEnd(p.Epoch)
	in := make(map[OrderID]bool, len(p.Orders))
	for _, o := range p.Orders {
		in[o.ID()] = true
	}
	missing := 0
	var example OrderID
	for id, po := range e.pool {
		if in[id] || po.recv > end-e.p.OmissionGraceMs || po.ord.Stamp >= end-e.p.OmissionGraceMs || po.ord.Expiry <= end {
			continue
		}
		if _, seen := e.state.seen[id]; seen {
			continue
		}
		if len(p.Orders) >= e.p.MaxOrdersPerEpoch {
			continue // a full proposal may leave orders for the next epoch
		}
		missing++
		example = id
	}
	if missing > 0 {
		e.omissions[p.Proposer]++
		e.addFlagLocked(&EpochFlag{Epoch: p.Epoch, Kind: "omission", Peer: p.Proposer,
			Detail: fmt.Sprintf("proposal left out %d order(s) we had for >%d ms, e.g. %s", missing, e.p.OmissionGraceMs, example)})
	}
}

func (e *EpochEngine) considerSign(ctx context.Context, c *epochCandidate) {
	if !e.signer {
		return
	}
	n := c.prop.Epoch
	end := e.p.EpochEnd(n)
	nowMs := e.nowMs()
	earliest := end + e.p.ProposeDelayMs + int64(c.prop.Rank)*e.p.FallbackMs - e.p.MaxClockSkewMs
	if nowMs < earliest || nowMs > end+e.p.SignWindowMs {
		e.log.Debugf("Not signing epoch %d proposal from %s outside the window (now %+d ms from epoch end)", n, e.rel.ValidatorName(c.prop.Proposer), nowMs-end)
		return
	}
	e.mtx.Lock()
	if _, did := e.signed[n]; did || c.res == nil {
		e.mtx.Unlock()
		return
	}
	if !e.isMemberLocked(n) {
		e.mtx.Unlock()
		return
	}
	// S4: sign bond admissions only after our own chain check, and keep a
	// latched switch on.
	if !e.bondsVerifiedLocked(c.prop) || !e.latchOKLocked(c.prop) {
		e.mtx.Unlock()
		return
	}
	// S5: not while another instance of our key may run, and only proposals
	// that carry the equivocation proofs we hold.
	if hold := e.signHoldLocked(nowMs); hold != "" {
		e.mtx.Unlock()
		e.log.Debugf("Not signing epoch %d: %s", n, hold)
		return
	}
	if !e.bansOKLocked(c.prop) {
		e.mtx.Unlock()
		return
	}
	// S3: we sign even if we are not in the active set: the signature does
	// not count for the quorum, but it is credited, so we rejoin the active
	// set after signing again for long enough.
	e.mtx.Unlock()
	if !e.clockOK() {
		return
	}
	e.mtx.Lock()
	if _, did := e.signed[n]; did {
		e.mtx.Unlock()
		return
	}
	// S5: the sign guard survives restarts (one result per epoch, ever).
	if err := e.guardSignLocked(n, c.res.fp); err != nil {
		e.mtx.Unlock()
		e.log.Errorf("Not signing epoch %d: %v", n, err)
		return
	}
	e.signed[n] = c.res.fp
	e.noteSentLocked(attKey(n, c.res.fp, nowMs), nowMs)
	alt := e.equivocateLocked(c, nowMs)
	e.mtx.Unlock()
	a := &EpochAttestation{Epoch: n, Fingerprint: c.res.fp, Stamp: nowMs}
	if err := a.sign(e.priv); err != nil {
		e.log.Errorf("Error signing attestation: %v", err)
		return
	}
	e.send(ctx, &epochMsg{Type: epMsgAtt, Att: a})
	if alt != nil {
		e.send(ctx, &epochMsg{Type: epMsgAtt, Att: alt})
	}
	e.handleAttestation(ctx, a)
}

func (e *EpochEngine) handleAttestation(ctx context.Context, a *EpochAttestation) {
	e.mtx.Lock()
	member := e.valsLocked(a.Epoch).IsValidator(a.Validator) || (e.last != nil && a.Epoch == e.last.Epoch && e.valsLocked(e.last.Epoch).IsValidator(a.Validator))
	e.mtx.Unlock()
	if !member || a.verify() != nil {
		return
	}
	if a.Validator != e.self {
		e.heardFrom(a.Validator, a.Stamp)
	}
	e.mtx.Lock()
	// S5: a second, different result signed by the same validator for the
	// same epoch is an equivocation proof.
	if p := e.checkSigLocked(a); p != nil {
		e.mtx.Unlock()
		e.send(ctx, &epochMsg{Type: epMsgProof, Proof: p})
		e.mtx.Lock()
	}
	if le, ok := e.lastEpochLocked(); ok && a.Epoch <= le {
		e.addCreditLocked(a) // a late signature of our last record
		e.mtx.Unlock()
		return
	}
	if e.atts[a.Epoch] == nil {
		e.atts[a.Epoch] = make(map[EpochHash]map[string]*EpochAttestation)
	}
	if e.atts[a.Epoch][a.Fingerprint] == nil {
		e.atts[a.Epoch][a.Fingerprint] = make(map[string]*EpochAttestation)
	}
	e.atts[a.Epoch][a.Fingerprint][a.Validator] = a
	e.mtx.Unlock()
	e.tryFinalize(ctx, a.Epoch)
}

// tryFinalize finalizes epoch n if a computed candidate has enough
// signatures.
func (e *EpochEngine) tryFinalize(ctx context.Context, n uint64) {
	e.mtx.Lock()
	var rec *EpochRecord
	var fin *EpochFinal
	if le, ok := e.lastEpochLocked(); !ok || n > le {
		for _, c := range e.cands[n] {
			if c.res == nil {
				continue
			}
			sigs := e.atts[n][c.res.fp]
			if len(sigs) == 0 {
				continue
			}
			r := &EpochRecord{Epoch: n, Prev: c.prop.Prev, ProposalHash: c.hash, Proposer: c.prop.Proposer,
				Active: c.prop.Active, Matches: c.res.matches, StateHash: c.res.state.hash(), Fingerprint: c.res.fp, Proposal: c.prop}
			ids := make([]string, 0, len(sigs))
			for v := range sigs {
				ids = append(ids, v)
			}
			sort.Strings(ids)
			for _, v := range ids {
				r.Sigs = append(r.Sigs, sigs[v])
			}
			if verifyRecord(e.valsLocked(n), r) != nil {
				continue
			}
			rec = r
			fin = e.commitLocked(r, c.res, false)
			break
		}
	}
	e.mtx.Unlock()
	if rec == nil {
		return
	}
	if e.signer {
		// Peers have the proposal already (or sync it): send the record
		// without it to keep the broadcast small.
		e.send(ctx, &epochMsg{Type: epMsgRec, Rec: stripProposal(rec)})
	}
	e.afterFinal(ctx, fin)
}

// commitLocked makes rec the last finalized record.
func (e *EpochEngine) commitLocked(rec *EpochRecord, res *epochResult, synced bool) *EpochFinal {
	if le, ok := e.lastEpochLocked(); ok && rec.Epoch > le+1 {
		e.skipped += rec.Epoch - le - 1
	}
	e.state = res.state
	e.last = rec
	e.lastFP = rec.Fingerprint
	e.lastFinAt = e.nowMs()
	e.finalized++
	e.records = append(e.records, rec)
	if len(e.records) > maxRecordsInMemory {
		old := e.records[0]
		delete(e.byEpoch, old.Epoch)
		e.records = e.records[1:]
		if e.chainStart > 0 {
			e.chainStart--
		}
	}
	e.byEpoch[rec.Epoch] = rec
	wasOn := e.ledger.switchSet
	e.ledger.apply(rec)
	e.afterBansLocked(e.ledger.applyBans(rec), synced)
	e.afterLedgerLocked(rec, wasOn)
	e.resetCreditsLocked(rec)
	for _, o := range rec.Proposal.Orders {
		delete(e.pool, o.ID())
	}
	for _, c := range rec.Proposal.Cancels {
		delete(e.cpool, c.OrderID)
	}
	for id := range e.cpool {
		if _, resting := e.state.resting[id]; !resting {
			if _, inPool := e.pool[id]; !inPool {
				delete(e.cpool, id)
			}
		}
	}
	for k := range e.cands {
		if k <= rec.Epoch {
			delete(e.cands, k)
			delete(e.atts, k)
		}
	}
	if e.store != nil {
		if b, err := json.Marshal(rec); err == nil {
			_, _ = e.store.Write(append(b, '\n'))
		}
	}
	if len(rec.Matches) > 0 {
		e.log.Infof("Epoch %d finalized by %d signers (proposer %s): %d matches", rec.Epoch, len(rec.Sigs), e.rel.ValidatorName(rec.Proposer), len(rec.Matches))
	} else {
		e.log.Debugf("Epoch %d finalized by %d signers (proposer %s)", rec.Epoch, len(rec.Sigs), e.rel.ValidatorName(rec.Proposer))
	}
	return &EpochFinal{Record: rec, Makers: res.makers, Synced: synced}
}

// afterFinal notifies and re-evaluates later candidates that build on the
// new record.
func (e *EpochEngine) afterFinal(ctx context.Context, fin *EpochFinal) {
	if e.onFinal != nil {
		e.onFinal(fin)
	}
	e.mtx.Lock()
	var later []*epochCandidate
	for _, cs := range e.cands {
		for _, c := range cs {
			if c.res == nil && c.prop.Prev == e.lastFP {
				e.computeLocked(c)
				later = append(later, c)
			}
		}
	}
	e.mtx.Unlock()
	for _, c := range later {
		e.considerSign(ctx, c)
		e.tryFinalize(ctx, c.prop.Epoch)
	}
}

// handleRecord applies a finalized record from a peer (live or sync).
func (e *EpochEngine) handleRecord(ctx context.Context, rec *EpochRecord, synced bool) {
	if rec.Proposal == nil {
		// Live records come without the proposal; use the one we have.
		e.mtx.Lock()
		c := e.cands[rec.Epoch][rec.ProposalHash]
		have := e.byEpoch[rec.Epoch] != nil
		e.mtx.Unlock()
		if c == nil {
			if !have {
				go e.requestSync(ctx, false)
			}
			return
		}
		r := *rec
		r.Proposal = c.prop
		rec = &r
	}
	if rec.Proposal.Epoch != rec.Epoch || rec.Proposal.Prev != rec.Prev ||
		rec.Proposal.Proposer != rec.Proposer || !equalStrings(rec.Proposal.Active, rec.Active) {
		e.log.Debugf("Bad epoch %d record proposal", rec.Epoch)
		return
	}
	if e.signer {
		// S5: our key's signatures we did not make (another instance).
		e.mtx.Lock()
		e.ownSigsLocked(rec.Sigs)
		if rec.Proposer == e.self {
			e.dupSeenLocked(propKey(rec.Epoch, rec.Proposal.Stamp), rec.Proposal.Stamp, fmt.Sprintf("proposal for epoch %d in a peer's record", rec.Epoch))
		}
		e.mtx.Unlock()
	}
	e.mtx.Lock()
	// S4: the record is checked against the validator set its chain
	// implies: ours if it builds on our last record, the seeded set (fresh
	// ledger) if it starts a new chain.
	var vs *valSet
	switch rec.Prev {
	case e.lastFP:
		vs = e.valsLocked(rec.Epoch)
	case e.genesis:
		vs = newBondLedger(e.rel).members(rec.Epoch, 0)
	}
	if vs != nil {
		if err := verifyRecord(vs, rec); err != nil {
			e.mtx.Unlock()
			e.log.Debugf("Bad epoch %d record: %v", rec.Epoch, err)
			return
		}
	} else if have := e.byEpoch[rec.Epoch]; have == nil || have.Fingerprint == rec.Fingerprint {
		// Not on our chain tip: we may be behind. Sync rather than judge it.
		e.mtx.Unlock()
		if have == nil {
			go e.requestSync(ctx, false)
		}
		return
	} else if verifyRecord(e.valsLocked(rec.Epoch), rec) != nil {
		e.mtx.Unlock()
		return
	}
	le, ok := e.lastEpochLocked()
	if ok && rec.Epoch <= le {
		if have := e.byEpoch[rec.Epoch]; have != nil && have.Fingerprint != rec.Fingerprint {
			e.addFlagLocked(&EpochFlag{Epoch: rec.Epoch, Kind: "conflict", Peer: rec.Proposer, Detail: "validly signed record conflicts with ours"})
		}
		e.mtx.Unlock()
		return
	}
	if rec.Prev != e.lastFP {
		if rec.Prev == e.genesis && ok {
			e.log.Warnf("Validators restarted the epoch chain at epoch %d (was at %d); resetting the epoch state", rec.Epoch, le)
			e.state, e.last, e.lastFP = newEpochState(), nil, e.genesis
			e.chainStart = len(e.records)
			e.resetLedgerLocked()
		} else {
			e.mtx.Unlock()
			go e.requestSync(ctx, false)
			return
		}
	}
	c := &epochCandidate{prop: rec.Proposal, hash: rec.ProposalHash, recv: e.nowMs()}
	e.computeLocked(c)
	if c.res == nil {
		e.mtx.Unlock()
		return // flagged by computeLocked (S3 active set or credits)
	}
	if c.res.fp != rec.Fingerprint {
		e.addFlagLocked(&EpochFlag{Epoch: rec.Epoch, Kind: "divergence", Peer: rec.Proposer, Detail: "our matching result differs from the signed record"})
		e.mtx.Unlock()
		return
	}
	fin := e.commitLocked(rec, c.res, synced)
	e.mtx.Unlock()
	e.afterFinal(ctx, fin)
}

func (e *EpochEngine) requestSync(ctx context.Context, force bool) {
	e.mtx.Lock()
	nowMs := e.nowMs()
	if !force && nowMs-e.syncAt < e.p.EpochMs {
		e.mtx.Unlock()
		return
	}
	e.syncAt = nowMs
	from, _ := e.lastEpochLocked()
	e.mtx.Unlock()
	e.send(ctx, &epochMsg{Type: epMsgSyncReq, SyncFrom: from, To: e.self})
}

// serveSync answers a sync request (validators only, rate limited).
func (e *EpochEngine) serveSync(ctx context.Context, to string, from uint64) {
	if !e.signer || to == "" {
		return
	}
	e.mtx.Lock()
	nowMs := e.nowMs()
	if nowMs-e.served[to] < e.p.EpochMs/4 {
		e.mtx.Unlock()
		return
	}
	e.served[to] = nowMs
	var out []*EpochRecord
	i := sort.Search(len(e.records), func(i int) bool { return e.records[i].Epoch > from })
	for ; i < len(e.records) && len(out) < maxSyncChunk; i++ {
		out = append(out, e.records[i])
	}
	// The requester may be on an old chain (validators restarted).
	if from > 0 && len(out) == 0 && len(e.records) > 0 && e.records[len(e.records)-1].Epoch < from {
		out = nil
	}
	e.mtx.Unlock()
	if len(out) > 0 {
		e.send(ctx, &epochMsg{Type: epMsgRecs, Recs: out, To: to})
	}
}

func (e *EpochEngine) loadStore() error {
	f, err := os.OpenFile(e.cfg.StorePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	n := 0
	for sc.Scan() {
		var rec EpochRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Proposal == nil {
			continue
		}
		if rec.Prev != e.lastFP {
			if rec.Prev != e.genesis || verifyRecord(newBondLedger(e.rel).members(rec.Epoch, 0), &rec) != nil {
				continue
			}
			e.state, e.last, e.lastFP = newEpochState(), nil, e.genesis // chain restart
			e.chainStart = len(e.records)
			e.resetLedgerLocked()
		} else if verifyRecord(e.valsLocked(rec.Epoch), &rec) != nil {
			continue
		}
		c := &epochCandidate{prop: rec.Proposal, hash: rec.ProposalHash}
		e.computeLocked(c)
		if c.res == nil || c.res.fp != rec.Fingerprint {
			continue
		}
		r := rec
		e.state, e.last, e.lastFP = c.res.state, &r, r.Fingerprint
		e.records = append(e.records, &r)
		e.byEpoch[r.Epoch] = &r
		wasOn := e.ledger.switchSet
		e.ledger.apply(&r)
		e.afterBansLocked(e.ledger.applyBans(&r), true)
		e.afterLedgerLocked(&r, wasOn)
		e.valsCache = nil
		e.resetCreditsLocked(&r)
		n++
	}
	e.store = f
	if n > 0 {
		e.log.Infof("Loaded %d epoch records (last epoch %d)", n, e.last.Epoch)
	}
	return nil
}

// LastRecord is the last finalized record (nil before the first).
func (e *EpochEngine) lastEpoch() (uint64, bool) {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	return e.lastEpochLocked()
}

func (e *EpochEngine) LastRecord() *EpochRecord {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	return e.last
}

// Record returns the finalized record of epoch n, if known.
func (e *EpochEngine) Record(n uint64) *EpochRecord {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	return e.byEpoch[n]
}

// Records returns finalized records with epoch > from, at most limit.
func (e *EpochEngine) Records(from uint64, limit int) []*EpochRecord {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	i := sort.Search(len(e.records), func(i int) bool { return e.records[i].Epoch > from })
	var out []*EpochRecord
	for ; i < len(e.records) && len(out) < limit; i++ {
		out = append(out, e.records[i])
	}
	return out
}

// RestingOrder returns an order resting in the finalized epoch state.
func (e *EpochEngine) RestingOrder(id OrderID) (*MeshOrder, uint64, bool) {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	r := e.state.resting[id]
	if r == nil {
		return nil, 0, false
	}
	return r.ord, r.remaining, true
}

// OrderStatus is "pool", "resting", "done" (matched, cancelled or expired
// after admission) or "unknown".
func (e *EpochEngine) OrderStatus(id OrderID) string {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	if _, ok := e.state.resting[id]; ok {
		return "resting"
	}
	if _, ok := e.state.seen[id]; ok {
		return "done"
	}
	if _, ok := e.pool[id]; ok {
		return "pool"
	}
	return "unknown"
}

// SetOmitOwner makes this validator leave out the orders of owner from its
// proposals (simnet adversary tests only).
func (e *EpochEngine) SetOmitOwner(owner string) {
	e.mtx.Lock()
	e.omitOwner = owner
	e.mtx.Unlock()
}

// EpochValidatorStatus is a validator as seen by this node.
type EpochValidatorStatus struct {
	PeerID    string `json:"peerID"`
	Name      string `json:"name"`
	Bond      uint64 `json:"bond"`
	Active    bool   `json:"active"`
	Self      bool   `json:"self"`
	LastSeen  int64  `json:"lastSeen,omitempty"`
	Omissions int    `json:"omissions"`
	// S3: signatures credited in the active-set window and their share.
	Credits   int     `json:"credits"`
	CreditPct float64 `json:"creditPct"`
	// S4: seeded in the release, or a bonded member since epoch Since.
	Seeded    bool   `json:"seeded"`
	Since     uint64 `json:"since,omitempty"`
	BondAsset uint32 `json:"bondAsset,omitempty"`
}

// ActiveSetInfo is how the current active set was derived (S3).
type ActiveSetInfo struct {
	Epoch      uint64 `json:"epoch"`
	AsOf       uint64 `json:"asOf"`
	WindowFrom uint64 `json:"windowFrom"`
	WindowTo   uint64 `json:"windowTo"`
	Sample     int    `json:"sample"`
	Fallback   bool   `json:"fallback"`
}

// EpochStatus is the engine status (debug API and Mesh page).
type EpochStatus struct {
	Release        string                  `json:"release"`
	ReleaseHash    string                  `json:"releaseHash"`
	Role           string                  `json:"role"`
	Self           string                  `json:"self"`
	Params         EpochParams             `json:"params"`
	Now            int64                   `json:"now"`
	CurrentEpoch   uint64                  `json:"currentEpoch"`
	LastFinalized  uint64                  `json:"lastFinalized"`
	HaveFinalized  bool                    `json:"haveFinalized"`
	LastFinalAt    int64                   `json:"lastFinalAt,omitempty"`
	LastFP         string                  `json:"lastFingerprint"`
	LastSigners    int                     `json:"lastSigners"`
	Finalized      int                     `json:"finalized"`
	Skipped        uint64                  `json:"skipped"`
	Unfinalized    uint64                  `json:"unfinalizedSinceLast"`
	Pool           int                     `json:"pool"`
	Resting        int                     `json:"resting"`
	StateHash      string                  `json:"stateHash"`
	ClockSkewMs    int64                   `json:"clockSkewMs"`
	ClockPeers     int                     `json:"clockPeers"`
	ClockOK        bool                    `json:"clockOK"`
	Signing        bool                    `json:"signing"`
	ActivePower    uint64                  `json:"activePower"`
	BondedPower    uint64                  `json:"bondedPower"`
	CanFinalize    bool                    `json:"canFinalize"`
	ActiveSet      *ActiveSetInfo          `json:"activeSet"`
	Validators     []*EpochValidatorStatus `json:"validators"`
	Flags          []*EpochFlag            `json:"flags"`
	ProtocolVer    uint32                  `json:"protocol"`
	OmitOwnerDebug string                  `json:"omitOwnerDebug,omitempty"`
	Bonds          *BondSwitchStatus       `json:"bonds,omitempty"`    // S4 bond switch (nil: release without bonds)
	Banned         []string                `json:"banned"`             // S5 validators banned in our chain
	SignHold       string                  `json:"signHold,omitempty"` // S5 why we do not sign now
}

// Status returns the engine status.
func (e *EpochEngine) Status() *EpochStatus {
	skew, peers, ok := e.ClockSkew()
	nowMs := e.nowMs()
	e.mtx.Lock()
	defer e.mtx.Unlock()
	rh := e.rel.Hash()
	st := &EpochStatus{
		Release: e.rel.Name, ReleaseHash: EpochHash(rh).String(), Self: e.self, Params: e.p, Now: nowMs,
		CurrentEpoch: e.p.EpochOf(nowMs), Finalized: e.finalized, Skipped: e.skipped, Pool: len(e.pool),
		Resting: len(e.state.resting), StateHash: EpochHash(e.state.hash()).String(), ClockSkewMs: skew,
		ClockPeers: peers, ClockOK: ok, Signing: e.signer && ok,
		LastFP: e.lastFP.String(), LastFinalAt: e.lastFinAt, ProtocolVer: ProtocolVersion, OmitOwnerDebug: e.omitOwner,
	}
	st.Role = "client"
	if e.signer {
		st.Role = "validator"
	}
	vs := e.valsLocked(st.CurrentEpoch)
	st.BondedPower = vs.BondedPower()
	st.Signing = st.Signing && vs.IsValidator(e.self)
	if e.signer && !vs.IsValidator(e.self) {
		st.Role = "candidate" // S4: validator mode, no counting bond yet
		if e.ledger.bans[e.self] != nil {
			st.Role = "banned" // S5
		}
	}
	if e.signer {
		st.SignHold = e.signHoldLocked(nowMs)
		st.Signing = st.Signing && st.SignHold == ""
	}
	for id := range e.ledger.bans {
		st.Banned = append(st.Banned, e.nameOf(id))
	}
	sort.Strings(st.Banned)
	if e.rel.Bonds != nil {
		st.Bonds = e.switchStatusLocked(st.CurrentEpoch, st.LastFinalized)
	}
	if e.last != nil {
		st.LastFinalized, st.HaveFinalized, st.LastSigners = e.last.Epoch, true, len(e.last.Sigs)
		if cur := st.CurrentEpoch; cur > e.last.Epoch+1 {
			st.Unfinalized = cur - e.last.Epoch - 1
		}
	}
	as := e.activeForLocked(st.CurrentEpoch)
	st.ActiveSet = &ActiveSetInfo{Epoch: as.Epoch, AsOf: as.AsOf, WindowFrom: as.WindowFrom, WindowTo: as.WindowTo, Sample: as.Sample, Fallback: as.Fallback}
	active := make(map[string]bool)
	for _, a := range as.Active {
		active[a] = true
	}
	for _, id := range vs.ids {
		m := vs.members[id]
		v := &EpochValidatorStatus{PeerID: id, Name: e.nameOf(id), Bond: m.Power, Seeded: m.Seeded, Since: m.Since,
			Active: active[id], Self: id == e.self, LastSeen: e.heard[id], Omissions: e.omissions[id],
			Credits: as.Credits[id]}
		sample := as.Sample
		if n, isNew := as.Samples[id]; isNew {
			sample = n
		}
		if sample > 0 {
			v.CreditPct = float64(min(as.Credits[id], sample)) * 100 / float64(sample)
		}
		if m.Bond != nil {
			v.BondAsset = m.Bond.claim.AssetID
		}
		if v.Active {
			st.ActivePower += m.Power
		}
		st.Validators = append(st.Validators, v)
	}
	st.CanFinalize = thresholdMet(&e.p, st.ActivePower, st.ActivePower, st.BondedPower)
	st.Flags = append([]*EpochFlag(nil), e.flags...)
	return st
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// resetCreditsLocked starts the credit list of a new last record with its
// valid signatures.
func (e *EpochEngine) resetCreditsLocked(rec *EpochRecord) {
	e.lastCredits = make(map[string]*EpochCredit, len(rec.Sigs))
	for _, a := range rec.Sigs {
		e.addCreditLocked(a)
	}
}

// checkCreditsLocked flags a proposal that leaves out our own signature of
// its previous record (S3: credits decide the active set).
func (e *EpochEngine) checkCreditsLocked(p *EpochProposal) {
	if !e.signer || p.Proposer == e.self || e.last == nil || p.Prev != e.lastFP {
		return
	}
	if fp, did := e.signed[e.last.Epoch]; !did || fp != e.lastFP {
		return
	}
	for _, c := range p.Credits {
		if c.Validator == e.self {
			return
		}
	}
	e.addFlagLocked(&EpochFlag{Epoch: p.Epoch, Kind: "credit-omission", Peer: p.Proposer,
		Detail: fmt.Sprintf("proposal leaves out our signature of epoch %d", e.last.Epoch)})
}

// isBanned: id is banned in our chain (S5).
func (e *EpochEngine) isBanned(id string) bool {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	return e.ledger.bans[id] != nil
}
