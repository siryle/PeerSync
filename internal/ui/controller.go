package ui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/identity"
	"github.com/netcraft/peersync/internal/invite"
	"github.com/netcraft/peersync/internal/node"
	"github.com/netcraft/peersync/internal/syncer"
	"github.com/netcraft/peersync/internal/version"
)

// ErrCancelled means the person closed the folder dialog.
var ErrCancelled = errors.New("cancelled")

// Controller edits the on-disk config and starts or stops the sync engine.
type Controller struct {
	home     string
	log      *slog.Logger
	deviceID string
	pick     func() (string, error)

	opMu    sync.Mutex
	mu      sync.Mutex
	cancel  func()
	done    chan struct{}
	running bool
	errMsg  string
}

// NewController prepares the config directory and device identity.
func NewController(home string, log *slog.Logger) (*Controller, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(filepath.Join(home, config.IdentityFile))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, config.FileName)
	cfg, exists, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := config.Save(path, cfg); err != nil {
			return nil, err
		}
	}
	return &Controller{home: home, log: log, deviceID: id.DeviceID()}, nil
}

// SetPicker installs the native folder dialog. Tests leave it unset.
func (c *Controller) SetPicker(pick func() (string, error)) {
	c.pick = pick
}

// PickFolder opens the native dialog.
func (c *Controller) PickFolder() (string, error) {
	if c.pick == nil {
		return "", errors.New("请直接填写文件夹路径")
	}
	return c.pick()
}

// State is what the window draws. It never includes a folder key.
type State struct {
	DeviceID   string      `json:"device_id"`
	DeviceName string      `json:"device_name"`
	OS         string      `json:"os"`
	OSName     string      `json:"os_name"`
	Server     string      `json:"server"`
	ListenPort int         `json:"listen_port"`
	Running    bool        `json:"running"`
	Error      string      `json:"error,omitempty"`
	PublicIP   string      `json:"public_ip,omitempty"`
	Listen     string      `json:"listen,omitempty"`
	Version    string      `json:"version"`
	Shares     []ShareView `json:"shares"`
	Logs       []LogView   `json:"logs"`
}

// LogView is one sync log line. It has no secrets.
type LogView struct {
	Time    time.Time `json:"time"`
	ShareID string    `json:"share_id,omitempty"`
	Level   string    `json:"level"`
	Text    string    `json:"text"`
}

// ShareView is one folder, without its key.
type ShareView struct {
	Label     string            `json:"label"`
	Path      string            `json:"path"`
	ShareID   string            `json:"share_id"`
	Files     int               `json:"files"`
	Discovery bool              `json:"discovery"`
	Peers     []node.PeerStatus `json:"peers"`
	Mode      config.SyncMode   `json:"mode"`
	SourceID  string            `json:"source_device_id,omitempty"`
}

// State returns the current device, folders, and live peers.
func (c *Controller) State() State {
	cfg, _, err := config.Load(filepath.Join(c.home, config.FileName))
	if err != nil {
		cfg = config.Default("")
	}
	c.mu.Lock()
	running := c.running
	errMsg := c.errMsg
	c.mu.Unlock()
	st := State{
		DeviceID:   c.deviceID,
		DeviceName: cfg.DeviceName,
		OS:         config.Platform(),
		OSName:     osName(config.Platform()),
		Server:     cfg.Server,
		ListenPort: cfg.ListenPort,
		Running:    running,
		Error:      errMsg,
		Version:    version.Current,
		Shares:     make([]ShareView, 0, len(cfg.Shares)),
		Logs:       []LogView{},
	}
	for _, sh := range cfg.Shares {
		st.Shares = append(st.Shares, ShareView{
			Label:    sh.Label,
			Path:     sh.Path,
			ShareID:  sh.ShareID,
			Peers:    []node.PeerStatus{},
			Mode:     sh.Mode.Effective(),
			SourceID: sh.SourceID,
		})
	}
	if !running {
		return st
	}
	b, err := os.ReadFile(config.StatusPath(c.home))
	if err != nil {
		return st
	}
	var live node.Status
	if json.Unmarshal(b, &live) != nil || time.Since(live.UpdatedAt) > 10*time.Second {
		return st
	}
	st.PublicIP = live.PublicIP
	st.Listen = live.Listen
	byID := map[string]node.ShareStatus{}
	for _, sh := range live.Shares {
		byID[sh.ShareID] = sh
	}
	for i := range st.Shares {
		liveSh, ok := byID[st.Shares[i].ShareID]
		if !ok {
			continue
		}
		st.Shares[i].Files = liveSh.Files
		st.Shares[i].Discovery = liveSh.Discovery
		if liveSh.Peers != nil {
			st.Shares[i].Peers = liveSh.Peers
		}
		sort.Slice(st.Shares[i].Peers, func(a, b int) bool {
			pa, pb := st.Shares[i].Peers[a], st.Shares[i].Peers[b]
			return pa.Name+pa.ID < pb.Name+pb.ID
		})
	}
	for _, ev := range live.Events {
		st.Logs = append(st.Logs, LogView{
			Time: ev.Time, ShareID: ev.ShareID, Level: ev.Level, Text: ev.Text,
		})
	}
	return st
}

