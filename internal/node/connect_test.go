package node

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/identity"
)

func TestCancelledUnselectedDialClosesConnection(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := id.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan *tls.Conn, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		c := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
		if err := c.Handshake(); err != nil {
			c.Close()
			return
		}
		accepted <- c
	}()
	n := &Node{clientTLS: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan secured)
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.dialTCP(ctx, ln.Addr().String(), ch)
	}()
	var peer *tls.Conn
	select {
	case peer = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("TLS dial did not finish")
	}
	defer peer.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unselected dial did not exit after cancellation")
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("unexpected data on unselected connection")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("unselected connection was not closed")
	}
}
