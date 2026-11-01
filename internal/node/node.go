// Package node runs a PeerSync client: discovery, direct connections, and
// directory sync. File bytes stay on the direct connection.
package node

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/discover"
	"github.com/netcraft/peersync/internal/identity"
	"github.com/netcraft/peersync/internal/invite"
	"github.com/netcraft/peersync/internal/syncer"
)

// Options controls a running client.
type Options struct {
	Home         string
	Server       string
	Stun         string
	ListenPort   int
	RandomListen bool
	ScanEvery    time.Duration
	Log          *slog.Logger
}

// Status is written for `peersync status` and the desktop window.
type Status struct {
	DeviceID   string        `json:"device_id"`
	DeviceName string        `json:"device_name"`
	OS         string        `json:"os"`
	Server     string        `json:"server,omitempty"`
	Listen     string        `json:"listen,omitempty"`
	PublicIP   string        `json:"public_ip,omitempty"`
	UpdatedAt  time.Time     `json:"updated_at"`
	Shares     []ShareStatus `json:"shares"`
	Events     []Event       `json:"events,omitempty"`
}

// ShareStatus is one folder in the status file.
type ShareStatus struct {
	Label     string       `json:"label"`
	Path      string       `json:"path"`
	ShareID   string       `json:"share_id"`
	Files     int          `json:"files"`
	Discovery bool         `json:"discovery,omitempty"`
	Peers     []PeerStatus `json:"peers"`
}

// PeerStatus is another device in a folder.
type PeerStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Address   string `json:"address,omitempty"`
}

type liveShare struct {
	label     string
	path      string
	id        string
	key       [32]byte
	signalKey [32]byte
	engine    *syncer.Engine
	client    *discover.Client
}

type peerState struct {
	name      string
	connected bool
	addr      string
	tcp       []string
	udp       []string
	punched   []string
	cancel    context.CancelFunc
}

type slot struct {
	preferred bool
	token     uint64
	addr      string
	cancel    context.CancelFunc
}

// Node is one device.
type Node struct {
	opt       Options
	log       *slog.Logger
	home      string
	cfg       config.Config
	id        identity.Identity
	cert      tls.Certificate
	clientTLS *tls.Config
	serverTLS *tls.Config
	qconf     *quic.Config

	shares map[string]*liveShare

	tcpLn net.Listener
	udp   *net.UDPConn
	tr    *quic.Transport
	qln   *quic.Listener

	stunServer string
	publicIP   string
	reflex     string
	stunNonce  atomic.Uint64
	kick       chan struct{}

	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup

	mu        sync.Mutex
	peers     map[string]*peerState
	slots     map[string]*slot
	nextToken atomic.Uint64
	seenNonce map[string]uint64
	events    []Event
	eventPing chan struct{}
}

// Run serves until ctx is cancelled.
func Run(ctx context.Context, opt Options) error {
	n, err := prepare(ctx, opt)
	if err != nil {
		return err
	}
	defer n.close()
	return n.loop()
}

