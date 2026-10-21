package main

import (
	"github.com/lbryio/lbcd/wire"
	"github.com/lbryio/lbcutil"
)

type eventBlockConected struct {
	height int32
	header *wire.BlockHeader
	txns   []*lbcutil.Tx
}

type adapter struct {
	*bridge
}

func (a *adapter) onFilteredBlockConnected(height int32, header *wire.BlockHeader, txns []*lbcutil.Tx) {
	a.eventCh <- &eventBlockConected{height, header, txns}
}