// UIToken is the passphrase for a web UI that other machines can open.
// The operator sets PEERSYNC_UI_TOKEN. PeerSync does not generate one.
func (c *Controller) UIToken() (string, error) {
	tok := strings.TrimSpace(os.Getenv("PEERSYNC_UI_TOKEN"))
	if len([]rune(tok)) < 8 {
		return "", errors.New("set PEERSYNC_UI_TOKEN to at least 8 characters before exposing the web UI")
	}
	return tok, nil
}

// Start runs the sync engine until Stop.
func (c *Controller) Start() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.start()
}

// Stop waits until the engine has exited.
func (c *Controller) Stop() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.stop()
}

func (c *Controller) start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.cancel = cancel
	c.done = done
	c.running = true
	c.errMsg = ""
	home := c.home
	log := c.log
	c.mu.Unlock()
	go func() {
		err := node.Run(ctx, node.Options{Home: home, Log: log})
		c.mu.Lock()
		c.running = false
		c.cancel = nil
		if err != nil && !errors.Is(err, context.Canceled) {
			c.errMsg = err.Error()
		}
		c.mu.Unlock()
		close(done)
	}()
}

func (c *Controller) stop() {
	c.mu.Lock()
	cancel := c.cancel
	done := c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	_ = os.Remove(config.StatusPath(c.home))
}

// UpdateSettings saves the device name, discovery server, and listen port.
// A running engine is restarted so it picks up the change.
func (c *Controller) UpdateSettings(name, server string, port int) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	server, err = cleanServer(server)
	if err != nil {
		return err
	}
	port, err = cleanPort(port)
	if err != nil {
		return err
	}
	return c.mutate(func(cfg *config.Config) error {
		cfg.DeviceName = name
		cfg.Server = server
		cfg.ListenPort = port
		return nil
	})
}

// CreateShare makes a folder and returns the invite. The invite is the secret.
func (c *Controller) CreateShare(path, label string, options ...syncer.Options) (inviteCode, id, name, abs string, err error) {
	opt, err := c.syncOptions(options)
	if err != nil {
		return "", "", "", "", err
	}
	abs, err = cleanDir(path)
	if err != nil {
		return "", "", "", "", err
	}
	label, err = cleanLabel(label, filepath.Base(abs))
	if err != nil {
		return "", "", "", "", err
	}
	var code string
	err = c.mutate(func(cfg *config.Config) error {
		if err := overlap(cfg.Shares, abs); err != nil {
			return err
		}
		material, err := invite.NewShare()
		if err != nil {
			return err
		}
		material.Server = cfg.Server
		code, err = invite.Encode(material)
		if err != nil {
			return err
		}
		id = invite.IDHex(material.ShareID)
		if err := syncer.CheckConflictLocation(abs, config.IndexPath(c.home, id)); err != nil {
			return err
		}
		cfg.Shares = append(cfg.Shares, config.Share{
			Label:    label,
			Path:     abs,
			ShareID:  id,
			ShareKey: invite.KeyString(material.ShareKey),
			Mode:     opt.Mode, SourceID: opt.SourceID,
		})
		return nil
	})
	if err != nil {
		return "", "", "", "", err
	}
	return code, id, label, abs, nil
}

// JoinShare adds a folder from an invite code.
func (c *Controller) JoinShare(code, path, label string, options ...syncer.Options) (id, abs, server string, err error) {
	opt, err := c.syncOptions(options)
	if err != nil {
		return "", "", "", err
	}
	material, err := invite.Decode(code)
	if err != nil {
		return "", "", "", errors.New("邀请码无效")
	}
	abs, err = cleanDir(path)
	if err != nil {
		return "", "", "", err
	}
	label, err = cleanLabel(label, filepath.Base(abs))
	if err != nil {
		return "", "", "", err
	}
	id = invite.IDHex(material.ShareID)
	if err := syncer.CheckConflictLocation(abs, config.IndexPath(c.home, id)); err != nil {
		return "", "", "", err
	}
	err = c.mutate(func(cfg *config.Config) error {
		for _, sh := range cfg.Shares {
			if sh.ShareID == id {
				return errors.New("这台设备已经加入了这个文件夹")
			}
		}
		if err := overlap(cfg.Shares, abs); err != nil {
			return err
		}
		if material.Server != "" {
			server, err = cleanServer(material.Server)
			if err != nil {
				return err
			}
			cfg.Server = server
		} else {
			server = cfg.Server
		}
		cfg.Shares = append(cfg.Shares, config.Share{
			Label:    label,
			Path:     abs,
			ShareID:  id,
			ShareKey: invite.KeyString(material.ShareKey),
			Mode:     opt.Mode, SourceID: opt.SourceID,
		})
		return nil
	})
	if err != nil {
		return "", "", "", err
	}
	return id, abs, server, nil
}

