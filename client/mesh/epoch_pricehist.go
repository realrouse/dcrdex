// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S7 engine side: trade proofs from completed swaps, their on-chain checks
// by validators, admissions in proposals, and the verified price history
// (pricehist.go).

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	dexbtc "decred.org/dcrdex/dex/networks/btc"
	dexdcr "decred.org/dcrdex/dex/networks/dcr"
	"github.com/btcsuite/btcd/btcutil"
	btcchaincfg "github.com/btcsuite/btcd/chaincfg"
	dcrchaincfg "github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
)

// TradeChain looks swap contracts up on chain (Core, with the validator's
// own wallets).
type TradeChain interface {
	// SwapConfs: confirmations and spend status of a swap contract output.
	SwapConfs(ctx context.Context, assetID uint32, coinID, contract []byte, since time.Time) (confs uint32, spent bool, err error)
	// SwapLockExpired: the asset's chain passed lockTime (a refund is
	// possible from then on).
	SwapLockExpired(ctx context.Context, assetID uint32, lockTime int64) (bool, error)
}

type tradeEntry struct {
	proof      *TradeProof
	d          *tradeDetails
	recv       int64
	verified   bool
	verifiedAt int64
	dead       bool // can never verify (lock passed before a completed check)
	checking   bool
	checkAt    int64
	err        string
}

const maxTradePool = 2000

// SetTradeChain sets the swap contract lookup (validators).
func (e *EpochEngine) SetTradeChain(c TradeChain) {
	e.mtx.Lock()
	e.tradeChain = c
	e.mtx.Unlock()
}

// PriceHistoryOn: the release takes price history only from verified trades.
func (e *EpochEngine) PriceHistoryOn() bool { return e.rel.PriceHistory != nil }

func (e *EpochEngine) noteTradeRefusedLocked(p *TradeProof, reason string) {
	if len(e.tradeRefused) >= maxTradePool {
		e.tradeRefused = make(map[string]string)
	}
	ck := p.contentKey()
	if _, have := e.tradeRefused[ck]; !have {
		e.log.Infof("Refused trade proof %s: %s", ck, reason)
	}
	e.tradeRefused[ck] = reason
}

// addTradeLocked merges a (half or full) proof into the pool (validators).
func (e *EpochEngine) addTradeLocked(p *TradeProof) {
	if e.rel.PriceHistory == nil || !e.signer || p == nil {
		return
	}
	if e.ledger.tkeys[p.Key()] {
		return // already admitted
	}
	ck := p.contentKey()
	pe := e.tradePool[ck]
	if pe != nil && (len(p.MakerSig) == 0 || len(pe.proof.MakerSig) > 0) && (len(p.TakerSig) == 0 || len(pe.proof.TakerSig) > 0) {
		return // nothing new
	}
	lm := e.ledger.matchFor(p.Key(), e.p.EpochOf(e.nowMs()))
	if lm == nil {
		e.noteTradeRefusedLocked(p, "no such finalized epoch match (or past the proof window)")
		return
	}
	d, err := checkTradeProof(p, lm.m, lm.end, false)
	if err != nil {
		e.noteTradeRefusedLocked(p, err.Error())
		return
	}
	if pe == nil {
		if len(e.tradePool) >= maxTradePool {
			return
		}
		pe = &tradeEntry{proof: p.copyUnsigned(), d: d, recv: e.nowMs()}
		e.tradePool[ck] = pe
	}
	if len(p.MakerSig) > 0 && len(pe.proof.MakerSig) == 0 {
		pe.proof.MakerSig = p.MakerSig
	}
	if len(p.TakerSig) > 0 && len(pe.proof.TakerSig) == 0 {
		pe.proof.TakerSig = p.TakerSig
	}
}

func (pe *tradeEntry) full() bool { return len(pe.proof.MakerSig) > 0 && len(pe.proof.TakerSig) > 0 }

