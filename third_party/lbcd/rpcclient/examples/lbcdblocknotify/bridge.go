package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type bridge struct {
	ctx context.Context

	prevJobContext context.Context
	prevJobCancel  context.CancelFunc

	eventCh chan interface{}
	errorc  chan error
	wg      sync.WaitGroup

	stratum *stratumClient

	customCmd string
}

func newBridge(stratumServer, stratumPass, coinid string) *bridge {

	s := &bridge{
		ctx:     context.Background(),
		eventCh: make(chan interface{}),
		errorc:  make(chan error),
	}

	if len(stratumServer) > 0 {
		s.stratum = newStratumClient(stratumServer, stratumPass, coinid)
	}

	return s
}

func (b *bridge) start() {

	if b.stratum != nil {
		backoff := time.Second
		for {
			err := b.stratum.dial()
			if err == nil {
				break
			}
			log.Printf("WARN: stratum.dial() error: %s, retry in %s", err, backoff)
			time.Sleep(backoff)
			if backoff < 60*time.Second {
				backoff += time.Second
			}
		}
	}

	for e := range b.eventCh {
		switch e := e.(type) {
		case *eventBlockConected:
			b.handleFilteredBlockConnected(e)
		default:
			b.errorc <- fmt.Errorf("unknown event type: %T", e)
			return
		}
	}
}

func (b *bridge) handleFilteredBlockConnected(e *eventBlockConected) {

	if !*quiet {
		log.Printf("Block connected: %s (%d) %v", e.header.BlockHash(), e.height, e.header.Timestamp)
	}

	hash := e.header.BlockHash().String()
	height := e.height

	// Cancel jobs on previous block. It's safe if they are already done.
	if b.prevJobContext != nil {
		select {
		case <-b.prevJobContext.Done():
			log.Printf("prev one canceled")
		default:
			b.prevJobCancel()
		}
	}

	// Wait until all previous jobs are done or canceled.
	b.wg.Wait()

	// Create and save cancelable subcontext for new jobs.
	ctx, cancel := context.WithCancel(b.ctx)
	b.prevJobContext, b.prevJobCancel = ctx, cancel

	if len(b.customCmd) > 0 {
		go b.execCustomCommand(ctx, hash, height)
	}

	// Send stratum update block message
	if b.stratum != nil {
		go b.stratumUpdateBlock(ctx, hash, height)
	}
}

func (s *bridge) stratumUpdateBlock(ctx context.Context, hash string, height int32) {
	s.wg.Add(1)
	defer s.wg.Done()

	backoff := time.Second
	retry := func(err error) {
		if backoff < 60*time.Second {
			backoff += time.Second
		}
		log.Printf("WARN: stratum.send() on block %d error: %s", height, err)
		time.Sleep(backoff)
		s.stratum.dial()
	}

	msg := stratumUpdateBlockMsg(*stratumPass, *coinid, hash)

	for {
		switch err := s.stratum.send(ctx, msg); {
		case err == nil:
			return
		case errors.Is(err, context.Canceled):
			log.Printf("INFO: stratum.send() on block %d: %s.", height, err)
			return
		case errors.Is(err, syscall.EPIPE):
			errClose := s.stratum.conn.Close()
			if errClose != nil {
				log.Printf("WARN: stratum.conn.Close() on block %d: %s.", height, errClose)
			}
			retry(err)
		case errors.Is(err, net.ErrClosed):
			retry(err)
		default:
			retry(err)
		}
	}

}

func (s *bridge) execCustomCommand(ctx context.Context, hash string, height int32) {
	s.wg.Add(1)
	defer s.wg.Done()

	cmd := strings.ReplaceAll(s.customCmd, "%s", hash)
	err := doExecCustomCommand(ctx, cmd)
	if err != nil {
		log.Printf("ERROR: execCustomCommand on block %s(%d): %s", hash, height, err)
	}
}

func doExecCustomCommand(ctx context.Context, cmd string) error {
	strs := strings.Split(cmd, " ")
	path, err := exec.LookPath(strs[0])
	if errors.Is(err, exec.ErrDot) {
		err = nil
	}
	if err != nil {
		return err
	}
	c := exec.CommandContext(ctx, path, strs[1:]...)
	c.Stdout = os.Stdout
	return c.Run()
}
