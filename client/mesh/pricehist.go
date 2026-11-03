// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// S7: verified price history.
//
// With PriceHistory rules in the release, the price history (candles, spot
// prices) of a market comes only from trades whose swaps completed on chain,
// admitted in finalized epoch records:
//
//   - When a swap of a finalized epoch match completes, the maker and the
//     taker each sign the same TradeProof: the epoch match (epoch, index), the
//     swapped quantity, and both swap contracts and their coins. Proofs are
//     gossiped on the epoch topic. A proof signed by only one side is never
//     admitted, so neither side can report a trade alone.
//   - Validators check a complete proof against the match in their finalized
//     chain (participants, quantity, assets, one secret hash, lock times
//     after the match) and then on chain with their own wallets: both
//     contract outputs must exist with enough confirmations and be spent while
//     their lock times have not yet passed. A swap contract can only be
//     refunded after its lock time, so a spend seen before the lock is a
//     redemption: both sides redeemed, the swap completed. Unswapped matches
//     (no contracts), abandoned or refunded swaps, and invented reports fail
//     this check. Each contract coin counts for one trade only.
//   - Proposers put verified proofs into proposals and validators sign a
//     proposal with proofs only after their own chain check (as for S6
//     trading bonds). Once a record admits a proof, every peer derives the
//     same trade from the finalized chain: the rate is the epoch match's rate
//     (never a reported one), the stamp is the match epoch's end. Candles are
//     built from these trades only, so every peer computes the same candles.
//   - Self trades (maker owner = taker owner) are refused; matches below the
//     market's standard lot (dust) are refused.
//
// Limits: two colluding traders can still trade with each other on chain at
// a rate they agreed (wash trading); that costs real swaps and fees, needs
// two counted trading bonds, and moves only candles of bins they trade in.
// The contract values are not read from chain (wallets look outputs up by
// script, not value); each side audited the other's contract value during
// the swap and signs the proof. Only BTC, LBC and DCR swaps are verified;
// USDC.polygon trades do not enter the verified history yet.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/msgjson"
	dexbtc "decred.org/dcrdex/dex/networks/btc"
	dexdcr "decred.org/dcrdex/dex/networks/dcr"
	btcchaincfg "github.com/btcsuite/btcd/chaincfg"
	dcrchaincfg "github.com/decred/dcrd/chaincfg/v3"
)

// PriceRules are the release-defined S7 rules.
type PriceRules struct {
	// MinConfs: confirmations a validator needs on each swap contract.
	MinConfs uint32 `json:"minConfs"`
	// MaxProofsPerEpoch caps the trade admissions in one proposal.
	MaxProofsPerEpoch int `json:"maxProofsPerEpoch"`
	// MaxAgeSecs: a match's proof must be admitted within this long after
	// the match epoch (the match is forgotten after it).
	MaxAgeSecs int64 `json:"maxAgeSecs"`
	// MinLockLeftSecs: a proposer includes a proof only while both swap
	// contracts have at least this much lock left, so the other validators
	// can still tell a redemption from a refund when they check.
	MinLockLeftSecs int64 `json:"minLockLeftSecs"`
}

// Validate checks the rules.
func (r *PriceRules) Validate() error {
	switch {
	case r.MaxProofsPerEpoch <= 0:
		return errors.New("maxProofsPerEpoch must be positive")
	case r.MaxAgeSecs <= 0:
		return errors.New("maxAgeSecs must be positive")
	case r.MinLockLeftSecs < 0:
		return errors.New("negative minLockLeftSecs")
	}
	return nil
}

// DefaultPriceRules are a PROPOSAL for release 1.
var DefaultPriceRules = PriceRules{MinConfs: 1, MaxProofsPerEpoch: 16, MaxAgeSecs: 24 * 3600, MinLockLeftSecs: 120}

const tradeProofDomain = "bisonary-trade-proof-v1:"

// TradeProof is the maker's and taker's signed statement that the swap of a
// finalized epoch match completed with these contracts.
type TradeProof struct {
	Epoch         uint64 `json:"e"`
	Index         int    `json:"i"`
	Qty           uint64 `json:"q"`
	MakerCoin     []byte `json:"mc"`
	MakerContract []byte `json:"mk"`
	TakerCoin     []byte `json:"tc"`
	TakerContract []byte `json:"tk"`
	MakerSig      []byte `json:"ms,omitempty"`
	TakerSig      []byte `json:"ts,omitempty"`
}

