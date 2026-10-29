// Package discover is the rendezvous service. It tells clients which peers
// are online and forwards small encrypted address messages. It does not
// receive file names or file contents, and it cannot decrypt the addresses.
package discover

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

var (
	deviceIDRe = regexp.MustCompile(`^PS[A-Z2-7]{52}$`)
	shareIDRe  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	tokenRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Server is the discovery process.
type Server struct {
	// Listen is the HTTP address, for example ":8080".
	Listen string
	// Stun is the UDP address clients use to learn their public UDP port.
	// One request packet is answered with one response packet. Payloads that
	// arrive here are not forwarded to anybody else.
	Stun string
	Log  *slog.Logger

	// SignalBytes counts opaque signal payload bytes forwarded between peers.
	// Directory contents never pass through the server.
	SignalBytes atomic.Int64

	initOnce sync.Once
	ready    chan struct{}

	mu    sync.Mutex
	rooms map[string]*room

	httpAddr string
	stunAddr string
}

type room struct {
	token [32]byte
	peers map[string]*peer
}

type peer struct {
	id     string
	name   string
	send   chan []byte
	cancel context.CancelFunc
}

func (s *Server) init() {
	s.initOnce.Do(func() {
		s.ready = make(chan struct{})
		if s.Log == nil {
			s.Log = slog.Default()
		}
		if s.Listen == "" {
			s.Listen = ":8080"
		}
		if s.Stun == "" {
			s.Stun = ":3478"
		}
		if s.rooms == nil {
			s.rooms = map[string]*room{}
		}
	})
}

// HTTPAddr is the bound HTTP address. Valid after Ready returns.
func (s *Server) HTTPAddr() string { return s.httpAddr }

// STUNAddr is the bound UDP reflector address.
func (s *Server) STUNAddr() string { return s.stunAddr }

// Ready waits until both listeners are bound.
func (s *Server) Ready(ctx context.Context) error {
	s.init()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ready:
		return nil
	}
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.init()
	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return err
	}
	udpAddr, err := net.ResolveUDPAddr("udp", s.Stun)
	if err != nil {
		_ = ln.Close()
		return err
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = ln.Close()
		return err
	}
	s.httpAddr = ln.Addr().String()
	s.stunAddr = udp.LocalAddr().String()
	close(s.ready)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /ws", s.handleWS)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		s.dropAll()
		shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
		_ = udp.Close()
	}()
	go s.reflect(ctx, udp)

	s.Log.Info("discovery listening", "http", s.httpAddr, "stun", s.stunAddr)
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) reflect(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if n < 11 || buf[0] != 0x11 || buf[1] != 0x01 || buf[2] != 0x01 {
			continue
		}
		resp := make([]byte, 0, 11+len(addr.String()))
		resp = append(resp, 0x11, 0x01, 0x02)
		resp = append(resp, buf[3:11]...)
		resp = append(resp, addr.String()...)
		_, _ = conn.WriteToUDP(resp, addr)
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.Log.Debug("websocket rejected", "err", err)
		return
	}
	conn.SetReadLimit(32 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer conn.CloseNow()

	host := clientHost(r.RemoteAddr)

	_, data, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var hello helloMsg
	if err := unmarshal(data, &hello); err != nil || hello.Type != "hello" {
		writeErr(ctx, conn, "expected hello")
		return
	}
	if !deviceIDRe.MatchString(hello.DeviceID) || !shareIDRe.MatchString(hello.ShareID) || !tokenRe.MatchString(hello.Token) {
		writeErr(ctx, conn, "invalid hello")
		return
	}
	if len(hello.Name) > 64 {
		hello.Name = hello.Name[:64]
	}

	send := make(chan []byte, 32)
	p := &peer{id: hello.DeviceID, name: hello.Name, send: send, cancel: cancel}
	if err := s.join(hello.ShareID, hello.Token, p); err != nil {
		writeErr(ctx, conn, err.Error())
		return
	}
	defer s.leave(hello.ShareID, p)

	welcome, _ := marshal(serverMsg{Type: "welcome", PublicIP: host})
	list, _ := marshal(serverMsg{Type: "peers", Peers: s.peerList(hello.ShareID, hello.DeviceID)})
	s.broadcast(hello.ShareID, hello.DeviceID, serverMsg{Type: "peer_joined", ID: hello.DeviceID, Name: hello.Name})

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-send:
				if !ok {
					return
				}
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageText, msg)
				wcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	enqueue(send, welcome)
	enqueue(send, list)

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg signalMsg
		if err := unmarshal(data, &msg); err != nil || msg.Type != "signal" || !deviceIDRe.MatchString(msg.To) {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(msg.Data)
		if err != nil || len(raw) == 0 || len(raw) > 12<<10 {
			continue
		}
		out, err := marshal(serverMsg{Type: "signal", From: hello.DeviceID, Data: msg.Data})
		if err != nil {
			continue
		}
		if s.forward(hello.ShareID, msg.To, out) {
			s.SignalBytes.Add(int64(len(raw)))
		}
	}
}

