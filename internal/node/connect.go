package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/netcraft/peersync/internal/discover"
	"github.com/netcraft/peersync/internal/frame"
	"github.com/netcraft/peersync/internal/identity"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/syncer"
)

var errAuth = errors.New("authentication failed")

func (n *Node) acceptTCP() {
	for {
		conn, err := n.tcpLn.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			tuneTCP(conn)
			tlsConn := tls.Server(conn, n.serverTLS)
			hctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
			err := tlsConn.HandshakeContext(hctx)
			cancel()
			if err != nil {
				_ = conn.Close()
				return
			}
			_ = n.finish(n.ctx, tlsConn, tlsConn.ConnectionState(), false, "", "", conn.RemoteAddr().String(), nil)
		}(conn)
	}
}

func (n *Node) acceptQUIC() {
	for {
		conn, err := n.qln.Accept(n.ctx)
		if err != nil {
			return
		}
		go func(conn *quic.Conn) {
			defer conn.CloseWithError(0, "bye")
			stream, err := conn.AcceptStream(conn.Context())
			if err != nil {
				return
			}
			_ = n.finish(n.ctx, stream, conn.ConnectionState().TLS, false, "", "", conn.RemoteAddr().String(), nil)
		}(conn)
	}
}

func (n *Node) watch(ctx context.Context, shareID, peerID string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if n.connected(shareID, peerID) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		n.attempt(ctx, shareID, peerID)
		n.mu.Lock()
		kick := n.kick
		n.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-kick:
			backoff = time.Second
		case <-time.After(backoff):
			if backoff < 15*time.Second {
				backoff *= 2
			} else {
				backoff = 15 * time.Second
			}
		}
	}
}

func (n *Node) attempt(ctx context.Context, shareID, peerID string) {
	dialCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	tcp, udp := n.peerCandidates(shareID, peerID)
	ch := make(chan secured, 8)
	for _, addr := range tcp {
		go n.dialTCP(dialCtx, addr, ch)
	}
	go n.punch(dialCtx, peerID, udp)
	higher := n.id.DeviceID() > peerID
	if higher {
		go n.quicDial(dialCtx, udp, peerID, ch)
	} else {
		go func() {
			timer := time.NewTimer(4 * time.Second)
			defer timer.Stop()
			select {
			case <-dialCtx.Done():
			case <-timer.C:
				if !n.connected(shareID, peerID) {
					n.quicDial(dialCtx, udp, peerID, ch)
				}
			}
		}()
	}
	for {
		select {
		case sec := <-ch:
			err := n.finish(n.ctx, sec.stream, sec.cs, true, shareID, peerID, sec.addr, cancel)
			if err == nil || errors.Is(err, context.Canceled) || !errors.Is(err, errAuth) {
				return
			}
		case <-dialCtx.Done():
			return
		}
	}
}

type secured struct {
	stream frame.Stream
	cs     tls.ConnectionState
	addr   string
}

func (n *Node) dialTCP(ctx context.Context, addr string, ch chan<- secured) {
	d := net.Dialer{Timeout: 4 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return
	}
	tuneTCP(conn)
	tlsConn := tls.Client(conn, n.clientTLS.Clone())
	hctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	err = tlsConn.HandshakeContext(hctx)
	cancel()
	if err != nil {
		_ = conn.Close()
		return
	}
	sec := secured{stream: tlsConn, cs: tlsConn.ConnectionState(), addr: addr}
	select {
	case ch <- sec:
	case <-ctx.Done():
		_ = tlsConn.Close()
	}
}

func (n *Node) punch(ctx context.Context, peerID string, addrs []string) {
	pub, err := identity.ParseDeviceID(peerID)
	if err != nil {
		return
	}
	pkt := punchPacket(n.id.Public, pub)
	send := func() {
		targets := n.punchTargets(peerID, addrs)
		for _, addr := range targets {
			ua, err := udpAddr(addr)
			if err != nil {
				continue
			}
			_, _ = n.tr.WriteTo(pkt, ua)
		}
	}
	send()
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		}
	}
}

func (n *Node) punchTargets(peerID string, announced []string) []string {
	_, udp := n.peerCandidates(shareOf(n, peerID), peerID)
	if len(udp) == 0 {
		return announced
	}
	return udp
}

func (n *Node) quicDial(ctx context.Context, announced []string, peerID string, ch chan<- secured) {
	tried := map[string]struct{}{}
	try := func() {
		for _, addr := range n.punchTargets(peerID, announced) {
			if _, ok := tried[addr]; ok {
				continue
			}
			tried[addr] = struct{}{}
			go n.dialQUIC(ctx, addr, ch)
		}
	}
	try()
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			try()
		}
	}
}

func (n *Node) dialQUIC(ctx context.Context, addr string, ch chan<- secured) {
	ua, err := udpAddr(addr)
	if err != nil {
		return
	}
	conn, err := n.tr.Dial(ctx, ua, n.clientTLS.Clone(), n.qconf)
	if err != nil {
		return
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream")
		return
	}
	sec := secured{stream: &quicStream{Stream: stream, conn: conn}, cs: conn.ConnectionState().TLS, addr: addr}
	select {
	case ch <- sec:
	case <-ctx.Done():
		_ = conn.CloseWithError(0, "cancel")
	}
}

type quicStream struct {
	*quic.Stream
	conn *quic.Conn
}

func (s *quicStream) Close() error {
	_ = s.Stream.Close()
	return s.conn.CloseWithError(0, "bye")
}