func prepare(ctx context.Context, opt Options) (*Node, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	home := opt.Home
	var err error
	if home == "" {
		home, err = config.Home()
		if err != nil {
			return nil, err
		}
	}
	cfg, _, err := config.Load(filepath.Join(home, config.FileName))
	if err != nil {
		return nil, err
	}
	if opt.Server != "" {
		cfg.Server = opt.Server
	}
	if cfg.DeviceName == "" {
		cfg.DeviceName = config.Default("").DeviceName
	}
	id, err := identity.LoadOrCreate(filepath.Join(home, config.IdentityFile))
	if err != nil {
		return nil, err
	}
	cert, err := id.Certificate()
	if err != nil {
		return nil, err
	}
	nctx, cancel := context.WithCancel(ctx)
	n := &Node{
		opt:       opt,
		log:       opt.Log,
		home:      home,
		cfg:       cfg,
		id:        id,
		cert:      cert,
		shares:    map[string]*liveShare{},
		ctx:       nctx,
		cancel:    cancel,
		peers:     map[string]*peerState{},
		slots:     map[string]*slot{},
		seenNonce: map[string]uint64{},
		eventPing: make(chan struct{}, 1),
		kick:      make(chan struct{}),
		clientTLS: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS13,
			NextProtos:         []string{"peersync"},
			ServerName:         "peersync",
		},
		serverTLS: &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAnyClientCert,
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"peersync"},
		},
		qconf: &quic.Config{
			MaxIdleTimeout:       45 * time.Second,
			KeepAlivePeriod:      15 * time.Second,
			HandshakeIdleTimeout: 8 * time.Second,
		},
	}
	prepared := false
	defer func() {
		if !prepared {
			n.close()
		}
	}()
	for _, sh := range cfg.Shares {
		if err := n.addShare(sh); err != nil {
			cancel()
			return nil, err
		}
	}
	port := cfg.ListenPort
	if opt.ListenPort != 0 {
		port = opt.ListenPort
	}
	if opt.RandomListen {
		port = 0
	}
	if err := n.listen(port); err != nil {
		cancel()
		return nil, err
	}
	stun := opt.Stun
	if stun == "" {
		stun, err = defaultStun(cfg.Server)
		if err != nil {
			cancel()
			return nil, err
		}
	}
	if _, _, err = net.SplitHostPort(stun); err != nil {
		cancel()
		return nil, err
	}
	n.stunServer = stun
	n.log.Info("peersync device", "id", id.DeviceID(), "name", cfg.DeviceName, "tcp", n.tcpLn.Addr(), "udp", n.udp.LocalAddr())
	prepared = true
	return n, nil
}

func (n *Node) addShare(sh config.Share) error {
	if sh.Mode == config.ReceiveOnly {
		if _, err := identity.ParseDeviceID(sh.SourceID); err != nil {
			return err
		}
		if sh.SourceID == n.id.DeviceID() {
			return errors.New("receive-only source must be another device")
		}
	}
	key, err := invite.ParseKey(sh.ShareKey)
	if err != nil {
		return err
	}
	if _, err := invite.ParseIDHex(sh.ShareID); err != nil {
		return err
	}
	abs, err := filepath.Abs(sh.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	eng, err := syncer.Open(abs, config.IndexPath(n.home, sh.ShareID), n.id.DeviceID(), n.log,
		syncer.Options{Mode: sh.Mode, SourceID: sh.SourceID})
	if err != nil {
		return err
	}
	ls := &liveShare{
		label:     sh.Label,
		path:      abs,
		id:        sh.ShareID,
		key:       key,
		signalKey: discover.SignalKey(key[:], sh.ShareID),
		engine:    eng,
	}
	ls.client = &discover.Client{
		URL:      n.cfg.Server,
		DeviceID: n.id.DeviceID(),
		Name:     n.cfg.DeviceName,
		ShareID:  sh.ShareID,
		Token:    invite.RoomToken(key[:], sh.ShareID),
		Log:      n.log,
		Handler: discover.Handler{
			Welcome: func(ip string) {
				n.notePublicIP(ip)
				n.poke()
				n.announce(sh.ShareID)
			},
			Peers:  func(peers []discover.Peer) { n.syncPeers(sh.ShareID, peers) },
			Joined: func(p discover.Peer) { n.ensurePeer(sh.ShareID, p); n.announce(sh.ShareID) },
			Left:   func(id string) { n.dropPeer(sh.ShareID, id) },
			Signal: func(from string, sealed []byte) { n.onSignal(sh.ShareID, from, sealed) },
		},
	}
	n.shares[sh.ShareID] = ls
	return nil
}

func (n *Node) listen(port int) error {
	tcp, err := net.Listen("tcp", net.JoinHostPort("", itoaPort(port)))
	if err != nil {
		return err
	}
	got := tcp.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: got})
	if err != nil {
		tcp.Close()
		if port != 0 {
			return err
		}
		tcp, err = net.Listen("tcp", ":0")
		if err != nil {
			return err
		}
		got = tcp.Addr().(*net.TCPAddr).Port
		udp, err = net.ListenUDP("udp", &net.UDPAddr{Port: got})
		if err != nil {
			tcp.Close()
			return err
		}
	}
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(n.serverTLS, n.qconf)
	if err != nil {
		tcp.Close()
		udp.Close()
		return err
	}
	n.tcpLn = tcp
	n.udp = udp
	n.tr = tr
	n.qln = ln
	return nil
}