// tradeChecks starts on-chain checks of complete pool proofs (validators).
func (e *EpochEngine) tradeChecks(ctx context.Context, nowMs int64) {
	r := e.rel.PriceHistory
	if !e.signer || r == nil {
		return
	}
	e.mtx.Lock()
	chain := e.tradeChain
	var todo []*tradeEntry
	for _, pe := range e.tradePool {
		if pe.verified || pe.dead || pe.checking || !pe.full() || nowMs < pe.checkAt {
			continue
		}
		pe.checking = true
		todo = append(todo, pe)
	}
	if chain == nil {
		for _, pe := range todo {
			pe.checking, pe.err, pe.checkAt = false, "no wallet for swap checks", nowMs+10_000
		}
		todo = nil
	}
	e.mtx.Unlock()
	for _, pe := range todo {
		go func(pe *tradeEntry) {
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			reason, dead := checkTradeOnChain(cctx, chain, r, pe.proof, pe.d)
			e.mtx.Lock()
			defer e.mtx.Unlock()
			pe.checking = false
			pe.checkAt = e.nowMs() + 2*e.p.EpochMs
			pe.dead = dead
			if reason != "" {
				pe.err = reason
				if dead {
					e.noteTradeRefusedLocked(pe.proof, reason)
				}
				return
			}
			if !pe.verified {
				m := pe.d.match
				e.log.Infof("Verified trade %s on chain: %s %d @ %d (maker %s, taker %s)", pe.proof.Key(), marketName(m.Market), pe.proof.Qty, m.Rate,
					e.nameOf(m.Maker), e.nameOf(m.Taker))
			}
			pe.verified, pe.verifiedAt, pe.err = true, e.nowMs(), ""
		}(pe)
	}
}

// checkTradeOnChain checks both contracts: confirmed, spent, and spent while
// the lock has not passed (a redemption). dead: it can never pass.
func checkTradeOnChain(ctx context.Context, chain TradeChain, r *PriceRules, p *TradeProof, d *tradeDetails) (reason string, dead bool) {
	for _, s := range []struct {
		name     string
		side     *swapSide
		coin, ct []byte
	}{{"maker", d.maker, p.MakerCoin, p.MakerContract}, {"taker", d.taker, p.TakerCoin, p.TakerContract}} {
		start := time.UnixMilli(d.end).Add(-time.Hour)
		confs, spent, err := chain.SwapConfs(ctx, s.side.AssetID, s.coin, s.ct, start)
		if err != nil {
			return fmt.Sprintf("%s contract %s: %v", s.name, coinTxID(s.coin), err), false
		}
		if confs < r.MinConfs {
			return fmt.Sprintf("%s contract %s: %d of %d confirmations", s.name, coinTxID(s.coin), confs, r.MinConfs), false
		}
		if !spent {
			return fmt.Sprintf("%s contract %s is not spent (swap not complete)", s.name, coinTxID(s.coin)), false
		}
		// Checked after the spend was seen: if the lock has not passed
		// now, the spend happened before it, so it is a redemption.
		expired, err := chain.SwapLockExpired(ctx, s.side.AssetID, s.side.LockTime)
		if err != nil {
			return fmt.Sprintf("%s lock time check: %v", s.name, err), false
		}
		if expired {
			return fmt.Sprintf("%s contract %s: lock time passed, the spend may be a refund", s.name, coinTxID(s.coin)), true
		}
	}
	return "", false
}

// tAdmissibleLocked: proof p may be admitted in epoch n.
func (e *EpochEngine) tAdmissibleLocked(p *TradeProof, n uint64) error {
	if e.ledger.tkeys[p.Key()] {
		return errors.New("already admitted")
	}
	lm := e.ledger.matchFor(p.Key(), n)
	if lm == nil {
		return errors.New("no such finalized epoch match (or past the proof window)")
	}
	if e.ledger.tcoins[hex.EncodeToString(p.MakerCoin)] || e.ledger.tcoins[hex.EncodeToString(p.TakerCoin)] {
		return errors.New("a contract coin already counted for another trade")
	}
	_, err := checkTradeProof(p, lm.m, lm.end, true)
	return err
}

