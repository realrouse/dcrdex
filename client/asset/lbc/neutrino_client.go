package lbc

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btclog"
	"github.com/lbryio/lbcd/chaincfg"
	"github.com/lbryio/lbcd/chaincfg/chainhash"
	"github.com/lbryio/lbcd/rpcclient"
	"github.com/lbryio/lbcd/txscript"
	"github.com/lbryio/lbcd/wire"
	btcutil "github.com/lbryio/lbcutil"
	"github.com/lbryio/lbcutil/gcs"
	"github.com/lbryio/lbcutil/gcs/builder"
	"github.com/lbryio/lbcwallet/chain"
	"github.com/lbryio/lbcwallet/waddrmgr"
	"github.com/lbryio/lbcwallet/wtxmgr"
	"github.com/realrouse/lbcd-spv/neutrino"
	"github.com/realrouse/lbcd-spv/neutrino/headerfs"
)

// nlog is used by NeutrinoClient. Disabled until logNeutrino runs.
var nlog btclog.Logger

func init() {
	nlog = btclog.Disabled
}

func buildFilterBlocksWatchList(req *chain.FilterBlocksRequest) ([][]byte, error) {
	watchList := make([][]byte, 0, len(req.Addresses)+len(req.WatchedOutPoints))
	for _, addr := range req.Addresses {
		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}
		watchList = append(watchList, script)
	}
	for _, addr := range req.WatchedOutPoints {
		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}
		watchList = append(watchList, script)
	}
	return watchList, nil
}

// NeutrinoClient implements chain.Interface over a Neutrino ChainService.
type NeutrinoClient struct {
	CS          *neutrino.ChainService
	chainParams *chaincfg.Params

	rescan *neutrino.Rescan

	enqueueNotification chan interface{}
	dequeueNotification chan interface{}
	startTime           time.Time
	lastProgressSent    bool
	lastFilteredHeader  *wire.BlockHeader
	currentBlock        chan *waddrmgr.BlockStamp

	quit       chan struct{}
	rescanQuit chan struct{}
	rescanErr  <-chan error
	wg         sync.WaitGroup
	started    bool
	scanning   bool
	finished   bool
	isRescan   bool

	clientMtx sync.Mutex
}

var _ chain.Interface = (*NeutrinoClient)(nil)

// NewNeutrinoClient wraps an already-constructed ChainService.
func NewNeutrinoClient(chainParams *chaincfg.Params,
	chainService *neutrino.ChainService) *NeutrinoClient {
	return &NeutrinoClient{
		CS:          chainService,
		chainParams: chainParams,
	}
}

// BackEnd returns the name of the driver.
func (s *NeutrinoClient) BackEnd() string { return "neutrino" }

// Start starts the chain service and notification pump.
func (s *NeutrinoClient) Start() error {
	if err := s.CS.Start(); err != nil {
		return fmt.Errorf("error starting chain service: %v", err)
	}

	s.clientMtx.Lock()
	defer s.clientMtx.Unlock()
	if !s.started {
		s.enqueueNotification = make(chan interface{})
		s.dequeueNotification = make(chan interface{})
		s.currentBlock = make(chan *waddrmgr.BlockStamp)
		s.quit = make(chan struct{})
		s.started = true
		s.wg.Add(1)
		go func() {
			select {
			case s.enqueueNotification <- chain.ClientConnected{}:
			case <-s.quit:
			}
		}()
		go s.notificationHandler()
	}
	return nil
}

// Stop shuts down the client. The underlying ChainService is left running
// so the wallet can reconnect the client after a rescan restart.
func (s *NeutrinoClient) Stop() {
	s.clientMtx.Lock()
	defer s.clientMtx.Unlock()
	if !s.started {
		return
	}
	close(s.quit)
	s.started = false
}

// WaitForShutdown waits for the notification handler to exit.
func (s *NeutrinoClient) WaitForShutdown() { s.wg.Wait() }

// GetBlock returns the full block for hash, fetching it over P2P if needed.
func (s *NeutrinoClient) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error) {
	block, err := s.CS.GetBlock(*hash)
	if err != nil {
		return nil, err
	}
	return block.MsgBlock(), nil
}

// GetBestBlock returns the header-and-filter tip.
func (s *NeutrinoClient) GetBestBlock() (*chainhash.Hash, int32, error) {
	tip, err := s.CS.BestBlock()
	if err != nil {
		return nil, 0, err
	}
	return &tip.Hash, tip.Height, nil
}