// clientHost is the address to report back to a client. Loopback and
// unspecified addresses are omitted: a restarted Docker proxy often presents
// the peer as 127.0.0.1, and that must not replace the address the client
// already learned.
func clientHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return ""
	}
	return ip.String()
}

func writeErr(ctx context.Context, conn *websocket.Conn, text string) {
	b, _ := marshal(serverMsg{Type: "error", Error: text})
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = conn.Write(wctx, websocket.MessageText, b)
}

func (s *Server) join(shareID, token string, p *peer) error {
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	rm := s.rooms[shareID]
	if rm == nil {
		if len(s.rooms) >= 100000 {
			return errors.New("server is full")
		}
		rm = &room{token: sum, peers: map[string]*peer{}}
		s.rooms[shareID] = rm
	} else if subtle.ConstantTimeCompare(rm.token[:], sum[:]) != 1 {
		return errors.New("folder token rejected")
	}
	if old, ok := rm.peers[p.id]; ok {
		old.cancel()
	} else if len(rm.peers) >= 64 {
		return errors.New("folder is full")
	}
	rm.peers[p.id] = p
	return nil
}

func (s *Server) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rm := range s.rooms {
		for _, p := range rm.peers {
			if p.cancel != nil {
				p.cancel()
			}
		}
	}
}

func (s *Server) leave(shareID string, p *peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rm := s.rooms[shareID]
	if rm == nil {
		return
	}
	cur, ok := rm.peers[p.id]
	if !ok || cur != p {
		return
	}
	delete(rm.peers, p.id)
	close(p.send)
	left, _ := marshal(serverMsg{Type: "peer_left", ID: p.id})
	for _, other := range rm.peers {
		enqueue(other.send, left)
	}
	if len(rm.peers) == 0 {
		delete(s.rooms, shareID)
	}
}

func (s *Server) peerList(shareID, self string) []peerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	rm := s.rooms[shareID]
	if rm == nil {
		return nil
	}
	out := make([]peerInfo, 0, len(rm.peers))
	for id, p := range rm.peers {
		if id == self {
			continue
		}
		out = append(out, peerInfo{ID: p.id, Name: p.name})
	}
	return out
}

func (s *Server) broadcast(shareID, self string, msg serverMsg) {
	b, err := marshal(msg)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rm := s.rooms[shareID]
	if rm == nil {
		return
	}
	for id, p := range rm.peers {
		if id == self {
			continue
		}
		enqueue(p.send, b)
	}
}

func (s *Server) forward(shareID, to string, msg []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rm := s.rooms[shareID]
	if rm == nil {
		return false
	}
	p := rm.peers[to]
	if p == nil {
		return false
	}
	return enqueue(p.send, msg)
}

func enqueue(ch chan []byte, msg []byte) bool {
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}
