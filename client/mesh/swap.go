// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// Mesh atomic swaps (P3). Once a match is agreed (negotiate.go), both sides
// run a swap state machine driven by a ticker and by encrypted inbox messages.
// The maker initiates, as in dcrdex:
//
//	maker                                     taker
//	secret, Initiate(maker asset -> TakerAddr, MakerLockTime)
//	swap_init {coin, contract, secretHash} ->  audit (recipient, value, lock
//	                                           time, secret hash), wait confs,
//	                                           Initiate(taker asset -> MakerAddr,
//	                                           TakerLockTime, same secret hash)
//	audit, wait confs   <- swap_part {coin, contract}
//	Redeem (reveals secret)
//	swap_redeem {coin, secret}  ------------>  check sha256(secret), Redeem
//	complete            <- swap_done {coin}    complete
//
// Messages carry only coin IDs and contracts (no raw txs), so they stay far
// below the 1 KiB mesh limit; auditors look the contract output up on chain.
// Each message is re-sent every tick until the next message arrives. If the
// secret message is lost, the taker finds the secret on chain once its own
// contract is spent. Refunds: whoever has a contract outstanding when its lock
// time expires refunds it (the maker after MakerLockTime, the taker after the
// shorter TakerLockTime). Every state change is persisted (SwapStore) before
// the next on-chain action, and LoadSwaps resumes unfinished swaps.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/libp2p/go-libp2p/core/peer"
)

// SwapContract describes a swap contract to create.
type SwapContract struct {
	Address    string
	Value      uint64
	SecretHash []byte
	LockTime   int64 // unix seconds
}

// SwapCoin is an on-chain swap output or transaction.
type SwapCoin struct {
	CoinID   []byte `json:"coinID"`
	Contract []byte `json:"contract,omitempty"`
	TxID     string `json:"txid"`
}

// SwapAudit is what an auditor learns about a counterparty contract.
type SwapAudit struct {
	Recipient  string
	Value      uint64
	SecretHash []byte
	Expiration int64 // unix seconds
}

// SwapWallet is the per-asset wallet functionality a swap needs. Core
// implements it on top of asset.Wallet.
type SwapWallet interface {
	Initiate(ctx context.Context, c *SwapContract) (*SwapCoin, error)
	Audit(ctx context.Context, coinID, contract []byte) (*SwapAudit, error)
	Confirmations(ctx context.Context, coinID, contract []byte) (confs uint32, spent bool, err error)
	Redeem(ctx context.Context, coinID, contract, secret []byte) (*SwapCoin, error)
	FindRedemption(ctx context.Context, coinID, contract []byte) (redeem *SwapCoin, secret []byte, err error)
	Refund(ctx context.Context, coinID, contract []byte) (*SwapCoin, error)
	LockTimeExpired(ctx context.Context, lockTime int64) (bool, error)
}

// Swap status values.
const (
	SwapNew         = "new"          // both: agreed, nothing on chain yet
	SwapInitSent    = "init_sent"    // maker: own contract broadcast
	SwapPartAudited = "part_audited" // maker: taker contract audited
	SwapRedeemed    = "redeemed"     // maker: taker contract redeemed
	SwapInitAudited = "init_audited" // taker: maker contract audited
	SwapPartSent    = "part_sent"    // taker: own contract broadcast
	SwapComplete    = "complete"     // both: done
	SwapRefunded    = "refunded"     // own contract refunded
	SwapAbandoned   = "abandoned"    // nothing at stake, gave up
	swapMsgInit     = "swap_init"
	swapMsgPart     = "swap_part"
	swapMsgRedeem   = "swap_redeem"
	swapMsgDone     = "swap_done"
	swapEventsMax   = 64
)

// Debug halt points to simulate a silent counterparty (simnet tests).
const (
	HaltNone        = ""
	HaltInit        = "init"        // maker never initiates
	HaltParticipate = "participate" // taker never participates
	HaltRedeem      = "redeem"      // maker never redeems (and stops talking)
)

// SwapEvent is a timestamped log line kept in the swap record.
type SwapEvent struct {
	Time int64  `json:"time"` // unix ms
	Msg  string `json:"msg"`
}

