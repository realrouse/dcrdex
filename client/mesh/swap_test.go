// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// fakeChain is a shared in-memory "blockchain" holding swap contracts for all
// assets, with an adjustable clock for lock times.
type fakeChain struct {
	mtx       sync.Mutex
	n         uint32
	contracts map[string]*fakeContract
	balances  map[string]int64 // "asset/addr-or-owner" -> atoms
	offset    time.Duration
	// tamper, if set, changes the value of contracts created by owner.
	tamper string
	// crash simulates a process crash inside Initiate (once per owner).
	crash        map[string]string
	onCrash      func()
	spentFunding map[string]bool
	// spv makes Confirmations behave like an SPV wallet: 0 confirmations
	// and no error, whether or not the output exists.
	spv bool
	// dead owners' wallets fail every Initiate: a crashed process does
	// nothing more, even if test goroutines are still winding down.
	dead map[string]bool
}

type fakeContract struct {
	asset      uint32
	contract   []byte
	addr       string
	owner      string
	value      uint64
	secretHash []byte
	lockTime   int64
	spent      string // "", "redeem", "refund"
	secret     []byte
}

func (c *fakeChain) setSPV(spv bool) {
	c.mtx.Lock()
	c.spv = spv
	c.mtx.Unlock()
}

func newFakeChain() *fakeChain {
	return &fakeChain{contracts: make(map[string]*fakeContract), balances: make(map[string]int64),
		crash: make(map[string]string), spentFunding: make(map[string]bool), dead: make(map[string]bool)}
}

func (c *fakeChain) advance(d time.Duration) {
	c.mtx.Lock()
	c.offset += d
	c.mtx.Unlock()
}

func (c *fakeChain) balance(asset uint32, who string) int64 {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return c.balances[fmt.Sprintf("%d/%s", asset, who)]
}

type fakeSwapWallet struct {
	c     *fakeChain
	asset uint32
	owner string
}

func (w *fakeSwapWallet) key(who string) string { return fmt.Sprintf("%d/%s", w.asset, who) }

func (w *fakeSwapWallet) Initiate(_ context.Context, sc *SwapContract, prepared PreparedFunc) (*SwapCoin, error) {
	w.c.mtx.Lock()
	if w.c.dead[w.owner] {
		w.c.mtx.Unlock()
		return nil, errors.New("process is dead")
	}
	funding := sc.Inputs
	if len(funding) == 0 {
		w.c.n++
		f := sha256.Sum256(append([]byte("fund"), binary.BigEndian.AppendUint32(nil, w.c.n)...))
		funding = [][]byte{append(f[:], 0, 0, 0, 0)}
	}
	for _, f := range funding {
		if w.c.spentFunding[string(f)] {
			w.c.mtx.Unlock()
			return nil, fmt.Errorf("%w: funding coin spent", ErrFundingSpent)
		}
	}
	w.c.n++
	coinID := make([]byte, 36)
	h := sha256.Sum256(binary.BigEndian.AppendUint32(nil, w.c.n))
	copy(coinID, h[:])
	contract := bytes.Repeat([]byte{byte(w.c.n)}, 97) // dcr/btc contracts are ~97 bytes
	value := sc.Value
	if w.c.tamper == w.owner {
		value--
	}
	crash := w.c.crash[w.owner]
	delete(w.c.crash, w.owner)
	w.c.mtx.Unlock()

	coin := &SwapCoin{CoinID: coinID, Contract: contract, TxID: coinTxID(coinID)}
	if err := prepared(coin, funding); err != nil {
		return nil, err
	}
	if crash == crashBeforeBroadcast {
		return nil, errors.New("crashed before broadcast")
	}
	if crash == fundingStolen {
		// Another tx spent the funding coins first; ours is rejected.
		w.c.mtx.Lock()
		for _, f := range funding {
			w.c.spentFunding[string(f)] = true
		}
		w.c.mtx.Unlock()
		return nil, errors.New("sendrawtx error: TX rejected: output already spent")
	}

	w.c.mtx.Lock()
	defer w.c.mtx.Unlock()
	for _, f := range funding {
		w.c.spentFunding[string(f)] = true
	}
	w.c.contracts[string(coinID)] = &fakeContract{asset: w.asset, contract: contract, addr: sc.Address, owner: w.owner,
		value: value, secretHash: sc.SecretHash, lockTime: sc.LockTime}
	w.c.balances[w.key(w.owner)] -= int64(value)
	if crash == crashAfterBroadcast {
		if w.c.onCrash != nil {
			w.c.onCrash()
		}
		return nil, errors.New("crashed after broadcast")
	}
	return coin, nil
}

const (
	crashBeforeBroadcast = "before"
	crashAfterBroadcast  = "after"
	fundingStolen        = "stolen"
)