// BlockStamp returns the latest block observed by the notification handler.
func (s *NeutrinoClient) BlockStamp() (*waddrmgr.BlockStamp, error) {
	select {
	case bs := <-s.currentBlock:
		return bs, nil
	case <-s.quit:
		return nil, errors.New("disconnected")
	}
}

// GetBlockHash returns the hash at height.
func (s *NeutrinoClient) GetBlockHash(height int64) (*chainhash.Hash, error) {
	return s.CS.GetBlockHash(height)
}

// GetBlockHeader returns the header for hash.
func (s *NeutrinoClient) GetBlockHeader(blockHash *chainhash.Hash) (*wire.BlockHeader, error) {
	return s.CS.GetBlockHeader(blockHash)
}

// IsCurrent reports whether Neutrino considers itself caught up.
func (s *NeutrinoClient) IsCurrent() bool { return s.CS.IsCurrent() }

// SendRawTransaction broadcasts a transaction to P2P peers.
func (s *NeutrinoClient) SendRawTransaction(tx *wire.MsgTx, _ bool) (*chainhash.Hash, error) {
	if err := s.CS.SendTransaction(tx); err != nil {
		return nil, err
	}
	hash := tx.TxHash()
	return &hash, nil
}

// FilterBlocks matches compact filters then scans matching full blocks.
func (s *NeutrinoClient) FilterBlocks(req *chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {
	blockFilterer := chain.NewBlockFilterer(s.chainParams, req)
	watchList, err := buildFilterBlocksWatchList(req)
	if err != nil {
		return nil, err
	}

	for i, blk := range req.Blocks {
		filter, err := s.pollCFilter(&blk.Hash)
		if err != nil {
			return nil, err
		}
		if filter == nil || filter.N() == 0 {
			continue
		}
		key := builder.DeriveKey(&blk.Hash)
		matched, err := filter.MatchAny(key, watchList)
		if err != nil {
			return nil, err
		} else if !matched {
			continue
		}

		nlog.Infof("Fetching block height=%d hash=%v", blk.Height, blk.Hash)
		rawBlock, err := s.GetBlock(&blk.Hash)
		if err != nil {
			return nil, err
		}
		if !blockFilterer.FilterBlock(rawBlock) {
			continue
		}
		return &chain.FilterBlocksResponse{
			BatchIndex:     uint32(i),
			BlockMeta:      blk,
			FoundAddresses: blockFilterer.FoundAddresses,
			FoundOutPoints: blockFilterer.FoundOutPoints,
			RelevantTxns:   blockFilterer.RelevantTxns,
		}, nil
	}
	return nil, nil
}

func (s *NeutrinoClient) pollCFilter(hash *chainhash.Hash) (*gcs.Filter, error) {
	var (
		filter *gcs.Filter
		err    error
	)
	const maxFilterRetries = 50
	for count := 0; count < maxFilterRetries; count++ {
		if count > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		filter, err = s.CS.GetCFilter(*hash, wire.GCSFilterRegular, neutrino.OptimisticBatch())
		if err != nil {
			continue
		}
		return filter, nil
	}
	return nil, err
}

// Rescan starts a wallet birthday rescan from startHash.
func (s *NeutrinoClient) Rescan(startHash *chainhash.Hash, addrs []btcutil.Address,
	outPoints map[wire.OutPoint]btcutil.Address) error {

	s.clientMtx.Lock()
	if !s.started {
		s.clientMtx.Unlock()
		return fmt.Errorf("can't do a rescan when the chain client is not started")
	}
	if s.scanning {
		close(s.rescanQuit)
		rescan := s.rescan
		s.clientMtx.Unlock()
		rescan.WaitForShutdown()
		s.clientMtx.Lock()
		s.rescan = nil
		s.rescanErr = nil
	}
	s.rescanQuit = make(chan struct{})
	s.scanning = true
	s.finished = false
	s.lastProgressSent = false
	s.lastFilteredHeader = nil
	s.isRescan = true
	s.clientMtx.Unlock()

	bestBlock, err := s.CS.BestBlock()
	if err != nil {
		return fmt.Errorf("can't get chain service's best block: %s", err)
	}
	header, err := s.CS.GetBlockHeader(&bestBlock.Hash)
	if err != nil {
		return fmt.Errorf("can't get block header for hash %v: %s", bestBlock.Hash, err)
	}

	if header.BlockHash() == *startHash {
		s.clientMtx.Lock()
		s.finished = true
		rescanQuit := s.rescanQuit
		s.clientMtx.Unlock()
		select {
		case s.enqueueNotification <- &chain.RescanFinished{
			Hash: startHash, Height: bestBlock.Height, Time: header.Timestamp,
		}:
		case <-s.quit:
			return nil
		case <-rescanQuit:
			return nil
		}
	}

	var inputsToWatch []neutrino.InputWithScript
	for op, addr := range outPoints {
		addrScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return err
		}
		inputsToWatch = append(inputsToWatch, neutrino.InputWithScript{
			OutPoint: op, PkScript: addrScript,
		})
	}

	s.clientMtx.Lock()
	newRescan := neutrino.NewRescan(
		&neutrino.RescanChainSource{ChainService: s.CS},
		neutrino.NotificationHandlers(rpcclient.NotificationHandlers{
			OnBlockConnected:         s.onBlockConnected,
			OnFilteredBlockConnected: s.onFilteredBlockConnected,
			OnBlockDisconnected:      s.onBlockDisconnected,
		}),
		neutrino.StartBlock(&headerfs.BlockStamp{Hash: *startHash}),
		neutrino.StartTime(s.startTime),
		neutrino.QuitChan(s.rescanQuit),
		neutrino.WatchAddrs(addrs...),
		neutrino.WatchInputs(inputsToWatch...),
	)
	s.rescan = newRescan
	s.rescanErr = s.rescan.Start()
	s.clientMtx.Unlock()
	return nil
}

