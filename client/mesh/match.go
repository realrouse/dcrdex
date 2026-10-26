// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// Matching, ported from the legacy in-tree Tatanka client
// (tatanka/client/trade, #3165) onto mesh BookOrders.

import (
	"bytes"
	"encoding/binary"
	"math"

	"decred.org/dcrdex/dex/calc"
	"github.com/decred/dcrd/crypto/blake256"
)

// FeeParameters bound the fee exposure a user accepts for a match.
type FeeParameters struct {
	// MaxFeeExposure is the maximum fee losses we are willing to incur from a
	// trade, as a ratio of the trade size.
	MaxFeeExposure    float64
	BaseFeesPerMatch  uint64
	QuoteFeesPerMatch uint64
}

const (
	ReasonOurQtyTooSmall   = "our order size is less than their lot size"
	ReasonTheirQtyTooSmall = "their order size is less than our lot size"
)

// MinimumLotSize is the smallest power-of-2 lot size that keeps fee exposure
// under p.MaxFeeExposure at msgRate. See the derivation in the legacy
// tatanka/client/trade/compat.go.
func MinimumLotSize(msgRate uint64, p *FeeParameters) uint64 {
	atomicRate := float64(msgRate) / calc.RateEncodingFactor
	perfectLotSize := math.Ceil((float64(p.BaseFeesPerMatch) + float64(p.QuoteFeesPerMatch)/atomicRate) / p.MaxFeeExposure)
	if perfectLotSize <= 1 {
		return 1
	}
	n := math.Ceil(math.Log2(perfectLotSize))
	return uint64(math.Round(math.Pow(2, n)))
}

// OrderIsMatchable checks lot size compatibility of a desired quantity with a
// book order's remaining quantity.
func OrderIsMatchable(desiredQty uint64, theirOrd *BookOrder, p *FeeParameters) (_ bool, reason string) {
	// Can we satisfy their lot size?
	if desiredQty < theirOrd.LotSize {
		return false, ReasonOurQtyTooSmall
	}
	// Can they satisfy our lot size?
	if theirOrd.Remaining < MinimumLotSize(theirOrd.Rate, p) {
		return false, ReasonTheirQtyTooSmall
	}
	return true, ""
}

// DesiredTrade is what a taker wants: up to Qty base atoms at Rate or better.
type DesiredTrade struct {
	Qty  uint64
	Rate uint64
	Sell bool
}

// MatchCandidate is a book order and the quantity we'd propose against it.
type MatchCandidate struct {
	Order *BookOrder
	Qty   uint64
}

// MatchBook walks the book (as returned by Book.Orders, best price first per
// side) for orders compatible with desire, skipping orders owned by self. It
// returns proposals in price-time priority and the unmatched remainder. Each
// proposal is an integer number of the maker's lots.
func MatchBook(desire *DesiredTrade, p *FeeParameters, book []*BookOrder, self string) (matches []*MatchCandidate, remain uint64) {
	remain = desire.Qty
	for _, ord := range book {
		if remain == 0 {
			break
		}
		if ord.Sell == desire.Sell || ord.Owner == self {
			continue
		}
		// Rate compatibility. The book is sorted best first, so the first
		// incompatible rate ends the walk.
		if desire.Sell {
			if ord.Rate < desire.Rate {
				break
			}
		} else if ord.Rate > desire.Rate {
			break
		}
		if compat, _ := OrderIsMatchable(remain, ord, p); !compat {
			continue
		}
		maxQty := ord.Remaining
		if maxQty > remain {
			maxQty = remain
		}
		qty := (maxQty / ord.LotSize) * ord.LotSize
		if qty == 0 {
			continue
		}
		matches = append(matches, &MatchCandidate{Order: ord, Qty: qty})
		remain -= qty
	}
	return matches, remain
}

// MatchID identifies a match. It is chosen by the taker.
type MatchID = OrderID

// Match status values.
const (
	MatchProposed = "proposed" // taker: proposal sent, awaiting answer
	MatchAccepted = "accepted" // maker: accepted and reserved, awaiting confirm
	MatchAgreed   = "agreed"   // both: final, ready for swap (P3)
	MatchRejected = "rejected"
	MatchTimedOut = "timed_out" // taker: no answer; maker: no confirm
)

// Initiator is the swap initiator role. As in dcrdex, the maker initiates.
const Initiator = "maker"