func (n *Node) loop() error {
	every := n.opt.ScanEvery
	if every <= 0 {
		every = 20 * time.Second
	}
	var wg sync.WaitGroup
	for _, sh := range n.shares {
		sh := sh
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := sh.engine.Run(n.ctx, every); err != nil && n.ctx.Err() == nil {
				n.log.Error("watch stopped", "share", sh.id, "err", err)
			}
		}()
		go func() {
			defer wg.Done()
			sh.client.Run(n.ctx)
		}()
	}
	wg.Add(6)
	go func() { defer wg.Done(); n.acceptTCP() }()
	go func() { defer wg.Done(); n.acceptQUIC() }()
	go func() { defer wg.Done(); n.readUDP() }()
	go func() { defer wg.Done(); n.stunLoop() }()
	go func() { defer wg.Done(); n.announceLoop() }()
	go func() { defer wg.Done(); n.statusLoop() }()

	<-n.ctx.Done()
	n.closeListeners()
	wg.Wait()
	return nil
}

func (n *Node) closeListeners() {
	if n.qln != nil {
		_ = n.qln.Close()
	}
	if n.tr != nil {
		_ = n.tr.Close()
	}
	if n.tcpLn != nil {
		_ = n.tcpLn.Close()
	}
	if n.udp != nil {
		_ = n.udp.Close()
	}
}

func (n *Node) close() {
	n.cancel()
	n.closeListeners()
	n.workers.Wait()
	for _, sh := range n.shares {
		_ = sh.engine.Close()
	}
}

func (n *Node) stunLoop() {
	n.stunOnce()
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
			n.stunOnce()
		}
	}
}

func (n *Node) stunOnce() {
	if n.stunServer == "" || n.tr == nil {
		return
	}
	addr, err := net.ResolveUDPAddr("udp", n.stunServer)
	if err != nil {
		n.log.Debug("stun resolve", "server", n.stunServer, "err", err)
		return
	}
	nonce := randomNonce()
	n.stunNonce.Store(nonce)
	_, _ = n.tr.WriteTo(reflectRequest(nonce), addr)
}

func (n *Node) readUDP() {
	buf := make([]byte, 2048)
	for {
		nread, addr, err := n.tr.ReadNonQUICPacket(n.ctx, buf)
		if err != nil {
			return
		}
		p := append([]byte(nil), buf[:nread]...)
		if nonce, public, ok := parseReflect(p); ok {
			if nonce == n.stunNonce.Load() && usableObserved(public) {
				n.mu.Lock()
				changed := n.reflex != public
				n.reflex = public
				n.mu.Unlock()
				if changed {
					n.log.Info("public udp address", "addr", public)
					n.announceAll()
				}
			}
			continue
		}
		from, to, ok := parsePunch(p)
		if !ok || !to.Equal(n.id.Public) {
			continue
		}
		n.notePunch(from, addr)
	}
}

func (n *Node) notePunch(from ed25519.PublicKey, addr net.Addr) {
	id := identity.DeviceID(from)
	hp := addr.String()
	n.mu.Lock()
	defer n.mu.Unlock()
	for key, st := range n.peers {
		if !hasDevice(key, id) {
			continue
		}
		if !contains(st.punched, hp) {
			st.punched = append(st.punched, hp)
			n.log.Debug("udp punch", "peer", id, "addr", hp)
		}
	}
}

func (n *Node) announceLoop() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	n.announceAll()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
			n.announceAll()
		}
	}
}

func (n *Node) announceAll() {
	for id := range n.shares {
		n.announce(id)
	}
}

