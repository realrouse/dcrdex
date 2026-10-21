package main

import (
	"context"
	"fmt"
	"net"
)

type stratumClient struct {
	server string
	passwd string
	coinid string
	conn   *net.TCPConn
}

func newStratumClient(server, passwd, coinid string) *stratumClient {

	return &stratumClient{
		server: server,
	}
}

func (c *stratumClient) dial() error {

	addr, err := net.ResolveTCPAddr("tcp", c.server)
	if err != nil {
		return fmt.Errorf("resolve tcp addr: %w", err)
	}

	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		return fmt.Errorf("dial tcp: %w", err)
	}
	c.conn = conn

	return nil
}

func (c *stratumClient) send(ctx context.Context, msg string) error {

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	_, err := c.conn.Write([]byte(msg))

	return err
}

func stratumUpdateBlockMsg(stratumPass, coinid, blockHash string) string {

	return fmt.Sprintf(`{"id":1,"method":"mining.update_block","params":[%q,%s,%q]}`,
		stratumPass, coinid, blockHash)
}