func (p *TradeProof) msg() []byte {
	b := new(bytes.Buffer)
	b.WriteString(tradeProofDomain)
	_ = binary.Write(b, binary.BigEndian, p.Epoch)
	_ = binary.Write(b, binary.BigEndian, uint32(p.Index))
	_ = binary.Write(b, binary.BigEndian, p.Qty)
	for _, x := range [][]byte{p.MakerCoin, p.MakerContract, p.TakerCoin, p.TakerContract} {
		_ = binary.Write(b, binary.BigEndian, uint16(len(x)))
		b.Write(x)
	}
	return b.Bytes()
}

// Key identifies the epoch match.
func (p *TradeProof) Key() string { return tradeKey(p.Epoch, p.Index) }

func tradeKey(epoch uint64, index int) string { return fmt.Sprintf("%d:%d", epoch, index) }

// contentKey identifies the signed content (both sides must sign the same).
func (p *TradeProof) contentKey() string {
	h := sha256.Sum256(p.msg())
	return p.Key() + ":" + hex.EncodeToString(h[:8])
}

func (p *TradeProof) write(b *bytes.Buffer) {
	b.Write(p.msg())
	for _, s := range [][]byte{p.MakerSig, p.TakerSig} {
		_ = binary.Write(b, binary.BigEndian, uint16(len(s)))
		b.Write(s)
	}
}

func (p *TradeProof) copyUnsigned() *TradeProof {
	c := *p
	c.MakerSig, c.TakerSig = nil, nil
	return &c
}

// swapSide are the parsed details of one swap contract.
type swapSide struct {
	AssetID    uint32
	LockTime   int64
	SecretHash []byte
}

// parseSwapContract reads the lock time and secret hash of a BTC, LBC or
// DCR swap contract script (chain independent).
func parseSwapContract(assetID uint32, coinID, contract []byte) (*swapSide, error) {
	if len(coinID) != 36 {
		return nil, fmt.Errorf("asset %d: coin ID is %d bytes, want 36", assetID, len(coinID))
	}
	var lock uint64
	var sh []byte
	var err error
	switch assetID {
	case BondAssetBTC, BondAssetLBC:
		// The chain parameters only shape the returned addresses.
		_, _, lock, sh, err = dexbtc.ExtractSwapDetails(contract, assetID == BondAssetBTC, &btcchaincfg.MainNetParams)
	case BondAssetDCR:
		_, _, lock, sh, err = dexdcr.ExtractSwapDetails(contract, dcrchaincfg.MainNetParams())
	default:
		return nil, fmt.Errorf("asset %d swaps are not verified on chain", assetID)
	}
	if err != nil {
		return nil, fmt.Errorf("asset %d: bad swap contract: %w", assetID, err)
	}
	return &swapSide{AssetID: assetID, LockTime: int64(lock), SecretHash: sh}, nil
}

// tradeDetails are the statically checked parts of a proof.
type tradeDetails struct {
	maker, taker *swapSide
	match        *EpochMatch
	end          int64 // match epoch end (ms)
}

// maxLockAfterMatch bounds the swap lock times after the match epoch end.
const maxLockAfterMatch = 48 * time.Hour