// SwapRecord is the persisted swap state.
type SwapRecord struct {
	Match       Match       `json:"match"`
	Side        string      `json:"side"`
	PeerEnc     []byte      `json:"peerEnc"`
	Status      string      `json:"status"`
	Reason      string      `json:"reason,omitempty"`
	Updated     int64       `json:"updated"`
	Secret      []byte      `json:"secret,omitempty"` // maker until revealed; SIMNET: stored in the clear
	SecretHash  []byte      `json:"secretHash,omitempty"`
	MakerSwap   *SwapCoin   `json:"makerSwap,omitempty"`
	TakerSwap   *SwapCoin   `json:"takerSwap,omitempty"`
	MakerRedeem *SwapCoin   `json:"makerRedeem,omitempty"`
	TakerRedeem *SwapCoin   `json:"takerRedeem,omitempty"`
	MakerRefund *SwapCoin   `json:"makerRefund,omitempty"`
	TakerRefund *SwapCoin   `json:"takerRefund,omitempty"`
	Events      []SwapEvent `json:"events"`
}

// Final is true when no more actions are needed.
func (r *SwapRecord) Final() bool {
	switch r.Status {
	case SwapComplete, SwapRefunded, SwapAbandoned:
		return true
	}
	return false
}

// MakerSends is the amount the maker locks; TakerSends the taker's.
func (m *Match) MakerSends() uint64 {
	if m.MakerSell {
		return m.Qty
	}
	return m.QuoteQty
}

func (m *Match) TakerSends() uint64 {
	if m.MakerSell {
		return m.QuoteQty
	}
	return m.Qty
}

// Swap messages. JSON []byte fields are base64.
type SwapInitMsg struct {
	MatchID    MatchID `json:"matchID"`
	CoinID     []byte  `json:"coinID"`
	Contract   []byte  `json:"contract"`
	SecretHash []byte  `json:"secretHash"`
}

type SwapPartMsg struct {
	MatchID  MatchID `json:"matchID"`
	CoinID   []byte  `json:"coinID"`
	Contract []byte  `json:"contract"`
}

type SwapRedeemMsg struct {
	MatchID MatchID `json:"matchID"`
	CoinID  []byte  `json:"coinID"`
	Secret  []byte  `json:"secret"`
}

type SwapDoneMsg struct {
	MatchID MatchID `json:"matchID"`
	CoinID  []byte  `json:"coinID"`
}

// SwapConfig is swap policy.
type SwapConfig struct {
	// SwapConfs is the confirmations required on a counterparty contract
	// before acting on it.
	SwapConfs uint32
	// SafetyMargin is the minimum time that must remain before a contract's
	// lock time for the taker to participate or the maker to initiate.
	SafetyMargin time.Duration
	// ResendInterval is how often the latest swap message is re-sent.
	ResendInterval time.Duration
}

// DefaultSwapConfig is for simnet.
var DefaultSwapConfig = SwapConfig{SwapConfs: 1, SafetyMargin: 20 * time.Second, ResendInterval: 5 * time.Second}

// SwapStore persists swap records, one JSON file per match.
type SwapStore struct{ dir string }

// NewSwapStore creates the directory if needed.
func NewSwapStore(dir string) (*SwapStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &SwapStore{dir: dir}, nil
}

func (s *SwapStore) path(id MatchID) string { return filepath.Join(s.dir, id.String()+".json") }

// Save atomically writes a record.
func (s *SwapStore) Save(r *SwapRecord) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(r.Match.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(r.Match.ID))
}

// Load reads all records.
func (s *SwapStore) Load() ([]*SwapRecord, error) {
	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	recs := make([]*SwapRecord, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var r SwapRecord
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		recs = append(recs, &r)
	}
	return recs, nil
}

type swapState struct {
	busy atomic.Bool

	mtx      sync.Mutex // guards rec, peerEnc, lastSend
	rec      *SwapRecord
	peer     peer.ID
	peerEnc  *secp256k1.PublicKey
	lastSend time.Time

	inMtx sync.Mutex // inbound peer data, set by message handlers
	in    struct {
		init   *SwapInitMsg
		part   *SwapPartMsg
		redeem *SwapRedeemMsg
		done   *SwapDoneMsg
	}
}