// NotifyBlocks starts a rescan that watches blocks without extra addresses.
func (s *NeutrinoClient) NotifyBlocks() error {
	s.clientMtx.Lock()
	if !s.scanning {
		s.clientMtx.Unlock()
		return s.NotifyReceived([]btcutil.Address{})
	}
	s.clientMtx.Unlock()
	return nil
}

// NotifyReceived adds addresses to the watch list, starting a rescan if needed.
func (s *NeutrinoClient) NotifyReceived(addrs []btcutil.Address) error {
	s.clientMtx.Lock()
	if s.scanning {
		s.clientMtx.Unlock()
		return s.rescan.Update(neutrino.AddAddrs(addrs...))
	}
	s.rescanQuit = make(chan struct{})
	s.scanning = true
	s.finished = true
	s.lastProgressSent = true
	s.lastFilteredHeader = nil
	newRescan := neutrino.NewRescan(
		&neutrino.RescanChainSource{ChainService: s.CS},
		neutrino.NotificationHandlers(rpcclient.NotificationHandlers{
			OnBlockConnected:         s.onBlockConnected,
			OnFilteredBlockConnected: s.onFilteredBlockConnected,
			OnBlockDisconnected:      s.onBlockDisconnected,
		}),
		neutrino.StartTime(s.startTime),
		neutrino.QuitChan(s.rescanQuit),
		neutrino.WatchAddrs(addrs...),
	)
	s.rescan = newRescan
	s.rescanErr = s.rescan.Start()
	s.clientMtx.Unlock()
	return nil
}

// Notifications returns the notification queue.
func (s *NeutrinoClient) Notifications() <-chan interface{} {
	return s.dequeueNotification
}

// SetStartTime sets the wallet birthday used to skip old filters.
func (s *NeutrinoClient) SetStartTime(startTime time.Time) {
	s.clientMtx.Lock()
	defer s.clientMtx.Unlock()
	s.startTime = startTime
}

func (s *NeutrinoClient) onFilteredBlockConnected(height int32,
	header *wire.BlockHeader, relevantTxs []*btcutil.Tx) {
	ntfn := chain.FilteredBlockConnected{
		Block: &wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: header.BlockHash(), Height: height},
			Time:  header.Timestamp,
		},
	}
	for _, tx := range relevantTxs {
		rec, err := wtxmgr.NewTxRecordFromMsgTx(tx.MsgTx(), header.Timestamp)
		if err != nil {
			nlog.Errorf("Cannot create transaction record for relevant tx: %s", err)
			continue
		}
		ntfn.RelevantTxs = append(ntfn.RelevantTxs, rec)
	}
	select {
	case s.enqueueNotification <- ntfn:
	case <-s.quit:
		return
	case <-s.rescanQuit:
		return
	}
	s.clientMtx.Lock()
	s.lastFilteredHeader = header
	s.clientMtx.Unlock()
	s.dispatchRescanFinished()
}