func (n *Node) announce(shareID string) {
	sh := n.shares[shareID]
	if sh == nil {
		return
	}
	tcp, udp := n.ourCandidates()
	n.mu.Lock()
	var targets []string
	prefix := shareID + "\n"
	for key, st := range n.peers {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			targets = append(targets, key[len(prefix):])
			_ = st
		}
	}
	n.mu.Unlock()
	for _, to := range targets {
		body := discover.SignalBody{
			From: n.id.DeviceID(),
			To:   to,
			TS:   time.Now().UnixMilli(),
			N:    randomNonce(),
			TCP:  tcp,
			UDP:  udp,
		}
		plain, err := json.Marshal(body)
		if err != nil {
			continue
		}
		sealed, err := discover.Seal(sh.signalKey, plain)
		if err != nil {
			continue
		}
		if err := sh.client.Signal(to, sealed); err != nil {
			n.log.Debug("signal", "err", err, "to", to)
		}
	}
}

func (n *Node) onSignal(shareID, from string, sealed []byte) {
	sh := n.shares[shareID]
	if sh == nil {
		return
	}
	plain, err := discover.Open(sh.signalKey, sealed)
	if err != nil {
		n.log.Debug("signal rejected", "err", err)
		return
	}
	var body discover.SignalBody
	if err := json.Unmarshal(plain, &body); err != nil {
		return
	}
	if body.From != from || body.To != n.id.DeviceID() {
		return
	}
	if abs64(time.Now().UnixMilli()-body.TS) > int64(2*time.Minute/time.Millisecond) {
		return
	}
	n.mu.Lock()
	if n.seenNonce[from] == body.N && body.N != 0 {
		n.mu.Unlock()
		return
	}
	n.seenNonce[from] = body.N
	key := shareID + "\n" + from
	st := n.peers[key]
	if st == nil {
		st = &peerState{name: from}
		n.peers[key] = st
	}
	st.tcp = append([]string(nil), body.TCP...)
	st.udp = append([]string(nil), body.UDP...)
	name := st.name
	n.mu.Unlock()
	n.ensurePeer(shareID, discover.Peer{ID: from, Name: name})
}

func (n *Node) syncPeers(shareID string, peers []discover.Peer) {
	want := map[string]discover.Peer{}
	for _, p := range peers {
		if p.ID == "" || p.ID == n.id.DeviceID() {
			continue
		}
		want[p.ID] = p
		n.ensurePeer(shareID, p)
	}
	n.mu.Lock()
	var drop []string
	prefix := shareID + "\n"
	for key := range n.peers {
		if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
			continue
		}
		id := key[len(prefix):]
		if _, ok := want[id]; !ok {
			drop = append(drop, id)
		}
	}
	n.mu.Unlock()
	for _, id := range drop {
		n.dropPeer(shareID, id)
	}
	n.announce(shareID)
}

func (n *Node) ensurePeer(shareID string, p discover.Peer) {
	if p.ID == "" || p.ID == n.id.DeviceID() {
		return
	}
	key := shareID + "\n" + p.ID
	n.mu.Lock()
	st := n.peers[key]
	if st != nil && st.cancel != nil {
		if p.Name != "" {
			st.name = p.Name
		}
		n.mu.Unlock()
		return
	}
	if st == nil {
		st = &peerState{}
		n.peers[key] = st
	}
	if p.Name != "" {
		st.name = p.Name
	}
	ctx, cancel := context.WithCancel(n.ctx)
	st.cancel = cancel
	n.mu.Unlock()
	n.workers.Go(func() { n.watch(ctx, shareID, p.ID) })
}

func (n *Node) dropPeer(shareID, id string) {
	key := shareID + "\n" + id
	n.mu.Lock()
	st := n.peers[key]
	if st != nil && st.cancel != nil {
		st.cancel()
		st.cancel = nil
	}
	n.mu.Unlock()
}

func (n *Node) notePublicIP(ip string) {
	if !usableObserved(ip) {
		if ip != "" {
			n.log.Info("discovery connected", "ignored_addr", ip)
			return
		}
		n.log.Info("discovery connected")
		return
	}
	n.mu.Lock()
	changed := n.publicIP != ip
	n.publicIP = ip
	n.mu.Unlock()
	if changed {
		n.log.Info("discovery sees this device as", "ip", ip)
		return
	}
	n.log.Info("discovery connected")
}

