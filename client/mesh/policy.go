// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"errors"
	"fmt"
	"time"
)

// LockPolicy is a market's swap lock time policy: contract lock durations
// counted from the match proposal stamp. The taker computes the proposal's
// lock times from its policy for the market and the maker rejects a proposal
// whose lock times don't match its own policy, so both sides of a match
// always agree. Both lock times are then checked again when auditing.
type LockPolicy struct {
	Maker time.Duration `json:"maker"`
	Taker time.Duration `json:"taker"`
}

// Policy limits. The taker must have time to see the maker's redeem and
// redeem in turn before the maker can refund.
const (
	MinTakerLock = 30 * time.Second
	MinLockGap   = 30 * time.Second
	MaxMakerLock = 48 * time.Hour
)

// Validate checks the policy limits.
func (p LockPolicy) Validate() error {
	switch {
	case p.Taker < MinTakerLock:
		return fmt.Errorf("taker lock %s < minimum %s", p.Taker, MinTakerLock)
	case p.Maker > MaxMakerLock:
		return fmt.Errorf("maker lock %s > maximum %s", p.Maker, MaxMakerLock)
	case p.Maker-p.Taker < MinLockGap || p.Maker*2 < p.Taker*3:
		return errors.New("maker lock must be at least 1.5x the taker lock and 30s longer")
	}
	return nil
}

func (p LockPolicy) lockTimes(stampMs int64) (maker, taker int64) {
	s := stampMs / 1000
	return s + int64(p.Maker/time.Second), s + int64(p.Taker/time.Second)
}

// FallbackLockPolicy applies to markets without an entry in
// DefaultLockPolicies.
var FallbackLockPolicy = LockPolicy{Maker: 2 * time.Hour, Taker: time.Hour}

// DefaultLockPolicies are the built-in per-market policies (simnet values;
// mesh trading is simnet only for now). Slower chains get longer locks.
var DefaultLockPolicies = map[string]LockPolicy{
	"dcr_btc": {Maker: 2 * time.Hour, Taker: time.Hour},
	"lbc_btc": {Maker: 3 * time.Hour, Taker: 90 * time.Minute},
	"lbc_dcr": {Maker: 3 * time.Hour, Taker: 90 * time.Minute},
	"dcr_lbc": {Maker: 3 * time.Hour, Taker: 90 * time.Minute},
	"btc_lbc": {Maker: 3 * time.Hour, Taker: 90 * time.Minute},
}

// policyLocked returns the lock policy for a market. ob.mtx must be held.
func (ob *OrderBooks) policyLocked(m Market) LockPolicy {
	if p, ok := ob.policies[m]; ok {
		return p
	}
	if name, err := m.Name(); err == nil {
		if p, ok := DefaultLockPolicies[name]; ok {
			return p
		}
	}
	return FallbackLockPolicy
}

// LockPolicy returns the lock policy in force for a market.
func (ob *OrderBooks) LockPolicy(m Market) LockPolicy {
	ob.mtx.RLock()
	defer ob.mtx.RUnlock()
	return ob.policyLocked(m)
}

// SetLockPolicy overrides a market's lock policy. Only the simnet debug API
// uses it; both sides of a test must set the same values.
func (ob *OrderBooks) SetLockPolicy(m Market, p LockPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	ob.mtx.Lock()
	ob.policies[m] = p
	ob.mtx.Unlock()
	return nil
}
