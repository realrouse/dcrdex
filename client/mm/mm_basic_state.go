// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// inventorySkewStateFile is the name of the file, in the market making data
// directory (the directory holding the event log DB, e.g.
// <appdata>/mainnet), where basic market makers with PersistInventorySkew
// enabled keep their inventory deviation between runs.
const inventorySkewStateFile = "mm_inventory_skew.json"

// skewStateSaveInterval is the minimum time between two writes of the skew
// state while the bot is running. The state is also written when the bot
// stops.
const skewStateSaveInterval = time.Minute

// botSkewState is the persisted inventory skew state of one bot.
type botSkewState struct {
	// BaseDeviation is the bot's base asset inventory minus its skew target,
	// in atoms, when the state was saved. Positive means the bot held more
	// base than its target (it had been buying).
	BaseDeviation int64 `json:"baseDeviation"`
	// Updated is when the state was saved, in unix seconds.
	Updated int64 `json:"updated"`
}

type skewStateFile struct {
	Bots map[string]*botSkewState `json:"bots"`
}

// skewStateMtx serializes read-modify-write of the shared state file across
// bots.
var skewStateMtx sync.Mutex

func readSkewStateFile(path string) (*skewStateFile, error) {
	f := &skewStateFile{Bots: make(map[string]*botSkewState)}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("error parsing %s: %w", path, err)
	}
	if f.Bots == nil {
		f.Bots = make(map[string]*botSkewState)
	}
	return f, nil
}

// loadBotSkewState returns the saved state for botID, or nil if there is
// none.
func loadBotSkewState(path, botID string) (*botSkewState, error) {
	skewStateMtx.Lock()
	defer skewStateMtx.Unlock()
	f, err := readSkewStateFile(path)
	if err != nil {
		return nil, err
	}
	return f.Bots[botID], nil
}

// saveBotSkewState writes the state for botID, keeping the other bots'
// entries. The file is replaced atomically.
func saveBotSkewState(path, botID string, st *botSkewState) error {
	skewStateMtx.Lock()
	defer skewStateMtx.Unlock()
	f, err := readSkewStateFile(path)
	if err != nil {
		// Do not clobber a file we cannot parse.
		return err
	}
	f.Bots[botID] = st
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *basicMarketMaker) persistSkewEnabled() bool {
	cfg := m.cfg()
	return m.skewStatePath != "" && cfg != nil && cfg.PersistInventorySkew != nil && *cfg.PersistInventorySkew
}

// baseDeviation is the current base inventory minus the skew target.
func (m *basicMarketMaker) baseDeviation() int64 {
	return int64(m.currentBaseInventory()) - int64(m.targetBaseInventory())
}

// restoreSkewState loads the deviation saved by the previous run, so that
// inventory accumulated before a restart keeps producing skew. The bot
// restarts with its configured allocation as its balance, so the target is
// shifted by the saved deviation instead.
//
// It only runs on the first start of the bot loop. Config updates pause and
// restart the loop on the same balances, and restoring again would count
// the deviation twice. If PersistInventorySkew is first enabled by such an
// update, tracking starts from this run's start and the next real start
// restores it.
func (m *basicMarketMaker) restoreSkewState() {
	if !m.skewRestoreTried.CompareAndSwap(false, true) {
		return
	}
	if !m.persistSkewEnabled() {
		return
	}
	st, err := loadBotSkewState(m.skewStatePath, m.botID)
	if err != nil {
		m.log.Errorf("inventory skew: could not read saved state, starting without it: %v", err)
		return
	}
	m.skewSaveMtx.Lock()
	defer m.skewSaveMtx.Unlock()
	if st == nil {
		m.log.Infof("inventory skew: no saved state for %s, starting from the allocation", m.botID)
		return
	}
	m.skewCarry.Store(st.BaseDeviation)
	m.lastSavedDeviation = st.BaseDeviation
	m.log.Infof("inventory skew: restored base deviation %s%s saved %s",
		signStr(st.BaseDeviation), m.fmtBase(absInt64(st.BaseDeviation)),
		time.Unix(st.Updated, 0).Format(time.RFC3339))
}

// maybeSaveSkewState writes the current deviation if it changed and enough
// time has passed since the last write, or unconditionally if force is set.
func (m *basicMarketMaker) maybeSaveSkewState(force bool) {
	if !m.persistSkewEnabled() || m.unifiedExchangeAdaptor == nil {
		return
	}
	dev := m.baseDeviation()
	now := m.now()
	m.skewSaveMtx.Lock()
	defer m.skewSaveMtx.Unlock()
	if dev == m.lastSavedDeviation && !m.lastSkewSave.IsZero() {
		return
	}
	if !force && !m.lastSkewSave.IsZero() && now.Sub(m.lastSkewSave) < skewStateSaveInterval {
		return
	}
	err := saveBotSkewState(m.skewStatePath, m.botID, &botSkewState{
		BaseDeviation: dev,
		Updated:       now.Unix(),
	})
	if err != nil {
		m.log.Errorf("inventory skew: could not save state: %v", err)
		return
	}
	m.lastSavedDeviation = dev
	m.lastSkewSave = now
}

func absInt64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

func signStr(v int64) string {
	if v < 0 {
		return "-"
	}
	return "+"
}

// skewStatePath returns the skew state file path for a market making data
// directory, or "" if there is none.
func skewStatePathForDir(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, inventorySkewStateFile)
}