// checkS7Locked is the deterministic check of a proposal's trade admissions.
func (e *EpochEngine) checkS7Locked(p *EpochProposal) error {
	if len(p.Trades) == 0 {
		return nil
	}
	if e.rel.PriceHistory == nil {
		return errors.New("trade admissions in a release without price history rules")
	}
	for _, tp := range p.Trades {
		if err := e.tAdmissibleLocked(tp, p.Epoch); err != nil {
			return fmt.Errorf("trade %s: %v", tp.Key(), err)
		}
	}
	return nil
}

// tradesVerifiedLocked: we verified every trade of p on chain ourselves.
func (e *EpochEngine) tradesVerifiedLocked(p *EpochProposal) bool {
	ok := true
	for _, tp := range p.Trades {
		pe := e.tradePool[tp.contentKey()]
		if pe == nil {
			e.addTradeLocked(tp)
			ok = false
			continue
		}
		if !pe.full() {
			e.addTradeLocked(tp)
		}
		if !pe.verified {
			ok = false
		}
	}
	return ok
}

// s7ProposalLocked fills a new proposal's trade admissions.
func (e *EpochEngine) s7ProposalLocked(p *EpochProposal) {
	r := e.rel.PriceHistory
	if r == nil {
		return
	}
	nowMs := e.nowMs()
	byKey := make(map[string]string)
	for ck, pe := range e.tradePool {
		if !pe.verified || nowMs-pe.verifiedAt < 2*e.p.EpochMs { // give the others time to check too
			continue
		}
		if min(pe.d.maker.LockTime, pe.d.taker.LockTime)*1000-nowMs < r.MinLockLeftSecs*1000 {
			continue
		}
		if e.tAdmissibleLocked(pe.proof, p.Epoch) != nil {
			continue
		}
		k := pe.proof.Key()
		if prev, have := byKey[k]; !have || ck < prev {
			byKey[k] = ck
		}
	}
	cks := make([]string, 0, len(byKey))
	for _, ck := range byKey {
		cks = append(cks, ck)
	}
	sort.Strings(cks)
	if len(cks) > r.MaxProofsPerEpoch {
		cks = cks[:r.MaxProofsPerEpoch]
	}
	for _, ck := range cks {
		p.Trades = append(p.Trades, e.tradePool[ck].proof)
	}
	sort.Slice(p.Trades, func(i, j int) bool { return p.Trades[i].Key() < p.Trades[j].Key() })
}

// afterTradesLocked runs after a record was applied to the ledger.
func (e *EpochEngine) afterTradesLocked(rec *EpochRecord) {
	if rec.Proposal == nil || len(rec.Proposal.Trades) == 0 {
		return
	}
	for _, tp := range rec.Proposal.Trades {
		for ck, pe := range e.tradePool {
			if pe.proof.Key() == tp.Key() {
				delete(e.tradePool, ck)
			}
		}
		if _, own := e.ownTrades[tp.Key()]; own {
			e.log.Infof("Our trade %s entered the verified price history in epoch %d", tp.Key(), rec.Epoch)
			delete(e.ownTrades, tp.Key())
		}
	}
}

// s7Gossip re-sends our unadmitted trade proofs every few epochs.
func (e *EpochEngine) s7Gossip(ctx context.Context, cur uint64) {
	if e.rel.PriceHistory == nil || cur%3 != 2 {
		return
	}
	e.mtx.Lock()
	if e.s7SentAt == cur {
		e.mtx.Unlock()
		return
	}
	e.s7SentAt = cur
	var msgs []*epochMsg
	for k, tp := range e.ownTrades {
		if e.ledger.tkeys[k] || e.ledger.matchFor(k, cur) == nil {
			delete(e.ownTrades, k)
			continue
		}
		msgs = append(msgs, &epochMsg{Type: epMsgTrade, Trade: tp})
	}
	e.mtx.Unlock()
	for _, m := range msgs {
		e.send(ctx, m)
	}
}