func (s *NeutrinoClient) onBlockDisconnected(hash *chainhash.Hash, height int32, t time.Time) {
	select {
	case s.enqueueNotification <- chain.BlockDisconnected(wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Hash: *hash, Height: height},
		Time:  t,
	}):
	case <-s.quit:
	case <-s.rescanQuit:
	}
}

func (s *NeutrinoClient) onBlockConnected(hash *chainhash.Hash, height int32, ts time.Time) {
	sendRescanProgress := func() {
		select {
		case s.enqueueNotification <- &chain.RescanProgress{Hash: hash, Height: height, Time: ts}:
		case <-s.quit:
		case <-s.rescanQuit:
		}
	}
	if ts.Before(s.startTime) {
		if height%10000 == 0 {
			s.clientMtx.Lock()
			shouldSend := s.isRescan && !s.finished
			s.clientMtx.Unlock()
			if shouldSend {
				sendRescanProgress()
			}
		}
	} else {
		s.clientMtx.Lock()
		if !s.lastProgressSent {
			shouldSend := s.isRescan && !s.finished
			if shouldSend {
				s.clientMtx.Unlock()
				sendRescanProgress()
				s.clientMtx.Lock()
				s.lastProgressSent = true
			}
		}
		s.clientMtx.Unlock()
		select {
		case s.enqueueNotification <- chain.BlockConnected(wtxmgr.BlockMeta{
			Block: wtxmgr.Block{Hash: *hash, Height: height},
			Time:  ts,
		}):
		case <-s.quit:
		case <-s.rescanQuit:
		}
	}
	s.dispatchRescanFinished()
}

func (s *NeutrinoClient) dispatchRescanFinished() {
	bs, err := s.CS.BestBlock()
	if err != nil {
		nlog.Errorf("Can't get chain service's best block: %s", err)
		return
	}
	s.clientMtx.Lock()
	if s.lastFilteredHeader == nil || s.finished {
		s.clientMtx.Unlock()
		return
	}
	if bs.Hash != s.lastFilteredHeader.BlockHash() {
		s.clientMtx.Unlock()
		return
	}
	s.finished = s.CS.IsCurrent() && s.lastProgressSent
	if !s.finished {
		s.clientMtx.Unlock()
		return
	}
	header := s.lastFilteredHeader
	s.clientMtx.Unlock()
	select {
	case s.enqueueNotification <- &chain.RescanFinished{
		Hash: &bs.Hash, Height: bs.Height, Time: header.Timestamp,
	}:
	case <-s.quit:
	case <-s.rescanQuit:
	}
}

func (s *NeutrinoClient) notificationHandler() {
	hash, height, err := s.GetBestBlock()
	if err != nil {
		nlog.Errorf("Failed to get best block from chain service: %s", err)
		s.Stop()
		s.wg.Done()
		return
	}
	bs := &waddrmgr.BlockStamp{Hash: *hash, Height: height}

	var notifications []interface{}
	enqueue := s.enqueueNotification
	var dequeue chan interface{}
	var next interface{}
out:
	for {
		s.clientMtx.Lock()
		rescanErr := s.rescanErr
		s.clientMtx.Unlock()
		select {
		case n, ok := <-enqueue:
			if !ok {
				if len(notifications) == 0 {
					break out
				}
				enqueue = nil
				continue
			}
			if len(notifications) == 0 {
				next = n
				dequeue = s.dequeueNotification
			}
			notifications = append(notifications, n)

		case dequeue <- next:
			if n, ok := next.(chain.BlockConnected); ok {
				bs = &waddrmgr.BlockStamp{Height: n.Height, Hash: n.Hash}
			}
			notifications[0] = nil
			notifications = notifications[1:]
			if len(notifications) != 0 {
				next = notifications[0]
			} else {
				if enqueue == nil {
					break out
				}
				dequeue = nil
			}

		case err := <-rescanErr:
			if err != nil {
				nlog.Errorf("Neutrino rescan ended with error: %s", err)
			}

		case s.currentBlock <- bs:

		case <-s.quit:
			break out
		}
	}
	s.Stop()
	close(s.dequeueNotification)
	s.wg.Done()
}