type swapper struct {
	cfg     SwapConfig
	wallets func(assetID uint32) (SwapWallet, error)
	store   *SwapStore
	halt    atomic.Value // string
	kick    chan struct{}
	swaps   map[MatchID]*swapState // guarded by ob.mtx
}

// EnableSwaps turns on swap execution for agreed matches. store may be nil
// (no persistence). Call RunSwaps to drive them.
func (ob *OrderBooks) EnableSwaps(cfg SwapConfig, wallets func(assetID uint32) (SwapWallet, error), store *SwapStore) {
	sw := &swapper{cfg: cfg, wallets: wallets, store: store, kick: make(chan struct{}, 1), swaps: make(map[MatchID]*swapState)}
	sw.halt.Store(HaltNone)
	ob.mtx.Lock()
	ob.swapper = sw
	ob.mtx.Unlock()
}

// SetSwapHalt sets a debug halt point (simnet tests).
func (ob *OrderBooks) SetSwapHalt(stage string) error {
	switch stage {
	case HaltNone, HaltInit, HaltParticipate, HaltRedeem:
	default:
		return fmt.Errorf("unknown halt stage %q", stage)
	}
	sw := ob.getSwapper()
	if sw == nil {
		return errors.New("swaps not enabled")
	}
	sw.halt.Store(stage)
	return nil
}

// SetLockTimes changes the swap lock durations (simnet tests). Both sides
// must use the same values, since proposals are checked against policy.
func (ob *OrderBooks) SetLockTimes(maker, taker time.Duration) error {
	if taker <= 0 || maker <= taker {
		return errors.New("need 0 < taker lock < maker lock")
	}
	ob.mtx.Lock()
	ob.cfg.MakerLockTime, ob.cfg.TakerLockTime = maker, taker
	ob.mtx.Unlock()
	return nil
}

func (ob *OrderBooks) getSwapper() *swapper {
	ob.mtx.RLock()
	defer ob.mtx.RUnlock()
	return ob.swapper
}

// startSwapLocked registers a swap for a newly agreed match. ob.mtx is held.
func (ob *OrderBooks) startSwapLocked(st *matchState) {
	sw := ob.swapper
	if sw == nil {
		return
	}
	if _, exists := sw.swaps[st.match.ID]; exists {
		return
	}
	rec := &SwapRecord{Match: st.match, Side: st.side, PeerEnc: st.peerEnc.SerializeCompressed(), Status: SwapNew}
	s := &swapState{rec: rec, peer: st.peer, peerEnc: st.peerEnc}
	ob.swapEvent(s, "match agreed, swap started as %s", st.side)
	sw.swaps[st.match.ID] = s
	ob.persist(sw, s)
	sw.poke()
}

func (sw *swapper) poke() {
	select {
	case sw.kick <- struct{}{}:
	default:
	}
}

// LoadSwaps restores persisted swaps (unfinished ones resume). EnableSwaps
// must have been called with a store.
func (ob *OrderBooks) LoadSwaps() (int, error) {
	sw := ob.getSwapper()
	if sw == nil || sw.store == nil {
		return 0, errors.New("no swap store")
	}
	recs, err := sw.store.Load()
	if err != nil {
		return 0, err
	}
	var n int
	ob.mtx.Lock()
	defer ob.mtx.Unlock()
	for _, r := range recs {
		peerID := r.Match.Maker
		if r.Side == sideMaker {
			peerID = r.Match.Taker
		}
		pid, err := peer.Decode(peerID)
		if err != nil {
			return n, err
		}
		enc, err := secp256k1.ParsePubKey(r.PeerEnc)
		if err != nil {
			return n, err
		}
		if _, exists := sw.swaps[r.Match.ID]; exists {
			continue
		}
		sw.swaps[r.Match.ID] = &swapState{rec: r, peer: pid, peerEnc: enc}
		if !r.Final() {
			n++
		}
	}
	sw.poke()
	return n, nil
}

