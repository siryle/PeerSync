package discover

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/identity"
	"github.com/netcraft/peersync/internal/invite"
)

func TestJoinCapsConnectionsPerIP(t *testing.T) {
	s := &Server{}
	s.init()
	token := "aa"
	var first string
	for i := 0; i < maxConnsPerIP; i++ {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = id.DeviceID()
		}
		p := &peer{id: id.DeviceID(), ip: "203.0.113.9", send: make(chan []byte, 1), cancel: func() {}}
		if err := s.join(fmt.Sprintf("%032x", i), token, p); err != nil {
			t.Fatal(err)
		}
	}
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.join(fmt.Sprintf("%032x", maxConnsPerIP), token, &peer{id: id.DeviceID(), ip: "203.0.113.9", send: make(chan []byte, 1), cancel: func() {}}); err == nil {
		t.Fatal("expected the per-address cap")
	}
	if err := s.join(fmt.Sprintf("%032x", maxConnsPerIP+1), token, &peer{id: id.DeviceID(), ip: "203.0.113.10", send: make(chan []byte, 1), cancel: func() {}}); err != nil {
		t.Fatal(err)
	}
	if err := s.join(fmt.Sprintf("%032x", 0), token, &peer{id: first, ip: "203.0.113.9", send: make(chan []byte, 1), cancel: func() {}}); err != nil {
		t.Fatalf("replacing an existing device should stay under the cap: %v", err)
	}
}

func TestHelloRejectsForeignKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &Server{Listen: "127.0.0.1:0", Stun: "127.0.0.1:0"}
	go srv.Run(ctx)
	if err := srv.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	material, err := invite.NewShare()
	if err != nil {
		t.Fatal(err)
	}
	shareID := invite.IDHex(material.ShareID)
	welcomes := make(chan struct{}, 1)
	go (&Client{
		URL: "ws://" + srv.HTTPAddr(), DeviceID: dev.DeviceID(), Name: "x",
		ShareID: shareID, Token: invite.RoomToken(material.ShareKey[:], shareID),
		Private: other.Private,
		Handler: Handler{Welcome: func(string) { welcomes <- struct{}{} }},
	}).Run(ctx)
	select {
	case <-welcomes:
		t.Fatal("foreign key was accepted")
	case <-time.After(700 * time.Millisecond):
	}
}

func TestClientHostIgnoresLoopback(t *testing.T) {
	if got := clientHost("127.0.0.1:42420"); got != "" {
		t.Fatalf("loopback %q", got)
	}
	if got := clientHost("[::1]:42420"); got != "" {
		t.Fatalf("ipv6 loopback %q", got)
	}
	if got := clientHost("0.0.0.0:8080"); got != "" {
		t.Fatalf("unspecified %q", got)
	}
	if got := clientHost("203.0.113.8:42420"); got != "203.0.113.8" {
		t.Fatalf("got %q", got)
	}
}

func TestClientReconnectsAfterRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	start := func() func() {
		sctx, stop := context.WithCancel(ctx)
		srv := &Server{Listen: addr, Stun: "127.0.0.1:0"}
		errc := make(chan error, 1)
		go func() { errc <- srv.Run(sctx) }()
		if err := srv.Ready(sctx); err != nil {
			t.Fatal(err)
		}
		return func() {
			stop()
			select {
			case <-errc:
			case <-time.After(3 * time.Second):
				t.Fatal("server did not stop")
			}
		}
	}
	stop := start()

	dev, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	material, err := invite.NewShare()
	if err != nil {
		t.Fatal(err)
	}
	shareID := invite.IDHex(material.ShareID)
	welcomes := make(chan struct{}, 4)
	client := &Client{
		URL:      "ws://" + addr,
		DeviceID: dev.DeviceID(),
		Name:     "rejoin",
		ShareID:  shareID,
		Token:    invite.RoomToken(material.ShareKey[:], shareID),
		Private:  dev.Private,
		Handler:  Handler{Welcome: func(string) { welcomes <- struct{}{} }},
	}
	go client.Run(ctx)

	waitWelcome := func() {
		t.Helper()
		select {
		case <-welcomes:
		case <-time.After(8 * time.Second):
			t.Fatal("discovery did not connect")
		}
	}
	waitWelcome()
	stop()
	stop = start()
	defer stop()
	waitWelcome()
}

func TestSignalDoesNotCarryDirectory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &Server{Listen: "127.0.0.1:0", Stun: "127.0.0.1:0"}
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(ctx) }()
	if err := srv.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	material, err := invite.NewShare()
	if err != nil {
		t.Fatal(err)
	}
	id := invite.IDHex(material.ShareID)
	token := invite.RoomToken(material.ShareKey[:], id)
	key := SignalKey(material.ShareKey[:], id)

	aID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	bID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan []byte, 1)
	ca := &Client{
		URL: "ws://" + srv.HTTPAddr(), DeviceID: aID.DeviceID(), Name: "a",
		ShareID: id, Token: token, Private: aID.Private,
	}
	cb := &Client{
		URL: "ws://" + srv.HTTPAddr(), DeviceID: bID.DeviceID(), Name: "b",
		ShareID: id, Token: token, Private: bID.Private,
		Handler: Handler{Signal: func(from string, sealed []byte) {
			if from == aID.DeviceID() {
				got <- sealed
			}
		}},
	}
	go ca.Run(ctx)
	go cb.Run(ctx)

	plain := []byte(`{"from":"a","tcp":["203.0.113.8:42420"],"note":"not-a-file"}`)
	sealed, err := Seal(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var sent bool
	for time.Now().Before(deadline) {
		if err := ca.Signal(bID.DeviceID(), sealed); err == nil {
			sent = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sent {
		t.Fatal("could not send signal")
	}
	select {
	case box := <-got:
		opened, err := Open(key, box)
		if err != nil {
			t.Fatal(err)
		}
		if string(opened) != string(plain) {
			t.Fatalf("opened %s", opened)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signal was not forwarded")
	}
	if srv.SignalBytes.Load() > int64(len(sealed)*4) {
		t.Fatalf("forwarded too much: %d", srv.SignalBytes.Load())
	}
	cancel()
	select {
	case <-errc:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
}
