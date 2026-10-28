package main

import (
	"io/ioutil"
	"log"
	"path/filepath"

	"github.com/lbryio/lbcd/rpcclient"
)

func newLbcdClient(server, user, pass string, notls bool, adpt adapter) *rpcclient.Client {

	ntfnHandlers := rpcclient.NotificationHandlers{
		OnFilteredBlockConnected: adpt.onFilteredBlockConnected,
	}

	// Config lbcd RPC client with websockets.
	connCfg := &rpcclient.ConnConfig{
		Host:       server,
		Endpoint:   "ws",
		User:       user,
		Pass:       pass,
		DisableTLS: true,
	}

	if !notls {
		cert, err := ioutil.ReadFile(filepath.Join(lbcdHomeDir, "rpc.cert"))
		if err != nil {
			log.Fatalf("can't read lbcd certificate: %s", err)
		}
		connCfg.Certificates = cert
		connCfg.DisableTLS = false
	}

	client, err := rpcclient.New(connCfg, &ntfnHandlers)
	if err != nil {
		log.Fatalf("can't create rpc client: %s", err)
	}

	// Register for block connect and disconnect notifications.
	if err = client.NotifyBlocks(); err != nil {
		log.Fatalf("can't register block notification: %s", err)
	}

	// Get the current block count.
	blockCount, err := client.GetBlockCount()
	if err != nil {
		log.Fatalf("can't get block count: %s", err)
	}
	log.Printf("Current block count: %d", blockCount)

	return client
}