// checkTradeProof is the chain-independent check of a proof against the
// finalized epoch match m (epochEnd ms is the match epoch's end). Both
// signatures are required when full.
func checkTradeProof(p *TradeProof, m *EpochMatch, epochEnd int64, full bool) (*tradeDetails, error) {
	if m == nil {
		return nil, errors.New("no such finalized epoch match")
	}
	if m.Maker == m.Taker {
		return nil, errors.New("self trade")
	}
	if p.Qty == 0 || p.Qty > m.Qty {
		return nil, fmt.Errorf("quantity %d is not within the match's %d", p.Qty, m.Qty)
	}
	if d, ok := MarketDefault(marketName(m.Market)); ok && p.Qty < d.LotSize {
		return nil, fmt.Errorf("dust: %d below the market lot %d", p.Qty, d.LotSize)
	}
	ma, ta := m.Market.Quote, m.Market.Base // maker buys: sends quote
	if m.MakerSell {
		ma, ta = m.Market.Base, m.Market.Quote
	}
	mk, err := parseSwapContract(ma, p.MakerCoin, p.MakerContract)
	if err != nil {
		return nil, fmt.Errorf("maker contract: %w", err)
	}
	tk, err := parseSwapContract(ta, p.TakerCoin, p.TakerContract)
	if err != nil {
		return nil, fmt.Errorf("taker contract: %w", err)
	}
	if bytes.Equal(p.MakerCoin, p.TakerCoin) {
		return nil, errors.New("one coin for both contracts")
	}
	if !bytes.Equal(mk.SecretHash, tk.SecretHash) {
		return nil, errors.New("the contracts have different secret hashes")
	}
	es := epochEnd / 1000
	if tk.LockTime <= es || mk.LockTime <= tk.LockTime || mk.LockTime > es+int64(maxLockAfterMatch/time.Second) {
		return nil, fmt.Errorf("lock times %d (maker) and %d (taker) do not fit a swap of a match at %d", mk.LockTime, tk.LockTime, es)
	}
	msg := p.msg()
	if len(p.MakerSig) > 0 || full {
		if err := verifyByID(m.Maker, msg, p.MakerSig); err != nil {
			return nil, fmt.Errorf("maker signature: %w", err)
		}
	}
	if len(p.TakerSig) > 0 || full {
		if err := verifyByID(m.Taker, msg, p.TakerSig); err != nil {
			return nil, fmt.Errorf("taker signature: %w", err)
		}
	}
	return &tradeDetails{maker: mk, taker: tk, match: m, end: epochEnd}, nil
}

// VerifiedTrade is a trade in the verified price history.
type VerifiedTrade struct {
	Market   Market `json:"market"`
	Epoch    uint64 `json:"epoch"` // match epoch
	Index    int    `json:"index"`
	Rate     uint64 `json:"rate"`
	Qty      uint64 `json:"qty"`
	Stamp    int64  `json:"stamp"`    // match epoch end, unix ms
	Admitted uint64 `json:"admitted"` // admitting record
	Maker    string `json:"maker"`
	Taker    string `json:"taker"`
	MakerTx  string `json:"makerTx"`
	TakerTx  string `json:"takerTx"`
}

// ---------------- ledger part (bondLedger in bonds.go) ----------------

type ledgerMatch struct {
	m   *EpochMatch
	end int64
}

// maxVerifiedPerMarket caps the verified trades kept per market.
const maxVerifiedPerMarket = 20_000

func (l *bondLedger) prules() *PriceRules { return l.rel.PriceHistory }

// applyTrades admits a record's trade proofs, then remembers its matches.
func (l *bondLedger) applyTrades(rec *EpochRecord) {
	r := l.prules()
	if r == nil {
		return
	}
	p := &l.rel.Params
	if rec.Proposal != nil {
		for _, tp := range rec.Proposal.Trades {
			lm := l.matches[tp.Key()]
			if lm == nil || l.tcoins[hex.EncodeToString(tp.MakerCoin)] || l.tcoins[hex.EncodeToString(tp.TakerCoin)] {
				continue // a finalized record passed the same check; defensive
			}
			if _, err := checkTradeProof(tp, lm.m, lm.end, true); err != nil {
				continue
			}
			l.tcoins[hex.EncodeToString(tp.MakerCoin)] = true
			l.tcoins[hex.EncodeToString(tp.TakerCoin)] = true
			l.tkeys[tp.Key()] = true
			delete(l.matches, tp.Key())
			vt := &VerifiedTrade{Market: lm.m.Market, Epoch: tp.Epoch, Index: tp.Index, Rate: lm.m.Rate, Qty: tp.Qty, Stamp: lm.end,
				Admitted: rec.Epoch, Maker: lm.m.Maker, Taker: lm.m.Taker, MakerTx: coinTxID(tp.MakerCoin), TakerTx: coinTxID(tp.TakerCoin)}
			ts := append(l.vtrades[lm.m.Market], vt)
			if len(ts) > maxVerifiedPerMarket {
				ts = ts[len(ts)-maxVerifiedPerMarket:]
			}
			l.vtrades[lm.m.Market] = ts
			l.vcount++
		}
	}
	end := p.EpochEnd(rec.Epoch)
	for i, m := range rec.Matches {
		l.matches[tradeKey(rec.Epoch, i)] = &ledgerMatch{m: m, end: end}
	}
	// Forget matches past the proof window (deterministic: by record epoch).
	cut := end - r.MaxAgeSecs*1000
	if rec.Epoch%16 == 0 {
		for k, lm := range l.matches {
			if lm.end < cut {
				delete(l.matches, k)
			}
		}
	}
}

