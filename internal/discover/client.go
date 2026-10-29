package discover

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Peer is another device in the same folder. Addresses are not included;
// they arrive later inside a sealed signal.
type Peer struct {
	ID   string
	Name string
}

// Handler receives discovery events. Callbacks run on the connection goroutine
// and must not block for long.
type Handler struct {
	Welcome func(publicIP string)
	Peers   func([]Peer)
	Joined  func(Peer)
	Left    func(id string)
	Signal  func(from string, sealed []byte)
}

// Client maintains one websocket to the discovery server for one folder.
type Client struct {
	// URL is the server base, such as ws://127.0.0.1:8080 or http://host:8080.
	URL      string
	DeviceID string
	Name     string
	ShareID  string
	Token    string
	Log      *slog.Logger
	Handler  Handler

	mu   sync.Mutex
	send chan []byte
}

// Run reconnects until ctx is cancelled.
func (c *Client) Run(ctx context.Context) {
	if c.Log == nil {
		c.Log = slog.Default()
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		established, err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.Log.Info("discovery disconnected", "err", err, "server", c.URL)
		}
		if established {
			backoff = time.Second
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if !established && backoff < 20*time.Second {
			backoff *= 2
		}
	}
}

// Connected reports whether the discovery websocket is up.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.send != nil
}

// Signal forwards an already sealed payload to another device in the folder.
// It fails when the websocket is down. The caller retries on its own timer.
func (c *Client) Signal(to string, sealed []byte) error {
	if len(sealed) > 12<<10 {
		return errors.New("signal too large")
	}
	msg, err := marshal(signalMsg{
		Type: "signal",
		To:   to,
		Data: base64.StdEncoding.EncodeToString(sealed),
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	ch := c.send
	c.mu.Unlock()
	if ch == nil {
		return errors.New("discovery is not connected")
	}
	select {
	case ch <- msg:
		return nil
	default:
		return errors.New("discovery send queue is full")
	}
}

func (c *Client) session(ctx context.Context) (established bool, err error) {
	wsURL, err := websocketURL(c.URL)
	if err != nil {
		return false, err
	}
	// A fresh transport avoids reusing a connection that died with the server.
	// The handshake itself has a deadline, so a restart that accepts TCP and
	// then never answers cannot pin this loop.
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: dialClient()})
	if err != nil {
		return false, err
	}
	conn.SetReadLimit(32 << 10)
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	defer conn.CloseNow()

	hello, err := marshal(helloMsg{
		Type:     "hello",
		DeviceID: c.DeviceID,
		Name:     c.Name,
		ShareID:  c.ShareID,
		Token:    c.Token,
	})
	if err != nil {
		return false, err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = conn.Write(wctx, websocket.MessageText, hello)
	cancel()
	if err != nil {
		return false, err
	}
	established = true

	send := make(chan []byte, 32)
	c.mu.Lock()
	c.send = send
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.send == send {
			c.send = nil
		}
		c.mu.Unlock()
	}()

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		defer scancel()
		ping := time.NewTicker(10 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-ping.C:
				pctx, pcancel := context.WithTimeout(sctx, 8*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					return
				}
			case msg, ok := <-send:
				if !ok {
					return
				}
				wctx, wcancel := context.WithTimeout(sctx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageText, msg)
				wcancel()
				if err != nil {
					return
				}
			}
		}
	}()
	defer func() {
		scancel()
		<-writeDone
	}()

	for {
		_, data, err := conn.Read(sctx)
		if err != nil {
			return established, err
		}
		var msg serverMsg
		if err := unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "welcome":
			if c.Handler.Welcome != nil {
				c.Handler.Welcome(msg.PublicIP)
			}
		case "peers":
			if c.Handler.Peers != nil {
				c.Handler.Peers(toPeers(msg.Peers))
			}
		case "peer_joined":
			if c.Handler.Joined != nil && msg.ID != "" && msg.ID != c.DeviceID {
				c.Handler.Joined(Peer{ID: msg.ID, Name: msg.Name})
			}
		case "peer_left":
			if c.Handler.Left != nil && msg.ID != "" {
				c.Handler.Left(msg.ID)
			}
		case "signal":
			raw, err := base64.StdEncoding.DecodeString(msg.Data)
			if err != nil || msg.From == "" || msg.From == c.DeviceID {
				continue
			}
			if c.Handler.Signal != nil {
				c.Handler.Signal(msg.From, raw)
			}
		case "error":
			return established, errors.New(msg.Error)
		}
	}
}

func dialClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

func toPeers(in []peerInfo) []Peer {
	out := make([]Peer, 0, len(in))
	for _, p := range in {
		out = append(out, Peer{ID: p.ID, Name: p.Name})
	}
	return out
}

func websocketURL(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("server url scheme must be ws, wss, http, or https")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/ws"
	}
	return u.String(), nil
}
