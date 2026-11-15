package discover

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestUnauthenticatedSocketsCountTowardIPLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{Listen: "127.0.0.1:0", Stun: "127.0.0.1:0"}
	go s.Run(ctx)
	if err := s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	var sockets []*websocket.Conn
	defer func() {
		for _, c := range sockets {
			c.CloseNow()
		}
	}()
	dial := func() (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, "wss://"+s.HTTPAddr()+"/ws", &websocket.DialOptions{HTTPClient: dialClient(true)})
	}
	for i := 0; i < maxConnsPerIP; i++ {
		c, _, err := dial()
		if err != nil {
			t.Fatal(err)
		}
		sockets = append(sockets, c)
		rctx, stop := context.WithTimeout(ctx, time.Second)
		_, _, err = c.Read(rctx)
		stop()
		if err != nil {
			t.Fatal(err)
		}
	}
	c, res, err := dial()
	if err == nil {
		c.CloseNow()
		t.Fatal("unauthenticated sockets bypassed the per-IP limit")
	}
	if res == nil || res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %v: %v", res, err)
	}
	sockets[0].CloseNow()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		count := s.connections["127.0.0.1"]
		s.mu.Unlock()
		if count < maxConnsPerIP {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed socket did not release its slot")
		}
		time.Sleep(time.Millisecond)
	}
	c, _, err = dial()
	if err != nil {
		t.Fatal(err)
	}
	c.CloseNow()
}

func TestDiscoveryHelloHasDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 13*time.Second)
	defer cancel()
	s := &Server{Listen: "127.0.0.1:0", Stun: "127.0.0.1:0"}
	go s.Run(ctx)
	if err := s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	c, _, err := websocket.Dial(ctx, "wss://"+s.HTTPAddr()+"/ws", &websocket.DialOptions{HTTPClient: dialClient(true)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("server did not close the socket before the client deadline: %v", err)
	}
}