// epochMatchOf finds the finalized epoch match of a swap's match.
func (e *EpochEngine) epochMatchOf(m *Match) (uint64, int, error) {
	if m.Stamp <= 0 || m.Stamp%e.p.EpochMs != 0 {
		return 0, 0, errors.New("match is not from an epoch")
	}
	n := uint64(m.Stamp/e.p.EpochMs) - 1
	rec := e.Record(n)
	if rec == nil {
		return 0, 0, fmt.Errorf("epoch %d record not in our chain", n)
	}
	for i, em := range rec.Matches {
		if em.MakerOrderID == m.MakerOrderID && em.Taker == m.Taker &&
			computeMatchID(em.MakerOrderID, em.Taker, epochMatchNonce(n, i), m.Stamp) == m.ID {
			return n, i, nil
		}
	}
	return 0, 0, fmt.Errorf("match %s not in epoch %d", m.ID, n)
}

// SubmitTradeProof signs and gossips our proof of a completed swap. force
// (simnet adversary tests) submits it whatever the swap status; fake
// replaces missing contracts with invented ones.
func (e *EpochEngine) SubmitTradeProof(ctx context.Context, r *SwapRecord, force, fake bool) (*TradeProof, error) {
	if e.rel.PriceHistory == nil {
		return nil, nil
	}
	if !force && r.Status != SwapComplete {
		return nil, fmt.Errorf("swap %s is %s, not complete", r.Match.ID, r.Status)
	}
	n, i, err := e.epochMatchOf(&r.Match)
	if err != nil {
		return nil, err
	}
	p := &TradeProof{Epoch: n, Index: i, Qty: r.Match.Qty}
	if fake {
		p.MakerCoin, p.MakerContract, err = fakeTradeContract(&r.Match, true)
		if err == nil {
			p.TakerCoin, p.TakerContract, err = fakeTradeContract(&r.Match, false)
		}
		if err != nil {
			return nil, err
		}
	} else {
		if r.MakerSwap == nil || r.TakerSwap == nil {
			return nil, errors.New("swap has no contracts on both sides")
		}
		p.MakerCoin, p.MakerContract = r.MakerSwap.CoinID, r.MakerSwap.Contract
		p.TakerCoin, p.TakerContract = r.TakerSwap.CoinID, r.TakerSwap.Contract
	}
	sig, err := e.priv.Sign(p.msg())
	if err != nil {
		return nil, err
	}
	switch e.self {
	case r.Match.Maker:
		p.MakerSig = sig
	case r.Match.Taker:
		p.TakerSig = sig
	default:
		return nil, errors.New("we are not a party of the match")
	}
	e.mtx.Lock()
	if e.ledger.tkeys[p.Key()] {
		e.mtx.Unlock()
		return p, nil
	}
	e.ownTrades[p.Key()] = p
	e.addTradeLocked(p)
	e.mtx.Unlock()
	e.send(ctx, &epochMsg{Type: epMsgTrade, Trade: p})
	e.log.Infof("Submitted trade proof %s for swap %s (%s)", p.Key(), r.Match.ID, r.Status)
	return p, nil
}

// SubmitRawTradeProof gossips a proof as is (simnet adversary tests).
func (e *EpochEngine) SubmitRawTradeProof(ctx context.Context, p *TradeProof) {
	e.mtx.Lock()
	e.addTradeLocked(p)
	e.mtx.Unlock()
	e.send(ctx, &epochMsg{Type: epMsgTrade, Trade: p})
}