// Match is the agreed match, identical on both sides, carrying the swap
// parameters P3 needs. Qty is base atoms, QuoteQty quote atoms, lock times are
// unix seconds. MakerAddr/TakerAddr are each side's receiving address for the
// asset it gets (maker gets quote if MakerSell, else base).
type Match struct {
	ID            MatchID `json:"id"`
	Market        Market  `json:"market"`
	MakerOrderID  OrderID `json:"makerOrderID"`
	Maker         string  `json:"maker"`
	Taker         string  `json:"taker"`
	MakerSell     bool    `json:"makerSell"`
	Qty           uint64  `json:"qty"`
	Rate          uint64  `json:"rate"`
	QuoteQty      uint64  `json:"quoteQty"`
	Stamp         int64   `json:"stamp"`
	Initiator     string  `json:"initiator"`
	MakerAddr     string  `json:"makerAddr"`
	TakerAddr     string  `json:"takerAddr"`
	MakerLockTime int64   `json:"makerLockTime"`
	TakerLockTime int64   `json:"takerLockTime"`
}

// MakerReceives and TakerReceives are the asset IDs each side receives.
func (m *Match) MakerReceives() uint32 {
	if m.MakerSell {
		return m.Market.Quote
	}
	return m.Market.Base
}

func (m *Match) TakerReceives() uint32 {
	if m.MakerSell {
		return m.Market.Base
	}
	return m.Market.Quote
}

// AgreementHash commits to every field of the match. Both sides must compute
// the same value.
func (m *Match) AgreementHash() OrderID {
	b := new(bytes.Buffer)
	b.WriteString("tatanka-match-agreement-v1")
	b.Write(m.ID[:])
	_ = binary.Write(b, binary.BigEndian, m.Market.Base)
	_ = binary.Write(b, binary.BigEndian, m.Market.Quote)
	b.Write(m.MakerOrderID[:])
	writeStr(b, m.Maker)
	writeStr(b, m.Taker)
	if m.MakerSell {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	for _, v := range []uint64{m.Qty, m.Rate, m.QuoteQty} {
		_ = binary.Write(b, binary.BigEndian, v)
	}
	for _, v := range []int64{m.Stamp, m.MakerLockTime, m.TakerLockTime} {
		_ = binary.Write(b, binary.BigEndian, v)
	}
	writeStr(b, m.Initiator)
	writeStr(b, m.MakerAddr)
	writeStr(b, m.TakerAddr)
	return blake256.Sum256(b.Bytes())
}

func computeMatchID(makerOrder OrderID, taker string, nonce uint64, stamp int64) MatchID {
	b := new(bytes.Buffer)
	b.WriteString("tatanka-match-id-v1")
	b.Write(makerOrder[:])
	writeStr(b, taker)
	_ = binary.Write(b, binary.BigEndian, nonce)
	_ = binary.Write(b, binary.BigEndian, stamp)
	return blake256.Sum256(b.Bytes())
}

// Inbox message types for negotiation.
const (
	msgMatchPropose = "match_propose"
	msgMatchAccept  = "match_accept"
	msgMatchConfirm = "match_confirm"
	msgMatchReject  = "match_reject"
)

// MatchProposal is sent taker -> maker. Qty is a multiple of the maker's lot
// size and Rate must equal the maker order's rate.
type MatchProposal struct {
	MatchID        MatchID `json:"matchID"`
	MakerOrderID   OrderID `json:"makerOrderID"`
	Taker          string  `json:"-"` // the sender
	TakerEncPubKey []byte  `json:"takerEncPubKey"`
	Qty            uint64  `json:"qty"`
	Rate           uint64  `json:"rate"`
	Stamp          int64   `json:"stamp"`
	Nonce          uint64  `json:"nonce"`
	TakerAddr      string  `json:"takerAddr"`
	MakerLockTime  int64   `json:"makerLockTime"`
	TakerLockTime  int64   `json:"takerLockTime"`
}

// MatchAccept is sent maker -> taker. Qty may be lower than proposed (partial
// acceptance when the order's available quantity shrank), never higher.
type MatchAccept struct {
	MatchID   MatchID `json:"matchID"`
	Qty       uint64  `json:"qty"`
	MakerAddr string  `json:"makerAddr"`
}

// MatchConfirm is sent taker -> maker to finalize an accepted match.
type MatchConfirm struct {
	MatchID MatchID `json:"matchID"`
	Qty     uint64  `json:"qty"`
}

// MatchReject is sent by either side to abort a match.
type MatchReject struct {
	MatchID MatchID `json:"matchID"`
	Reason  string  `json:"reason"`
}