func (n *Node) finish(ctx context.Context, stream frame.Stream, cs tls.ConnectionState, isClient bool, shareID, peerID, addr string, onReady func()) error {
	fc := frame.New(stream)
	defer fc.Close()
	var remote proto.Auth
	var sh *liveShare
	if isClient {
		sh = n.shares[shareID]
		if sh == nil {
			return errAuth
		}
		if err := n.writeAuth(fc, cs, sh); err != nil {
			n.log.Debug("auth write", "err", err, "addr", addr)
			return errAuth
		}
		got, err := n.readAuth(fc, cs, sh.key[:], shareID)
		if err != nil {
			n.log.Debug("auth read", "err", err, "addr", addr)
			return errAuth
		}
		remote = got
	} else {
		got, sh2, err := n.readAuthAny(fc, cs)
		if err != nil {
			n.log.Debug("auth read", "err", err, "addr", addr)
			return errAuth
		}
		remote = got
		sh = sh2
		shareID = sh.id
		if err := n.writeAuth(fc, cs, sh); err != nil {
			return errAuth
		}
	}
	if remote.DeviceID == n.id.DeviceID() {
		return errAuth
	}
	if peerID != "" && remote.DeviceID != peerID {
		n.log.Debug("peer id mismatch", "want", peerID, "got", remote.DeviceID)
		return errAuth
	}
	preferred := (n.id.DeviceID() > remote.DeviceID) == isClient
	sctx, cancel := context.WithCancel(ctx)
	release, ok := n.adopt(shareID, remote.DeviceID, preferred, addr, cancel)
	if !ok {
		cancel()
		return errAuth
	}
	if onReady != nil {
		onReady()
	}
	defer release()
	defer cancel()
	n.ensurePeer(shareID, discover.Peer{ID: remote.DeviceID, Name: remote.Name})
	label := peerLabel(remote.Name, remote.DeviceID)
	n.log.Info("connected", "peer", remote.DeviceID, "name", remote.Name, "share", shareID, "addr", addr, "dialer", isClient)
	n.addEvent(shareID, "info", "已连接 "+label)
	err := syncer.Serve(sctx, sh.engine, fc, n.log.With("peer", remote.DeviceID), func(note syncer.Note) {
		n.recordSync(shareID, label, note)
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		n.log.Info("disconnected", "peer", remote.DeviceID, "err", err)
		n.addEvent(shareID, "warn", "已断开 "+label)
	} else {
		n.log.Info("disconnected", "peer", remote.DeviceID)
		n.addEvent(shareID, "info", "已断开 "+label)
	}
	return err
}

func (n *Node) writeAuth(fc *frame.Conn, cs tls.ConnectionState, sh *liveShare) error {
	a := proto.Auth{DeviceID: n.id.DeviceID(), Name: n.cfg.DeviceName, ShareID: sh.id}
	signed, err := proto.SignAuth(n.id.Private, sh.key[:], cs, a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(signed)
	if err != nil {
		return err
	}
	return fc.Write(proto.TypeAuth, body)
}

func (n *Node) readAuth(fc *frame.Conn, cs tls.ConnectionState, key []byte, shareID string) (proto.Auth, error) {
	typ, payload, err := fc.Read(time.Now().Add(10 * time.Second))
	if err != nil {
		return proto.Auth{}, err
	}
	if typ != proto.TypeAuth {
		return proto.Auth{}, errors.New("expected auth")
	}
	var a proto.Auth
	if err := json.Unmarshal(payload, &a); err != nil {
		return proto.Auth{}, err
	}
	if a.ShareID != shareID {
		return proto.Auth{}, errors.New("share mismatch")
	}
	pub, err := identity.ParseDeviceID(a.DeviceID)
	if err != nil {
		return proto.Auth{}, err
	}
	if err := proto.VerifyAuth(pub, key, cs, a); err != nil {
		return proto.Auth{}, err
	}
	return a, nil
}

func (n *Node) readAuthAny(fc *frame.Conn, cs tls.ConnectionState) (proto.Auth, *liveShare, error) {
	typ, payload, err := fc.Read(time.Now().Add(10 * time.Second))
	if err != nil {
		return proto.Auth{}, nil, err
	}
	if typ != proto.TypeAuth {
		return proto.Auth{}, nil, errors.New("expected auth")
	}
	var a proto.Auth
	if err := json.Unmarshal(payload, &a); err != nil {
		return proto.Auth{}, nil, err
	}
	sh := n.shares[a.ShareID]
	if sh == nil {
		return proto.Auth{}, nil, errors.New("unknown share")
	}
	pub, err := identity.ParseDeviceID(a.DeviceID)
	if err != nil {
		return proto.Auth{}, nil, err
	}
	if err := proto.VerifyAuth(pub, sh.key[:], cs, a); err != nil {
		return proto.Auth{}, nil, err
	}
	return a, sh, nil
}

func shareOf(n *Node, peerID string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	suffix := "\n" + peerID
	for key := range n.peers {
		if len(key) > len(suffix) && key[len(key)-len(suffix):] == suffix {
			return key[:len(key)-len(suffix)]
		}
	}
	return ""
}

func tuneTCP(conn net.Conn) {
	if t, ok := conn.(*net.TCPConn); ok {
		_ = t.SetKeepAlive(true)
		_ = t.SetKeepAlivePeriod(15 * time.Second)
	}
}
