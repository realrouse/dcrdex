// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"decred.org/dcrdex/client/core"
)

func TestPersistInventorySkew(t *testing.T) {
	const (
		baseID     = uint32(42)
		quoteID    = uint32(0)
		basis      = uint64(100_000)
		lotSize    = uint64(1e8)
		quotedLots = uint64(10)
		allocation = lotSize * 100
		botID      = "dex.example.com:7232-42-0"
	)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newBot := func(statePath string, current uint64, persist *bool, now *time.Time) *basicMarketMaker {
		mm := &basicMarketMaker{
			unifiedExchangeAdaptor: mustParseAdaptorFromMarket(&core.Market{
				RateStep:   1,
				AtomToConv: 1,
				LotSize:    lotSize,
				BaseID:     baseID,
				QuoteID:    quoteID,
			}),
			skewStatePath: statePath,
			nowFn:         func() time.Time { return *now },
		}
		mm.botID = botID
		mm.initialBalances = map[uint32]uint64{baseID: allocation}
		mm.inventoryMods = map[uint32]int64{}
		mm.baseDexBalances[baseID] = int64(current)
		mm.botCfgV.Store(&BotConfig{
			BasicMMConfig: &BasicMarketMakingConfig{
				GapStrategy:          GapStrategyPercentPlus,
				SellPlacements:       []*OrderPlacement{{Lots: quotedLots, GapFactor: 0.01}},
				BuyPlacements:        []*OrderPlacement{{Lots: quotedLots, GapFactor: 0.01}},
				InventorySkew:        floatPtr(1),
				InventorySkewCap:     floatPtr(0.03),
				PersistInventorySkew: persist,
			},
		})
		return mm
	}

	readState := func(t *testing.T, path string) *skewStateFile {
		t.Helper()
		f, err := readSkewStateFile(path)
		if err != nil {
			t.Fatalf("read state: %v", err)
		}
		return f
	}

	t.Run("survives restart", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), inventorySkewStateFile)
		now := start
		// Run 1: bought 5 lots.
		run1 := newBot(path, allocation+5*lotSize, boolPtr(true), &now)
		run1.restoreSkewState()
		skewed1 := run1.applyInventorySkew(basis)
		if skewed1 >= basis {
			t.Fatalf("expected bids/asks to slide down after buying, got %d", skewed1)
		}
		run1.maybeSaveSkewState(true)
		st := readState(t, path).Bots[botID]
		if st == nil || st.BaseDeviation != int64(5*lotSize) {
			t.Fatalf("saved state %+v, want deviation %d", st, 5*lotSize)
		}

		// Run 2: restarts with the allocation as its balance.
		run2 := newBot(path, allocation, boolPtr(true), &now)
		if got := run2.applyInventorySkew(basis); got != basis {
			t.Fatalf("before restore skew should be zero, got %d", got)
		}
		run2.restoreSkewState()
		if got := run2.targetBaseInventory(); got != allocation-5*lotSize {
			t.Fatalf("target %d, want %d", got, allocation-5*lotSize)
		}
		if got := run2.applyInventorySkew(basis); got != skewed1 {
			t.Fatalf("restored skew %d, want %d", got, skewed1)
		}
		// A config update restarts the loop on the same balances. The
		// deviation must not be counted twice.
		run2.maybeSaveSkewState(true)
		run2.restoreSkewState()
		if got := run2.applyInventorySkew(basis); got != skewed1 {
			t.Fatalf("second restore changed skew: %d, want %d", got, skewed1)
		}
		if got := run2.baseDeviation(); got != int64(5*lotSize) {
			t.Fatalf("deviation %d, want %d", got, 5*lotSize)
		}
	})

	t.Run("off by default", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), inventorySkewStateFile)
		now := start
		run1 := newBot(path, allocation+5*lotSize, nil, &now)
		run1.restoreSkewState()
		run1.maybeSaveSkewState(true)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("state file written while disabled: %v", err)
		}
		// Even with a saved state on disk, a disabled bot ignores it.
		if err := saveBotSkewState(path, botID, &botSkewState{BaseDeviation: int64(5 * lotSize)}); err != nil {
			t.Fatal(err)
		}
		run2 := newBot(path, allocation, boolPtr(false), &now)
		run2.restoreSkewState()
		if got := run2.applyInventorySkew(basis); got != basis {
			t.Fatalf("disabled bot skewed to %d", got)
		}
	})

	t.Run("disable mid-run drops carry", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), inventorySkewStateFile)
		now := start
		if err := saveBotSkewState(path, botID, &botSkewState{BaseDeviation: -int64(3 * lotSize)}); err != nil {
			t.Fatal(err)
		}
		mm := newBot(path, allocation, boolPtr(true), &now)
		mm.restoreSkewState()
		if mm.targetBaseInventory() != allocation+3*lotSize {
			t.Fatalf("carry not applied")
		}
		mm.cfg().PersistInventorySkew = boolPtr(false)
		if mm.targetBaseInventory() != allocation {
			t.Fatalf("carry still applied after disabling")
		}
	})

	t.Run("save throttle and other bots kept", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), inventorySkewStateFile)
		now := start
		other := &botSkewState{BaseDeviation: 77, Updated: 1}
		if err := saveBotSkewState(path, "other-1-2", other); err != nil {
			t.Fatal(err)
		}
		mm := newBot(path, allocation+lotSize, boolPtr(true), &now)
		mm.restoreSkewState()
		mm.maybeSaveSkewState(false)
		if got := readState(t, path).Bots[botID].BaseDeviation; got != int64(lotSize) {
			t.Fatalf("first save %d", got)
		}
		mm.baseDexBalances[baseID] = int64(allocation + 2*lotSize)
		now = start.Add(skewStateSaveInterval / 2)
		mm.maybeSaveSkewState(false)
		if got := readState(t, path).Bots[botID].BaseDeviation; got != int64(lotSize) {
			t.Fatalf("throttled save wrote %d", got)
		}
		now = start.Add(skewStateSaveInterval)
		mm.maybeSaveSkewState(false)
		f := readState(t, path)
		if got := f.Bots[botID].BaseDeviation; got != int64(2*lotSize) {
			t.Fatalf("save after interval %d", got)
		}
		if got := f.Bots["other-1-2"]; got == nil || got.BaseDeviation != 77 {
			t.Fatalf("other bot entry lost: %+v", got)
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatal("temp file left behind")
		}
	})

	t.Run("carry larger than allocation saturates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), inventorySkewStateFile)
		now := start
		if err := saveBotSkewState(path, botID, &botSkewState{BaseDeviation: int64(2 * allocation)}); err != nil {
			t.Fatal(err)
		}
		mm := newBot(path, allocation, boolPtr(true), &now)
		mm.restoreSkewState()
		if got := mm.targetBaseInventory(); got != 1 {
			t.Fatalf("target %d, want 1", got)
		}
		want := steppedRate(uint64(float64(basis)*(1-0.03)), 1)
		if got := mm.applyInventorySkew(basis); got != want {
			t.Fatalf("got %d, want saturated %d", got, want)
		}
	})

	t.Run("corrupt file is not clobbered", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), inventorySkewStateFile)
		now := start
		if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
			t.Fatal(err)
		}
		mm := newBot(path, allocation+lotSize, boolPtr(true), &now)
		mm.restoreSkewState()
		if mm.skewCarry.Load() != 0 {
			t.Fatal("carry from corrupt file")
		}
		mm.maybeSaveSkewState(true)
		b, _ := os.ReadFile(path)
		if string(b) != "{not json" {
			t.Fatalf("corrupt file overwritten: %q", b)
		}
	})

	t.Run("config json", func(t *testing.T) {
		var cfg BasicMarketMakingConfig
		if err := json.Unmarshal([]byte(`{"gapStrategy":"absolute-plus"}`), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.PersistInventorySkew != nil {
			t.Fatal("absent field should stay nil (off)")
		}
		b, _ := json.Marshal(&cfg)
		if strings.Contains(string(b), "persistInventorySkew") {
			t.Fatalf("absent field should be omitted: %s", b)
		}
		cfg.PersistInventorySkew = boolPtr(true)
		cp := cfg.copy()
		if cp.PersistInventorySkew == cfg.PersistInventorySkew || !*cp.PersistInventorySkew {
			t.Fatal("copy should clone PersistInventorySkew")
		}
	})
}