// Swaps returns copies of all swap records, oldest first.
func (ob *OrderBooks) Swaps() []*SwapRecord {
	sw := ob.getSwapper()
	if sw == nil {
		return nil
	}
	ob.mtx.RLock()
	states := make([]*swapState, 0, len(sw.swaps))
	for _, s := range sw.swaps {
		states = append(states, s)
	}
	ob.mtx.RUnlock()
	recs := make([]*SwapRecord, 0, len(states))
	for _, s := range states {
		s.mtx.Lock()
		b, _ := json.Marshal(s.rec)
		s.mtx.Unlock()
		var r SwapRecord
		_ = json.Unmarshal(b, &r)
		r.Secret = nil // never expose
		recs = append(recs, &r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Match.Stamp < recs[j].Match.Stamp })
	return recs
}

// SwapRecord returns a copy of one record (without the secret).
func (ob *OrderBooks) SwapRecord(id MatchID) (*SwapRecord, bool) {
	for _, r := range ob.Swaps() {
		if r.Match.ID == id {
			return r, true
		}
	}
	return nil, false
}

// RunSwaps drives swaps until ctx is done.
func (ob *OrderBooks) RunSwaps(ctx context.Context, interval time.Duration) {
	sw := ob.getSwapper()
	if sw == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		ob.tickSwaps(ctx, sw)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-sw.kick:
		}
	}
}

func (ob *OrderBooks) tickSwaps(ctx context.Context, sw *swapper) {
	ob.mtx.RLock()
	states := make([]*swapState, 0, len(sw.swaps))
	for _, s := range sw.swaps {
		states = append(states, s)
	}
	ob.mtx.RUnlock()
	for _, s := range states {
		s.mtx.Lock()
		final := s.rec.Final()
		s.mtx.Unlock()
		if final || !s.busy.CompareAndSwap(false, true) {
			continue
		}
		go func() {
			defer s.busy.Store(false)
			ob.stepSwap(ctx, sw, s)
		}()
	}
}

// swapEvent logs and records an event. s.mtx must be held (or s unshared).
func (ob *OrderBooks) swapEvent(s *swapState, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ob.log.Infof("Swap %s (%s): %s", s.rec.Match.ID, s.rec.Side, msg)
	s.rec.Events = append(s.rec.Events, SwapEvent{Time: ob.now().UnixMilli(), Msg: msg})
	if len(s.rec.Events) > swapEventsMax {
		s.rec.Events = s.rec.Events[len(s.rec.Events)-swapEventsMax:]
	}
}

// persist saves the record. s.mtx must be held (or s unshared).
func (ob *OrderBooks) persist(sw *swapper, s *swapState) {
	s.rec.Updated = ob.now().UnixMilli()
	if sw.store == nil {
		return
	}
	if err := sw.store.Save(s.rec); err != nil {
		ob.log.Errorf("Error persisting swap %s: %v", s.rec.Match.ID, err)
	}
}

// swapWarn records a non-fatal problem once per distinct message.
func (ob *OrderBooks) swapWarn(sw *swapper, s *swapState, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if msg == s.rec.Reason {
		ob.log.Debugf("Swap %s: %s", s.rec.Match.ID, msg)
		return
	}
	s.rec.Reason = msg
	ob.swapEvent(s, "%s", msg)
	ob.persist(sw, s)
}

func (ob *OrderBooks) setSwapStatus(sw *swapper, s *swapState, status, format string, args ...any) {
	s.rec.Status, s.rec.Reason = status, ""
	ob.swapEvent(s, status+": "+format, args...)
	ob.persist(sw, s)
}

func (sw *swapper) wallet(assetID uint32) (SwapWallet, error) {
	return sw.wallets(assetID)
}

// stepSwap advances one swap. It holds s.mtx for the duration.
func (ob *OrderBooks) stepSwap(ctx context.Context, sw *swapper, s *swapState) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.inMtx.Lock()
	in := s.in
	s.inMtx.Unlock()
	halt, _ := sw.halt.Load().(string)
	if s.rec.Side == sideMaker {
		ob.stepMaker(ctx, sw, s, halt, in.part, in.done)
	} else {
		ob.stepTaker(ctx, sw, s, halt, in.init, in.redeem)
	}
}