func (c *fakeChain) contractsBy(owner string) int {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	var n int
	for _, fc := range c.contracts {
		if fc.owner == owner {
			n++
		}
	}
	return n
}

func (w *fakeSwapWallet) lookup(coinID, contract []byte) (*fakeContract, error) {
	fc := w.c.contracts[string(coinID)]
	if fc == nil || fc.asset != w.asset || !bytes.Equal(fc.contract, contract) {
		return nil, errors.New("contract not found")
	}
	return fc, nil
}

func (w *fakeSwapWallet) Audit(_ context.Context, coinID, contract []byte) (*SwapAudit, error) {
	w.c.mtx.Lock()
	defer w.c.mtx.Unlock()
	fc, err := w.lookup(coinID, contract)
	if err != nil {
		return nil, err
	}
	return &SwapAudit{Recipient: fc.addr, Value: fc.value, SecretHash: fc.secretHash, Expiration: fc.lockTime}, nil
}

func (w *fakeSwapWallet) Confirmations(_ context.Context, coinID, contract []byte) (uint32, bool, error) {
	w.c.mtx.Lock()
	defer w.c.mtx.Unlock()
	if w.c.dead[w.owner] {
		return 0, false, errors.New("process is dead")
	}
	fc, err := w.lookup(coinID, contract)
	if err != nil {
		if w.c.spv {
			return 0, false, nil
		}
		return 0, false, err
	}
	if w.c.spv {
		return 0, fc.spent != "", nil
	}
	return 1, fc.spent != "", nil
}

func (w *fakeSwapWallet) Redeem(_ context.Context, coinID, contract, secret []byte) (*SwapCoin, error) {
	w.c.mtx.Lock()
	defer w.c.mtx.Unlock()
	fc, err := w.lookup(coinID, contract)
	if err != nil {
		return nil, err
	}
	if fc.spent != "" {
		return nil, errors.New("already spent")
	}
	if !bytes.Equal(sha256Sum(secret), fc.secretHash) {
		return nil, errors.New("bad secret")
	}
	fc.spent, fc.secret = "redeem", secret
	w.c.balances[w.key(fc.addr)] += int64(fc.value)
	id := append([]byte("redeem"), coinID[:30]...)
	return &SwapCoin{CoinID: id, TxID: coinTxID(id)}, nil
}

func (w *fakeSwapWallet) FindRedemption(_ context.Context, coinID, contract []byte) (*SwapCoin, []byte, error) {
	w.c.mtx.Lock()
	defer w.c.mtx.Unlock()
	fc, err := w.lookup(coinID, contract)
	if err != nil {
		return nil, nil, err
	}
	if fc.spent != "redeem" {
		return nil, nil, errors.New("not redeemed")
	}
	id := append([]byte("redeem"), coinID[:30]...)
	return &SwapCoin{CoinID: id, TxID: coinTxID(id)}, fc.secret, nil
}

func (w *fakeSwapWallet) Refund(_ context.Context, coinID, contract []byte) (*SwapCoin, error) {
	w.c.mtx.Lock()
	defer w.c.mtx.Unlock()
	fc, err := w.lookup(coinID, contract)
	if err != nil {
		return nil, err
	}
	if fc.spent != "" {
		return nil, errors.New("already spent")
	}
	if time.Now().Add(w.c.offset).Unix() < fc.lockTime {
		return nil, errors.New("lock time not expired")
	}
	fc.spent = "refund"
	w.c.balances[w.key(fc.owner)] += int64(fc.value)
	id := append([]byte("refund"), coinID[:30]...)
	return &SwapCoin{CoinID: id, TxID: coinTxID(id)}, nil
}

func (w *fakeSwapWallet) LockTimeExpired(_ context.Context, lockTime int64) (bool, error) {
	w.c.mtx.Lock()
	defer w.c.mtx.Unlock()
	return time.Now().Add(w.c.offset).Unix() >= lockTime, nil
}

var fastSwapCfg = SwapConfig{SwapConfs: 1, SafetyMargin: time.Second, ResendInterval: 30 * time.Millisecond,
	MissingContractTimeout: 200 * time.Millisecond}

func walletOwner(ob *OrderBooks) string { return ob.self.String()[len(ob.self.String())-6:] }

var testStoreKey = bytes.Repeat([]byte{7}, 32)

func enableFakeSwaps(t *testing.T, ob *OrderBooks, chain *fakeChain, store *SwapStore) context.CancelFunc {
	t.Helper()
	owner := walletOwner(ob)
	ob.EnableSwaps(fastSwapCfg, func(assetID uint32) (SwapWallet, error) {
		return &fakeSwapWallet{c: chain, asset: assetID, owner: owner}, nil
	}, store)
	ctx, cancel := context.WithCancel(tCtx)
	go ob.RunSwaps(ctx, 20*time.Millisecond)
	t.Cleanup(cancel)
	return cancel
}