// SignTradeProof signs p as maker or taker (simnet adversary tests).
func (e *EpochEngine) SignTradeProof(p *TradeProof, maker bool) error {
	sig, err := e.priv.Sign(p.msg())
	if err != nil {
		return err
	}
	if maker {
		p.MakerSig = sig
	} else {
		p.TakerSig = sig
	}
	return nil
}

// fakeTradeContract invents a well-formed contract that is not on chain, the
// same on both sides of the match (simnet adversary tests).
func fakeTradeContract(m *Match, maker bool) (coin, contract []byte, err error) {
	asset, lock := m.Market.Quote, m.TakerLockTime
	if maker {
		lock = m.MakerLockTime
		if m.MakerSell {
			asset = m.Market.Base
		}
	} else if !m.MakerSell {
		asset = m.Market.Base
	}
	seed := func(tag string) []byte {
		h := sha256.Sum256(append([]byte("bisonary-fake-trade:"+tag), m.ID[:]...))
		return h[:]
	}
	side := "taker"
	if maker {
		side = "maker"
	}
	sh := seed("secret")
	pkr, pks := seed(side + "r")[:20], seed(side + "s")[:20]
	switch asset {
	case BondAssetBTC, BondAssetLBC:
		p := &btcchaincfg.MainNetParams
		ra, err := btcutil.NewAddressPubKeyHash(pkr, p)
		if err != nil {
			return nil, nil, err
		}
		sa, err := btcutil.NewAddressPubKeyHash(pks, p)
		if err != nil {
			return nil, nil, err
		}
		contract, err = dexbtc.MakeContract(ra, sa, sh, lock, false, p)
		if err != nil {
			return nil, nil, err
		}
	case BondAssetDCR:
		p := dcrchaincfg.MainNetParams()
		ra, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(pkr, p)
		if err != nil {
			return nil, nil, err
		}
		sa, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(pks, p)
		if err != nil {
			return nil, nil, err
		}
		contract, err = dexdcr.MakeContract(ra.String(), sa.String(), sh, lock, p)
		if err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("asset %d", asset)
	}
	coin = append(seed(side+"coin"), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(coin[32:], 0)
	return coin, contract, nil
}

// TradeProofInfo is a pool proof (status API).
type TradeProofInfo struct {
	Key      string `json:"key"`
	Content  string `json:"content"`
	Market   string `json:"market"`
	Qty      uint64 `json:"qty"`
	Rate     uint64 `json:"rate"`
	MakerSig bool   `json:"makerSig"`
	TakerSig bool   `json:"takerSig"`
	Verified bool   `json:"verified"`
	Dead     bool   `json:"dead,omitempty"`
	Err      string `json:"err,omitempty"`
	MakerTx  string `json:"makerTx"`
	TakerTx  string `json:"takerTx"`
}

// PriceHistoryStatus is the S7 state (debug API).
type PriceHistoryStatus struct {
	On       bool                         `json:"on"`
	Rules    *PriceRules                  `json:"rules,omitempty"`
	AsOf     uint64                       `json:"asOf"`
	Verified int                          `json:"verified"`
	Markets  map[string]*MarketPriceState `json:"markets"`
	Pool     []*TradeProofInfo            `json:"pool"`
	Refused  map[string]string            `json:"refused"`
	Own      []string                     `json:"ownPending"`
	Hash     string                       `json:"hash"` // over every market's trades and candles
}

// MarketPriceState is a market's verified trades and candles.
type MarketPriceState struct {
	Trades      []*VerifiedTrade  `json:"trades"`
	Candles     []*VerifiedCandle `json:"candles"`
	CandlesHash string            `json:"candlesHash"`
}

// VerifiedTrades returns a market's verified trades, oldest first.
func (e *EpochEngine) VerifiedTrades(m Market) []*VerifiedTrade {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	return e.ledger.verifiedTrades(m)
}

// PriceHistoryStatus reports the verified history (candles of binSize).
func (e *EpochEngine) PriceHistoryStatus(binSize time.Duration) *PriceHistoryStatus {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	out := &PriceHistoryStatus{On: e.rel.PriceHistory != nil, Rules: e.rel.PriceHistory, Markets: make(map[string]*MarketPriceState),
		Pool: []*TradeProofInfo{}, Refused: make(map[string]string), Own: []string{}}
	out.AsOf, _ = e.lastEpochLocked()
	out.Verified = e.ledger.vcount
	h := sha256.New()
	var names []string
	byName := make(map[string]Market)
	for m := range e.ledger.vtrades {
		names = append(names, marketName(m))
		byName[marketName(m)] = m
	}
	sort.Strings(names)
	for _, name := range names {
		ts := e.ledger.verifiedTrades(byName[name])
		cs := BuildVerifiedCandles(ts, binSize)
		ms := &MarketPriceState{Trades: ts, Candles: cs, CandlesHash: CandlesHash(cs)}
		out.Markets[name] = ms
		h.Write([]byte(name))
		for _, t := range ts {
			fmt.Fprintf(h, "|%d:%d:%d:%d:%d:%d:%s:%s", t.Epoch, t.Index, t.Rate, t.Qty, t.Stamp, t.Admitted, t.MakerTx, t.TakerTx)
		}
		h.Write([]byte(ms.CandlesHash))
	}
	out.Hash = hex.EncodeToString(h.Sum(nil))
	for ck, pe := range e.tradePool {
		m := pe.d.match
		out.Pool = append(out.Pool, &TradeProofInfo{Key: pe.proof.Key(), Content: ck, Market: marketName(m.Market), Qty: pe.proof.Qty, Rate: m.Rate,
			MakerSig: len(pe.proof.MakerSig) > 0, TakerSig: len(pe.proof.TakerSig) > 0, Verified: pe.verified, Dead: pe.dead, Err: pe.err,
			MakerTx: coinTxID(pe.proof.MakerCoin), TakerTx: coinTxID(pe.proof.TakerCoin)})
	}
	sort.Slice(out.Pool, func(i, j int) bool { return out.Pool[i].Content < out.Pool[j].Content })
	for k, v := range e.tradeRefused {
		out.Refused[k] = v
	}
	for k := range e.ownTrades {
		out.Own = append(out.Own, k)
	}
	sort.Strings(out.Own)
	return out
}

// SetSwapSource lets the engine find our completed swaps (OrderBooks), so
// proofs are (re)submitted after a restart or a missed gossip.
func (e *EpochEngine) SetSwapSource(f func() []*SwapRecord) {
	e.mtx.Lock()
	e.swapSource = f
	e.mtx.Unlock()
}

// s7Scan submits proofs of our completed epoch swaps that are not admitted
// yet, every 15 epochs.
func (e *EpochEngine) s7Scan(ctx context.Context, cur uint64) {
	if e.rel.PriceHistory == nil || cur%15 != 7 {
		return
	}
	e.mtx.Lock()
	src := e.swapSource
	if src == nil || e.s7ScanAt == cur {
		e.mtx.Unlock()
		return
	}
	e.s7ScanAt = cur
	cutoff := e.nowMs() - e.rel.PriceHistory.MaxAgeSecs*1000
	e.mtx.Unlock()
	go func() {
		for _, r := range src() {
			if r.Status != SwapComplete || r.Match.Stamp < cutoff {
				continue
			}
			n, i, err := e.epochMatchOf(&r.Match)
			if err != nil {
				continue
			}
			k := tradeKey(n, i)
			e.mtx.Lock()
			skip := e.ledger.tkeys[k] || e.ownTrades[k] != nil
			e.mtx.Unlock()
			if skip {
				continue
			}
			if _, err := e.SubmitTradeProof(ctx, r, false, false); err != nil {
				e.log.Debugf("Trade proof for swap %s: %v", r.Match.ID, err)
			}
		}
	}()
}