func (ob *OrderBooks) resendDue(sw *swapper, s *swapState) bool {
	now := ob.now()
	if now.Sub(s.lastSend) < sw.cfg.ResendInterval {
		return false
	}
	s.lastSend = now
	return true
}

func (ob *OrderBooks) sendSwapMsg(ctx context.Context, s *swapState, typ string, payload any) {
	go ob.sendInbox(ctx, s.peer, s.peerEnc, typ, payload)
}

func (ob *OrderBooks) lockExpired(ctx context.Context, w SwapWallet, lockTime int64) bool {
	expired, err := w.LockTimeExpired(ctx, lockTime)
	if err != nil {
		ob.log.Debugf("LockTimeExpired error: %v", err)
		return false
	}
	return expired
}

func (ob *OrderBooks) stepMaker(ctx context.Context, sw *swapper, s *swapState, halt string, part *SwapPartMsg, done *SwapDoneMsg) {
	r := s.rec
	m := &r.Match
	sendW, err := sw.wallet(m.TakerReceives()) // maker sends what the taker receives
	if err != nil {
		ob.log.Warnf("Swap %s: no wallet for asset %d: %v", m.ID, m.TakerReceives(), err)
		return
	}
	recvW, err := sw.wallet(m.MakerReceives())
	if err != nil {
		ob.log.Warnf("Swap %s: no wallet for asset %d: %v", m.ID, m.MakerReceives(), err)
		return
	}
	now := ob.now()

	switch r.Status {
	case SwapNew:
		if halt == HaltInit {
			return
		}
		if now.Add(sw.cfg.SafetyMargin).Unix() >= m.TakerLockTime {
			ob.setSwapStatus(sw, s, SwapAbandoned, "too late to initiate")
			return
		}
		if r.Secret == nil {
			secret := make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				return
			}
			h := sha256.Sum256(secret)
			r.Secret, r.SecretHash = secret, h[:]
			ob.persist(sw, s) // secret is saved before it is used on chain
		}
		coin, err := sendW.Initiate(ctx, &SwapContract{Address: m.TakerAddr, Value: m.MakerSends(), SecretHash: r.SecretHash, LockTime: m.MakerLockTime})
		if err != nil {
			ob.swapWarn(sw, s, "initiate error (will retry): %v", err)
			return
		}
		r.MakerSwap = coin
		ob.setSwapStatus(sw, s, SwapInitSent, "initiated %d atoms of asset %d to %s, lock %d, tx %s",
			m.MakerSends(), m.TakerReceives(), m.TakerAddr, m.MakerLockTime, coin.TxID)
		s.lastSend = time.Time{}
		fallthrough
	case SwapInitSent:
		if part != nil && r.TakerSwap == nil {
			a, err := recvW.Audit(ctx, part.CoinID, part.Contract)
			if err != nil {
				ob.log.Debugf("Swap %s: audit of taker contract not ready: %v", m.ID, err)
			} else if reason := checkAudit(a, m.MakerAddr, m.TakerSends(), r.SecretHash, m.TakerLockTime); reason != "" {
				ob.swapWarn(sw, s, "taker contract rejected: %s", reason)
			} else {
				r.TakerSwap = &SwapCoin{CoinID: part.CoinID, Contract: part.Contract, TxID: coinTxID(part.CoinID)}
				ob.setSwapStatus(sw, s, SwapPartAudited, "audited taker contract %s: %d atoms to %s, lock %d",
					r.TakerSwap.TxID, a.Value, a.Recipient, a.Expiration)
			}
		}
		if r.Status == SwapInitSent {
			if ob.lockExpired(ctx, sendW, m.MakerLockTime) {
				ob.makerRefund(ctx, sw, s, sendW)
				return
			}
			if ob.resendDue(sw, s) {
				ob.sendSwapMsg(ctx, s, swapMsgInit, &SwapInitMsg{MatchID: m.ID, CoinID: r.MakerSwap.CoinID, Contract: r.MakerSwap.Contract, SecretHash: r.SecretHash})
			}
			return
		}
		fallthrough
	case SwapPartAudited:
		if halt == HaltRedeem {
			if ob.lockExpired(ctx, sendW, m.MakerLockTime) {
				ob.makerRefund(ctx, sw, s, sendW)
			}
			return
		}
		confs, spent, err := recvW.Confirmations(ctx, r.TakerSwap.CoinID, r.TakerSwap.Contract)
		if err != nil || confs < sw.cfg.SwapConfs {
			if ob.lockExpired(ctx, sendW, m.MakerLockTime) {
				ob.makerRefund(ctx, sw, s, sendW)
			}
			return
		}
		if spent {
			ob.swapWarn(sw, s, "taker contract already spent (refunded?)")
		}
		coin, err := recvW.Redeem(ctx, r.TakerSwap.CoinID, r.TakerSwap.Contract, r.Secret)
		if err != nil {
			ob.swapWarn(sw, s, "redeem error (will retry): %v", err)
			if ob.lockExpired(ctx, sendW, m.MakerLockTime) {
				ob.makerRefund(ctx, sw, s, sendW)
			}
			return
		}
		r.MakerRedeem = coin
		ob.setSwapStatus(sw, s, SwapRedeemed, "redeemed taker contract, tx %s (secret revealed)", coin.TxID)
		s.lastSend = time.Time{}
		fallthrough
	case SwapRedeemed:
		if done != nil {
			r.TakerRedeem = &SwapCoin{CoinID: done.CoinID, TxID: coinTxID(done.CoinID)}
			ob.setSwapStatus(sw, s, SwapComplete, "taker reports redeem tx %s", r.TakerRedeem.TxID)
			return
		}
		if ob.resendDue(sw, s) {
			ob.sendSwapMsg(ctx, s, swapMsgRedeem, &SwapRedeemMsg{MatchID: m.ID, CoinID: r.MakerRedeem.CoinID, Secret: r.Secret})
		}
		// The taker can always find the secret on chain, so stop nagging once
		// the taker's window is long past.
		if now.Unix() > m.MakerLockTime {
			ob.setSwapStatus(sw, s, SwapComplete, "no done ack from taker before maker lock time")
		}
	}
}