// matchFor is the remembered epoch match for key, if still in the window
// for an admission in epoch n.
func (l *bondLedger) matchFor(key string, n uint64) *ledgerMatch {
	lm := l.matches[key]
	if lm == nil || l.rel.Params.EpochEnd(n)-lm.end > l.prules().MaxAgeSecs*1000 {
		return nil
	}
	return lm
}

// verifiedTrades returns a market's verified trades, oldest first.
func (l *bondLedger) verifiedTrades(m Market) []*VerifiedTrade {
	ts := l.vtrades[m]
	out := make([]*VerifiedTrade, len(ts))
	copy(out, ts)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Stamp != out[j].Stamp {
			return out[i].Stamp < out[j].Stamp
		}
		return out[i].Index < out[j].Index
	})
	return out
}

// TradeSummaries converts verified trades for the candle builders.
func TradeSummaries(ts []*VerifiedTrade) []*TradeSummary {
	out := make([]*TradeSummary, 0, len(ts))
	for _, t := range ts {
		s := &TradeSummary{Market: t.Market, Rate: t.Rate, Qty: t.Qty, Stamp: t.Stamp, Maker: t.Maker, Taker: t.Taker}
		s.MatchID = computeMatchID(OrderID{}, t.Taker, epochMatchNonce(t.Epoch, t.Index), t.Stamp)
		out = append(out, s)
	}
	return out
}

// VerifiedCandle is a candle with the median trade rate of its bin.
type VerifiedCandle struct {
	msgjson.Candle
	MedianRate uint64 `json:"medianRate"`
	Trades     int    `json:"trades"`
}

// BuildVerifiedCandles builds candles of binSize from verified trades
// (oldest first), with each bin's median rate.
func BuildVerifiedCandles(ts []*VerifiedTrade, binSize time.Duration) []*VerifiedCandle {
	binMs := binSize.Milliseconds()
	var out []*VerifiedCandle
	var rates []uint64
	flush := func() {
		if len(out) == 0 {
			return
		}
		sort.Slice(rates, func(i, j int) bool { return rates[i] < rates[j] })
		c := out[len(out)-1]
		n := len(rates)
		if n%2 == 1 {
			c.MedianRate = rates[n/2]
		} else {
			c.MedianRate = (rates[n/2-1] + rates[n/2]) / 2
		}
		rates = rates[:0]
	}
	for _, t := range ts {
		start := uint64(t.Stamp / binMs * binMs)
		if len(out) == 0 || out[len(out)-1].StartStamp != start {
			flush()
			out = append(out, &VerifiedCandle{Candle: msgjson.Candle{StartStamp: start, EndStamp: uint64(t.Stamp),
				HighRate: t.Rate, LowRate: t.Rate, StartRate: t.Rate, EndRate: t.Rate}})
		}
		c := out[len(out)-1]
		c.EndStamp, c.EndRate = uint64(t.Stamp), t.Rate
		c.MatchVolume += t.Qty
		c.QuoteVolume += calc.BaseToQuote(t.Rate, t.Qty)
		if t.Rate > c.HighRate {
			c.HighRate = t.Rate
		}
		if t.Rate < c.LowRate {
			c.LowRate = t.Rate
		}
		c.Trades++
		rates = append(rates, t.Rate)
	}
	flush()
	return out
}

// CandlesHash commits to a candle list (peers compare it).
func CandlesHash(cs []*VerifiedCandle) string {
	b := new(bytes.Buffer)
	for _, c := range cs {
		for _, v := range []uint64{c.StartStamp, c.EndStamp, c.MatchVolume, c.QuoteVolume, c.HighRate, c.LowRate, c.StartRate, c.EndRate, c.MedianRate, uint64(c.Trades)} {
			_ = binary.Write(b, binary.BigEndian, v)
		}
	}
	h := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(h[:])
}