// poke wakes peer dialers so a discovery session that just came back does not
// wait out the previous backoff.
func (n *Node) poke() {
	n.mu.Lock()
	ch := n.kick
	n.kick = make(chan struct{})
	n.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (n *Node) ourCandidates() (tcp, udp []string) {
	tcpPort := n.tcpLn.Addr().(*net.TCPAddr).Port
	udpPort := n.udp.LocalAddr().(*net.UDPAddr).Port
	ips := localIPs()
	n.mu.Lock()
	pub := n.publicIP
	reflex := n.reflex
	n.mu.Unlock()
	loop := pub == "" || isLoopbackHost(pub)
	tcp = hostports(ips, tcpPort, pub, loop)
	udp = hostports(ips, udpPort, reflex, loop)
	return tcp, udp
}

func (n *Node) peerCandidates(shareID, peerID string) (tcp, udp []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.peers[shareID+"\n"+peerID]
	if st == nil {
		return nil, nil
	}
	tcp = append([]string(nil), st.tcp...)
	udp = append([]string(nil), st.udp...)
	udp = append(udp, st.punched...)
	return tcp, udp
}

func (n *Node) connected(shareID, peerID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.slots[shareID+"\n"+peerID]
	return ok
}

func (n *Node) adopt(shareID, peerID string, preferred bool, addr string, cancel context.CancelFunc) (func(), bool) {
	key := shareID + "\n" + peerID
	token := n.nextToken.Add(1)
	n.mu.Lock()
	defer n.mu.Unlock()
	if cur, ok := n.slots[key]; ok {
		if cur.preferred || !preferred {
			return nil, false
		}
		cur.cancel()
	}
	n.slots[key] = &slot{preferred: preferred, token: token, addr: addr, cancel: cancel}
	if st := n.peers[key]; st != nil {
		st.connected = true
		st.addr = addr
	}
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		cur, ok := n.slots[key]
		if !ok || cur.token != token {
			return
		}
		delete(n.slots, key)
		if st := n.peers[key]; st != nil && st.addr == addr {
			st.connected = false
			st.addr = ""
		}
	}, true
}

func (n *Node) statusLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	n.writeStatus()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
			n.writeStatus()
		case <-n.eventPing:
			n.writeStatus()
		}
	}
}

func (n *Node) writeStatus() {
	st := Status{
		DeviceID:   n.id.DeviceID(),
		DeviceName: n.cfg.DeviceName,
		OS:         config.Platform(),
		Server:     n.cfg.Server,
		UpdatedAt:  time.Now().UTC(),
	}
	if n.tcpLn != nil {
		st.Listen = n.tcpLn.Addr().String()
	}
	n.mu.Lock()
	st.PublicIP = n.publicIP
	for _, sh := range n.shares {
		files, _ := sh.engine.Stats()
		ss := ShareStatus{
			Label:     sh.label,
			Path:      sh.path,
			ShareID:   sh.id,
			Files:     files,
			Discovery: sh.client != nil && sh.client.Connected(),
		}
		prefix := sh.id + "\n"
		for key, peer := range n.peers {
			if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
				continue
			}
			ss.Peers = append(ss.Peers, PeerStatus{
				ID:        key[len(prefix):],
				Name:      peer.name,
				Connected: peer.connected,
				Address:   peer.addr,
			})
		}
		st.Shares = append(st.Shares, ss)
	}
	if len(n.events) > 0 {
		st.Events = append([]Event(nil), n.events...)
	}
	n.mu.Unlock()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	path := config.StatusPath(n.home)
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
}

func defaultStun(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("server url has no host")
	}
	return net.JoinHostPort(host, "3478"), nil
}

func itoaPort(port int) string {
	return strconv.Itoa(port)
}

func hasDevice(key, id string) bool {
	return len(key) > len(id) && key[len(key)-len(id):] == id && key[len(key)-len(id)-1] == '\n'
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