func (ob *OrderBooks) makerRefund(ctx context.Context, sw *swapper, s *swapState, w SwapWallet) {
	r := s.rec
	coin, err := w.Refund(ctx, r.MakerSwap.CoinID, r.MakerSwap.Contract)
	if err != nil {
		ob.swapWarn(sw, s, "refund error (will retry): %v", err)
		return
	}
	r.MakerRefund = coin
	ob.setSwapStatus(sw, s, SwapRefunded, "maker lock time expired, refunded own contract, tx %s", coin.TxID)
}

func (ob *OrderBooks) stepTaker(ctx context.Context, sw *swapper, s *swapState, halt string, init *SwapInitMsg, redeem *SwapRedeemMsg) {
	r := s.rec
	m := &r.Match
	sendW, err := sw.wallet(m.MakerReceives()) // taker sends what the maker receives
	if err != nil {
		ob.log.Warnf("Swap %s: no wallet for asset %d: %v", m.ID, m.MakerReceives(), err)
		return
	}
	recvW, err := sw.wallet(m.TakerReceives())
	if err != nil {
		ob.log.Warnf("Swap %s: no wallet for asset %d: %v", m.ID, m.TakerReceives(), err)
		return
	}
	now := ob.now()

	switch r.Status {
	case SwapNew:
		if init == nil {
			if now.Unix() > m.TakerLockTime {
				ob.setSwapStatus(sw, s, SwapAbandoned, "maker never initiated")
			}
			return
		}
		a, err := recvW.Audit(ctx, init.CoinID, init.Contract)
		if err != nil {
			ob.log.Debugf("Swap %s: audit of maker contract not ready: %v", m.ID, err)
			return
		}
		if reason := checkAudit(a, m.TakerAddr, m.MakerSends(), init.SecretHash, m.MakerLockTime); reason != "" {
			ob.swapWarn(sw, s, "maker contract rejected: %s", reason)
			return
		}
		r.SecretHash = init.SecretHash
		r.MakerSwap = &SwapCoin{CoinID: init.CoinID, Contract: init.Contract, TxID: coinTxID(init.CoinID)}
		ob.setSwapStatus(sw, s, SwapInitAudited, "audited maker contract %s: %d atoms to %s, lock %d",
			r.MakerSwap.TxID, a.Value, a.Recipient, a.Expiration)
		fallthrough
	case SwapInitAudited:
		if halt == HaltParticipate {
			return
		}
		if now.Add(sw.cfg.SafetyMargin).Unix() >= m.TakerLockTime {
			ob.setSwapStatus(sw, s, SwapAbandoned, "too late to participate")
			return
		}
		confs, _, err := recvW.Confirmations(ctx, r.MakerSwap.CoinID, r.MakerSwap.Contract)
		if err != nil || confs < sw.cfg.SwapConfs {
			return
		}
		coin, err := sendW.Initiate(ctx, &SwapContract{Address: m.MakerAddr, Value: m.TakerSends(), SecretHash: r.SecretHash, LockTime: m.TakerLockTime})
		if err != nil {
			ob.swapWarn(sw, s, "participate error (will retry): %v", err)
			return
		}
		r.TakerSwap = coin
		ob.setSwapStatus(sw, s, SwapPartSent, "participated %d atoms of asset %d to %s, lock %d, tx %s",
			m.TakerSends(), m.MakerReceives(), m.MakerAddr, m.TakerLockTime, coin.TxID)
		s.lastSend = time.Time{}
		fallthrough
	case SwapPartSent:
		var secret []byte
		if redeem != nil && bytes.Equal(sha256Sum(redeem.Secret), r.SecretHash) {
			secret = redeem.Secret
			r.MakerRedeem = &SwapCoin{CoinID: redeem.CoinID, TxID: coinTxID(redeem.CoinID)}
			ob.swapEvent(s, "secret received from maker, maker redeem tx %s", r.MakerRedeem.TxID)
		}
		if secret == nil {
			_, spent, err := sendW.Confirmations(ctx, r.TakerSwap.CoinID, r.TakerSwap.Contract)
			if err == nil && spent {
				fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				rc, sec, err := sendW.FindRedemption(fctx, r.TakerSwap.CoinID, r.TakerSwap.Contract)
				cancel()
				if err == nil && bytes.Equal(sha256Sum(sec), r.SecretHash) {
					secret = sec
					r.MakerRedeem = rc
					ob.swapEvent(s, "secret found on chain in maker redeem tx %s", rc.TxID)
				}
			}
		}
		if secret == nil {
			if ob.lockExpired(ctx, sendW, m.TakerLockTime) {
				coin, err := sendW.Refund(ctx, r.TakerSwap.CoinID, r.TakerSwap.Contract)
				if err != nil {
					ob.swapWarn(sw, s, "refund error (will retry): %v", err)
					return
				}
				r.TakerRefund = coin
				ob.setSwapStatus(sw, s, SwapRefunded, "taker lock time expired, refunded own contract, tx %s", coin.TxID)
				return
			}
			if ob.resendDue(sw, s) {
				ob.sendSwapMsg(ctx, s, swapMsgPart, &SwapPartMsg{MatchID: m.ID, CoinID: r.TakerSwap.CoinID, Contract: r.TakerSwap.Contract})
			}
			return
		}
		r.Secret = secret
		coin, err := recvW.Redeem(ctx, r.MakerSwap.CoinID, r.MakerSwap.Contract, secret)
		if err != nil {
			ob.swapWarn(sw, s, "redeem error (will retry): %v", err)
			return
		}
		r.TakerRedeem = coin
		ob.setSwapStatus(sw, s, SwapComplete, "redeemed maker contract, tx %s", coin.TxID)
		ob.sendSwapMsg(ctx, s, swapMsgDone, &SwapDoneMsg{MatchID: m.ID, CoinID: coin.CoinID})
	}
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// checkAudit returns a rejection reason or "".
func checkAudit(a *SwapAudit, recipient string, value uint64, secretHash []byte, lockTime int64) string {
	switch {
	case a.Recipient != recipient:
		return fmt.Sprintf("recipient %s, want %s", a.Recipient, recipient)
	case a.Value < value:
		return fmt.Sprintf("value %d, want %d", a.Value, value)
	case len(secretHash) != 32 || !bytes.Equal(a.SecretHash, secretHash):
		return "secret hash mismatch"
	case a.Expiration != lockTime:
		return fmt.Sprintf("lock time %d, want %d", a.Expiration, lockTime)
	}
	return ""
}

// coinTxID renders a UTXO coin ID (32-byte tx hash + 4-byte vout) as txid:vout.
func coinTxID(coinID []byte) string {
	if len(coinID) != 36 {
		return fmt.Sprintf("%x", coinID)
	}
	h := make([]byte, 32)
	for i := range h {
		h[i] = coinID[31-i]
	}
	vout := uint32(coinID[32])<<24 | uint32(coinID[33])<<16 | uint32(coinID[34])<<8 | uint32(coinID[35])
	return fmt.Sprintf("%x:%d", h, vout)
}

// handleSwapMsg stores inbound swap data and kicks the swap loop.
func (ob *OrderBooks) handleSwapMsg(from peer.ID, typ string, payload []byte) {
	sw := ob.getSwapper()
	if sw == nil {
		return
	}
	var id MatchID
	var store func(s *swapState) bool
	switch typ {
	case swapMsgInit:
		var msg SwapInitMsg
		if json.Unmarshal(payload, &msg) != nil {
			return
		}
		id = msg.MatchID
		store = func(s *swapState) bool {
			if s.rec.Side != sideTaker || s.in.init != nil {
				return false
			}
			s.in.init = &msg
			return true
		}
	case swapMsgPart:
		var msg SwapPartMsg
		if json.Unmarshal(payload, &msg) != nil {
			return
		}
		id = msg.MatchID
		store = func(s *swapState) bool {
			if s.rec.Side != sideMaker || s.in.part != nil {
				return false
			}
			s.in.part = &msg
			return true
		}
	case swapMsgRedeem:
		var msg SwapRedeemMsg
		if json.Unmarshal(payload, &msg) != nil {
			return
		}
		id = msg.MatchID
		store = func(s *swapState) bool {
			if s.rec.Side != sideTaker || s.in.redeem != nil {
				return false
			}
			s.in.redeem = &msg
			return true
		}
	case swapMsgDone:
		var msg SwapDoneMsg
		if json.Unmarshal(payload, &msg) != nil {
			return
		}
		id = msg.MatchID
		store = func(s *swapState) bool {
			if s.rec.Side != sideMaker || s.in.done != nil {
				return false
			}
			s.in.done = &msg
			return true
		}
	default:
		return
	}
	ob.mtx.RLock()
	s := sw.swaps[id]
	ob.mtx.RUnlock()
	if s == nil || s.peer != from {
		ob.log.Debugf("Ignoring %s for unknown swap %s from %s", typ, id, from)
		return
	}
	s.inMtx.Lock()
	stored := store(s)
	s.inMtx.Unlock()
	if stored {
		ob.log.Infof("Swap %s: received %s from %s", id, typ, from)
		sw.poke()
	} else if typ == swapMsgRedeem {
		// Duplicate secret after we finished: re-ack.
		if s.mtx.TryLock() { // don't block the inbox on a running step
			if s.rec.Status == SwapComplete && s.rec.TakerRedeem != nil {
				ob.sendSwapMsg(context.Background(), s, swapMsgDone, &SwapDoneMsg{MatchID: id, CoinID: s.rec.TakerRedeem.CoinID})
			}
			s.mtx.Unlock()
		}
	}
}

// abortSwapLocked abandons a not-yet-started taker swap after a late maker
// reject. ob.mtx is held.
func (ob *OrderBooks) abortSwapLocked(id MatchID, reason string) {
	sw := ob.swapper
	if sw == nil {
		return
	}
	s := sw.swaps[id]
	if s == nil {
		return
	}
	go func() {
		s.mtx.Lock()
		defer s.mtx.Unlock()
		if s.rec.Status == SwapNew && s.rec.Side == sideTaker {
			ob.setSwapStatus(sw, s, SwapAbandoned, "match rejected: %s", reason)
		}
	}()
}