func waitSwap(t *testing.T, ob *OrderBooks, id MatchID, status string, timeout time.Duration) *SwapRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r, ok := ob.SwapRecord(id)
		if ok && r.Status == status {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("swap %s (%s): want %s, have %+v", id, ob.self, status, r)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// agree negotiates a 2-lot DCR (asset 42) sell by the maker, BTC (asset 0)
// quote, and returns the match ID.
func agree(t *testing.T, maker, taker *OrderBooks) MatchID {
	t.Helper()
	if _, err := maker.PostOrder(tCtx, dcrBTC, true, 2*lot, lot, 2e5, time.Minute); err != nil {
		t.Fatal(err)
	}
	res, err := taker.Take(tCtx, dcrBTC, false, 2*lot, 2e5, lot, false, time.Minute)
	if err != nil || len(res.Proposed) != 1 {
		t.Fatalf("take: %+v %v", res, err)
	}
	id := res.Proposed[0]
	waitStatus(t, taker, id, MatchAgreed)
	waitStatus(t, maker, id, MatchAgreed)
	return id
}

func TestSwapRedeem(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	id := agree(t, maker, taker)
	mr := waitSwap(t, maker, id, SwapComplete, 5*time.Second)
	tr := waitSwap(t, taker, id, SwapComplete, 5*time.Second)
	m := mr.Match
	if m.MakerSends() != 2*lot || m.TakerSends() != m.QuoteQty {
		t.Fatalf("amounts %d %d", m.MakerSends(), m.TakerSends())
	}
	// Maker paid DCR, got BTC at its address; taker the reverse.
	if chain.balance(42, walletOwner(maker)) != -2*lot || chain.balance(0, m.MakerAddr) != int64(m.QuoteQty) ||
		chain.balance(0, walletOwner(taker)) != -int64(m.QuoteQty) || chain.balance(42, m.TakerAddr) != 2*lot {
		t.Fatalf("balances wrong: %+v", chain.balances)
	}
	if mr.MakerSwap.TxID != tr.MakerSwap.TxID || mr.TakerSwap.TxID != tr.TakerSwap.TxID ||
		tr.MakerRedeem == nil || tr.MakerRedeem.TxID != mr.MakerRedeem.TxID || mr.TakerRedeem == nil || mr.TakerRedeem.TxID != tr.TakerRedeem.TxID {
		t.Fatalf("records disagree:\n%+v\n%+v", mr, tr)
	}
	if mr.Secret != nil || tr.Secret != nil {
		t.Fatal("secret exposed in snapshot")
	}
}

func TestSwapSecretFromChain(t *testing.T) {
	m, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	// Drop the maker's swap_redeem messages: the taker must find the secret
	// on chain. Inbox messages are opaque, so drop maker->taker inbox traffic
	// once the maker has redeemed.
	m.setDrop(func(topic string, from peer.ID) bool {
		if from != maker.self || !strings.HasPrefix(topic, InboxTopicPrefix) {
			return false
		}
		for _, r := range maker.Swaps() {
			if r.Status == SwapRedeemed {
				return true
			}
		}
		return false
	})
	id := agree(t, maker, taker)
	tr := waitSwap(t, taker, id, SwapComplete, 5*time.Second)
	if tr.MakerRedeem == nil {
		t.Fatal("maker redeem not recorded")
	}
	found := false
	for _, ev := range tr.Events {
		found = found || strings.Contains(ev.Msg, "secret found on chain")
	}
	if !found {
		t.Fatalf("no on-chain secret event: %+v", tr.Events)
	}
	// The taker's done ack still reaches the maker.
	waitSwap(t, maker, id, SwapComplete, time.Second)
}

func TestSwapRefunds(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	// The maker goes silent after the taker participates.
	if err := maker.SetSwapHalt(HaltRedeem); err != nil {
		t.Fatal(err)
	}
	id := agree(t, maker, taker)
	waitSwap(t, taker, id, SwapPartSent, 5*time.Second)
	waitSwap(t, maker, id, SwapPartAudited, 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	if r, _ := taker.SwapRecord(id); r.Status != SwapPartSent {
		t.Fatalf("taker refunded early: %s", r.Status)
	}
	// Past the taker lock time only.
	chain.advance(time.Hour + time.Minute)
	tr := waitSwap(t, taker, id, SwapRefunded, 5*time.Second)
	if r, _ := maker.SwapRecord(id); r.Status != SwapPartAudited {
		t.Fatalf("maker refunded early: %s", r.Status)
	}
	// Past the maker lock time.
	chain.advance(time.Hour)
	mr := waitSwap(t, maker, id, SwapRefunded, 5*time.Second)
	if tr.TakerRefund == nil || mr.MakerRefund == nil {
		t.Fatal("missing refund coins")
	}
	for _, ob := range obs {
		for _, a := range []uint32{0, 42} {
			if b := chain.balance(a, walletOwner(ob)); b != 0 {
				t.Fatalf("asset %d balance %d after refunds", a, b)
			}
		}
	}
}

func TestSwapTakerSilent(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	_ = taker.SetSwapHalt(HaltParticipate)
	id := agree(t, maker, taker)
	waitSwap(t, taker, id, SwapInitAudited, 5*time.Second)
	chain.advance(2*time.Hour + time.Minute)
	waitSwap(t, maker, id, SwapRefunded, 5*time.Second)
	if b := chain.balance(42, walletOwner(maker)); b != 0 {
		t.Fatalf("maker balance %d", b)
	}
}

func TestSwapBadContract(t *testing.T) {
	_, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	chain.tamper = walletOwner(maker) // maker locks 1 atom too little
	enableFakeSwaps(t, maker, chain, nil)
	enableFakeSwaps(t, taker, chain, nil)
	id := agree(t, maker, taker)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _ := taker.SwapRecord(id)
		if r != nil && strings.Contains(r.Reason, "maker contract rejected: value") {
			if r.Status != SwapNew || r.TakerSwap != nil {
				t.Fatalf("taker acted on bad contract: %+v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rejection: %+v", r)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b := chain.balance(0, walletOwner(taker)); b != 0 {
		t.Fatalf("taker spent %d", b)
	}
}

func TestSwapResume(t *testing.T) {
	m, obs := newNegotiators(t, 2)
	maker, taker := obs[0], obs[1]
	chain := newFakeChain()
	store, err := NewSwapStore(t.TempDir(), testStoreKey)
	if err != nil {
		t.Fatal(err)
	}
	stopMaker := enableFakeSwaps(t, maker, chain, store)
	enableFakeSwaps(t, taker, chain, nil)
	_ = maker.SetSwapHalt(HaltRedeem)
	id := agree(t, maker, taker)
	waitSwap(t, maker, id, SwapPartAudited, 5*time.Second)
	// "Crash" the maker and restart from disk with the same keys.
	stopMaker()
	time.Sleep(50 * time.Millisecond)
	maker2 := NewOrderBooks(m.client(maker.self), maker.priv, maker.encPriv, tLogger)
	maker2.cfg = fastCfg
	if err := maker2.SubscribeInbox(tCtx); err != nil {
		t.Fatal(err)
	}
	owner := walletOwner(maker)
	maker2.EnableSwaps(fastSwapCfg, func(assetID uint32) (SwapWallet, error) {
		return &fakeSwapWallet{c: chain, asset: assetID, owner: owner}, nil
	}, store)
	n, err := maker2.LoadSwaps()
	if err != nil || n != 1 {
		t.Fatalf("LoadSwaps: %d %v", n, err)
	}
	ctx, cancel := context.WithCancel(tCtx)
	defer cancel()
	go maker2.RunSwaps(ctx, 20*time.Millisecond)
	waitSwap(t, maker2, id, SwapComplete, 5*time.Second)
	waitSwap(t, taker, id, SwapComplete, 5*time.Second)
	recs, _ := store.Load()
	if len(recs) != 1 || recs[0].Status != SwapComplete || recs[0].MakerRedeem == nil {
		t.Fatalf("persisted %+v", recs)
	}
}

func TestSwapMessageSizes(t *testing.T) {
	const budget = 1024 - 200
	m := newFakeMesh()
	a, b := newTestBooks(t, m, nil), newTestBooks(t, m, nil)
	var id MatchID
	coin := make([]byte, 36)
	contract := make([]byte, 140) // larger than any dcr/btc swap contract
	for name, payload := range map[string]any{
		swapMsgInit:   &SwapInitMsg{MatchID: id, CoinID: coin, Contract: contract, SecretHash: make([]byte, 32)},
		swapMsgPart:   &SwapPartMsg{MatchID: id, CoinID: coin, Contract: contract},
		swapMsgRedeem: &SwapRedeemMsg{MatchID: id, CoinID: coin, Secret: make([]byte, 32)},
		swapMsgDone:   &SwapDoneMsg{MatchID: id, CoinID: coin},
	} {
		sealed, err := a.sealInbox(b.self, b.encPriv.PubKey(), name, payload)
		if err != nil {
			t.Fatal(err)
		}
		n := len(sealed) + len(InboxTopic(b.self))
		t.Logf("%s: %d bytes", name, n)
		if n > budget {
			t.Fatalf("%s is %d bytes, budget %d", name, n, budget)
		}
	}
}

func TestCoinTxID(t *testing.T) {
	c := make([]byte, 36)
	c[0], c[31], c[35] = 0xaa, 0x01, 7
	if s := coinTxID(c); !strings.HasPrefix(s, "01") || !strings.HasSuffix(s, "aa:7") {
		t.Fatal(s)
	}
}