func (c *Controller) syncOptions(options []syncer.Options) (syncer.Options, error) {
	var opt syncer.Options
	if len(options) > 0 {
		opt = options[0]
	}
	opt.Mode = opt.Mode.Effective()
	opt.SourceID = strings.TrimSpace(opt.SourceID)
	if err := config.ValidateSync(opt.Mode, opt.SourceID); err != nil {
		return opt, errors.New("请选择有效的同步模式；仅接收模式需要来源设备 ID")
	}
	if opt.Mode == config.ReceiveOnly {
		if _, err := identity.ParseDeviceID(opt.SourceID); err != nil || opt.SourceID == c.deviceID {
			return opt, errors.New("请填写另一台设备的有效设备 ID")
		}
	} else {
		opt.SourceID = ""
	}
	return opt, nil
}

func (c *Controller) UpdateShare(shareID string, mode config.SyncMode, sourceID string) error {
	opt, err := c.syncOptions([]syncer.Options{{Mode: mode, SourceID: sourceID}})
	if err != nil {
		return err
	}
	return c.mutate(func(cfg *config.Config) error {
		for i := range cfg.Shares {
			if cfg.Shares[i].ShareID == shareID {
				cfg.Shares[i].Mode, cfg.Shares[i].SourceID = opt.Mode, opt.SourceID
				return nil
			}
		}
		return errors.New("找不到这个文件夹")
	})
}

// Invite rebuilds the secret code for a folder already on this device.
func (c *Controller) Invite(shareID string) (string, error) {
	cfg, _, err := config.Load(filepath.Join(c.home, config.FileName))
	if err != nil {
		return "", err
	}
	for _, sh := range cfg.Shares {
		if sh.ShareID != shareID {
			continue
		}
		key, err := invite.ParseKey(sh.ShareKey)
		if err != nil {
			return "", err
		}
		id, err := invite.ParseIDHex(sh.ShareID)
		if err != nil {
			return "", err
		}
		return invite.Encode(invite.Material{ShareID: id, ShareKey: key, Server: cfg.Server})
	}
	return "", errors.New("找不到这个文件夹")
}

// RemoveShare drops a folder from this device. Files on disk stay.
func (c *Controller) RemoveShare(shareID string) error {
	found := false
	err := c.mutate(func(cfg *config.Config) error {
		next := cfg.Shares[:0]
		for _, sh := range cfg.Shares {
			if sh.ShareID == shareID {
				found = true
				continue
			}
			next = append(next, sh)
		}
		if !found {
			return errors.New("找不到这个文件夹")
		}
		cfg.Shares = next
		return nil
	})
	return err
}

func (c *Controller) mutate(fn func(*config.Config) error) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	was := c.running
	c.mu.Unlock()
	if was {
		c.stop()
	}
	path := filepath.Join(c.home, config.FileName)
	cfg, _, err := config.Load(path)
	if err != nil {
		if was {
			c.start()
		}
		return err
	}
	if err := fn(&cfg); err != nil {
		if was {
			c.start()
		}
		return err
	}
	if err := config.Save(path, cfg); err != nil {
		if was {
			c.start()
		}
		return err
	}
	if was {
		c.start()
	}
	return nil
}

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("请填写设备名称")
	}
	if len([]rune(s)) > 40 {
		return "", errors.New("设备名称最多 40 个字")
	}
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", errors.New("设备名称不能换行")
	}
	return s, nil
}

func cleanLabel(s, fallback string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = strings.TrimSpace(fallback)
	}
	if s == "" || s == "." || s == string(filepath.Separator) {
		s = "文件夹"
	}
	if len([]rune(s)) > 40 {
		return "", errors.New("名称最多 40 个字")
	}
	return s, nil
}

func cleanPort(p int) (int, error) {
	if p < 1 || p > 65535 {
		return 0, errors.New("端口需要在 1 到 65535 之间")
	}
	return p, nil
}

func cleanServer(s string) (string, error) {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("发现服务地址不正确")
	}
	switch u.Scheme {
	case "ws", "wss", "http", "https":
	default:
		return "", errors.New("发现服务需要以 ws:// 或 wss:// 开头")
	}
	return u.String(), nil
}

func cleanDir(path string) (string, error) {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, `"'`)
	if path == "" {
		return "", errors.New("请填写文件夹路径")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
		return abs, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("这不是文件夹")
	}
	return abs, nil
}

func overlap(shares []config.Share, abs string) error {
	for _, sh := range shares {
		if pathsOverlap(sh.Path, abs) {
			return errors.New("这个路径已经在另一个共享文件夹里面")
		}
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	return inside(a, b) || inside(b, a)
}

func inside(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func osName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return goos
	}
}
