package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/desktop"
	"github.com/netcraft/peersync/internal/identity"
	"github.com/netcraft/peersync/internal/invite"
	"github.com/netcraft/peersync/internal/node"
	"github.com/netcraft/peersync/internal/ui"
	"github.com/netcraft/peersync/internal/version"
)

func main() {
	if launchGUI(os.Args[1:]) {
		if err := desktop.Run(); err != nil {
			fatal(err)
		}
		return
	}
	desktop.EnsureConsole()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "id":
		cmdID(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "share":
		cmdShare(os.Args[2:])
	case "gui":
		if err := desktop.Run(); err != nil {
			fatal(err)
		}
	case "version", "-v", "--version":
		fmt.Println("peersync", version.Current)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func launchGUI(args []string) bool {
	if len(args) > 0 {
		return false
	}
	if version.Window == "1" {
		return true
	}
	return runtime.GOOS == "darwin" && desktop.Available()
}

func usage() {
	fmt.Fprintf(os.Stderr, `peersync %s — end-to-end directory sync

Usage:
  peersync                  Open the window on macOS and Windows
  peersync gui
  peersync init [--name name]
  peersync serve [--server ws://host:8080] [--listen 42420]
  peersync id
  peersync status
  peersync share create --path DIR [--label name]
  peersync share join --invite CODE --path DIR [--server URL]
  peersync share ls
  peersync share mode --mode MODE [--source DEVICE_ID] SHARE_ID
  peersync share rm SHARE_ID

The discovery server only introduces devices. Directory contents move
directly between clients. macOS and Windows open a window. Docker and
"peersync serve" also serve that page; see PEERSYNC_UI.

Config directory: PEERSYNC_HOME, or the OS config dir /peersync.
`, version.Current)
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	name := fs.String("name", "", "device name")
	home := fs.String("home", "", "config directory")
	_ = fs.Parse(args)
	dir := mustHome(*home)
	id, err := identity.LoadOrCreate(filepath.Join(dir, config.IdentityFile))
	if err != nil {
		fatal(err)
	}
	path := filepath.Join(dir, config.FileName)
	cfg, exists, err := config.Load(path)
	if err != nil {
		fatal(err)
	}
	if *name != "" {
		cfg.DeviceName = *name
	}
	if !exists || *name != "" {
		if err := config.Save(path, cfg); err != nil {
			fatal(err)
		}
	}
	fmt.Printf("device: %s\nname:   %s\nconfig: %s\n", id.DeviceID(), cfg.DeviceName, dir)
}

func cmdID(args []string) {
	fs := flag.NewFlagSet("id", flag.ExitOnError)
	home := fs.String("home", "", "config directory")
	_ = fs.Parse(args)
	dir := mustHome(*home)
	id, err := identity.LoadOrCreate(filepath.Join(dir, config.IdentityFile))
	if err != nil {
		fatal(err)
	}
	fmt.Println(id.DeviceID())
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	homeFlag := fs.String("home", "", "config directory")
	server := fs.String("server", "", "discovery server, for example ws://127.0.0.1:8080")
	listen := fs.Int("listen", 0, "tcp/udp port (0 keeps the config value)")
	verbose := fs.Bool("verbose", false, "debug logs")
	_ = fs.Parse(args)

	level := slog.LevelInfo
	if *verbose || os.Getenv("PEERSYNC_VERBOSE") == "1" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	dir := mustHome(*homeFlag)
	if err := applyEnv(dir, *server); err != nil {
		fatal(err)
	}
	port := *listen
	if port == 0 {
		if v := os.Getenv("PEERSYNC_LISTEN"); v != "" {
			var n int
			if _, err := fmt.Sscan(v, &n); err == nil {
				port = n
			}
		}
	}
	if port != 0 {
		cfg := mustConfig(dir)
		cfg.ListenPort = port
		if err := config.Save(filepath.Join(dir, config.FileName), cfg); err != nil {
			fatal(err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctrl, err := ui.NewController(dir, slog.Default())
	if err != nil {
		fatal(err)
	}
	page, _, err := ui.Listen(ctx, ctrl, uiListen())
	if err != nil && os.Getenv("PEERSYNC_UI") == "" && errors.Is(err, syscall.EADDRINUSE) {
		page, _, err = ui.Listen(ctx, ctrl, "127.0.0.1:0")
	}
	if err != nil {
		fatal(err)
	}
	fmt.Printf("UI=%s\n", page)
	ctrl.Start()
	defer ctrl.Stop()
	<-ctx.Done()
}

func uiListen() string {
	if v := strings.TrimSpace(os.Getenv("PEERSYNC_UI")); v != "" {
		return v
	}
	return "127.0.0.1:8384"
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	home := fs.String("home", "", "config directory")
	_ = fs.Parse(args)
	dir := mustHome(*home)
	b, err := os.ReadFile(config.StatusPath(dir))
	if err != nil {
		fmt.Fprintln(os.Stderr, "peersync is not running")
		os.Exit(1)
	}
	var st node.Status
	if err := json.Unmarshal(b, &st); err != nil {
		fatal(err)
	}
	if time.Since(st.UpdatedAt) > 15*time.Second {
		fmt.Fprintln(os.Stderr, "peersync is not running")
		os.Exit(1)
	}
	fmt.Printf("%s  %s  %s\n", st.DeviceName, st.DeviceID, st.OS)
	if len(st.Shares) == 0 {
		fmt.Println("no shared folders")
		return
	}
	for _, sh := range st.Shares {
		label := sh.Label
		if label == "" {
			label = sh.ShareID
		}
		fmt.Printf("\n%s  %s  (%d paths)\n", label, sh.Path, sh.Files)
		if len(sh.Peers) == 0 {
			fmt.Println("  no peers")
			continue
		}
		for _, p := range sh.Peers {
			state := "offline"
			if p.Connected {
				state = "connected"
			}
			name := p.Name
			if name == "" {
				name = p.ID
			}
			extra := ""
			if p.Address != "" {
				extra = "  " + p.Address
			}
			fmt.Printf("  %-12s  %s  %s%s\n", state, name, p.ID, extra)
		}
	}
}

func cmdShare(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		cmdShareCreate(args[1:])
	case "join":
		cmdShareJoin(args[1:])
	case "ls":
		cmdShareLs(args[1:])
	case "rm":
		cmdShareRm(args[1:])
	case "mode":
		cmdShareMode(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func cmdShareCreate(args []string) {
	fs := flag.NewFlagSet("share create", flag.ExitOnError)
	home := fs.String("home", "", "config directory")
	path := fs.String("path", "", "directory to share")
	label := fs.String("label", "", "short name")
	server := fs.String("server", "", "discovery server stored in the invite")
	mode := fs.String("mode", string(config.SendReceive), "send-receive, send-only, or receive-only")
	source := fs.String("source", "", "source device ID for receive-only")
	_ = fs.Parse(args)
	if err := validateShareMode(config.SyncMode(*mode), *source); err != nil {
		fatal(err)
	}
	if *path == "" {
		fatal(errors.New("--path is required"))
	}
	dir := mustHome(*home)
	cfg := mustConfig(dir)
	if *server != "" {
		cfg.Server = *server
	}
	abs, err := filepath.Abs(*path)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		fatal(err)
	}
	material, err := invite.NewShare()
	if err != nil {
		fatal(err)
	}
	material.Server = cfg.Server
	code, err := invite.Encode(material)
	if err != nil {
		fatal(err)
	}
	if *label == "" {
		*label = filepath.Base(abs)
	}
	cfg.Shares = append(cfg.Shares, config.Share{
		Label:    *label,
		Path:     abs,
		ShareID:  invite.IDHex(material.ShareID),
		ShareKey: invite.KeyString(material.ShareKey),
		Mode:     config.SyncMode(*mode), SourceID: *source,
	})
	if err := config.Save(filepath.Join(dir, config.FileName), cfg); err != nil {
		fatal(err)
	}
	fmt.Printf("label:  %s\npath:   %s\nshare:  %s\ninvite: %s\n", *label, abs, invite.IDHex(material.ShareID), code)
	fmt.Fprintln(os.Stderr, "The invite is a secret. Anyone who has it can join this folder.")
}

func cmdShareJoin(args []string) {
	fs := flag.NewFlagSet("share join", flag.ExitOnError)
	home := fs.String("home", "", "config directory")
	code := fs.String("invite", "", "invite code")
	path := fs.String("path", "", "local directory")
	label := fs.String("label", "", "short name")
	server := fs.String("server", "", "override the discovery server in the invite")
	mode := fs.String("mode", string(config.SendReceive), "send-receive, send-only, or receive-only")
	source := fs.String("source", "", "source device ID for receive-only")
	_ = fs.Parse(args)
	if err := validateShareMode(config.SyncMode(*mode), *source); err != nil {
		fatal(err)
	}
	if *code == "" || *path == "" {
		fatal(errors.New("--invite and --path are required"))
	}
	material, err := invite.Decode(*code)
	if err != nil {
		fatal(err)
	}
	if *server != "" {
		material.Server = *server
	}
	dir := mustHome(*home)
	cfg := mustConfig(dir)
	id := invite.IDHex(material.ShareID)
	for _, sh := range cfg.Shares {
		if sh.ShareID == id {
			fatal(errors.New("this folder is already joined"))
		}
	}
	abs, err := filepath.Abs(*path)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		fatal(err)
	}
	if *label == "" {
		*label = filepath.Base(abs)
	}
	if material.Server != "" {
		cfg.Server = material.Server
	}
	cfg.Shares = append(cfg.Shares, config.Share{
		Label:    *label,
		Path:     abs,
		ShareID:  id,
		ShareKey: invite.KeyString(material.ShareKey),
		Mode:     config.SyncMode(*mode), SourceID: *source,
	})
	if err := config.Save(filepath.Join(dir, config.FileName), cfg); err != nil {
		fatal(err)
	}
	fmt.Printf("joined %s\npath:  %s\nserver: %s\n", id, abs, cfg.Server)
}

func cmdShareLs(args []string) {
	fs := flag.NewFlagSet("share ls", flag.ExitOnError)
	home := fs.String("home", "", "config directory")
	_ = fs.Parse(args)
	cfg := mustConfig(mustHome(*home))
	if len(cfg.Shares) == 0 {
		fmt.Println("no shared folders")
		return
	}
	for _, sh := range cfg.Shares {
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", sh.ShareID, sh.Label, sh.Path, sh.Mode.Effective(), sh.SourceID)
	}
}

func cmdShareRm(args []string) {
	fs := flag.NewFlagSet("share rm", flag.ExitOnError)
	home := fs.String("home", "", "config directory")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fatal(errors.New("usage: peersync share rm SHARE_ID"))
	}
	dir := mustHome(*home)
	cfg := mustConfig(dir)
	id := fs.Arg(0)
	next := cfg.Shares[:0]
	found := false
	for _, sh := range cfg.Shares {
		if sh.ShareID == id {
			found = true
			continue
		}
		next = append(next, sh)
	}
	if !found {
		fatal(errors.New("share not found"))
	}
	cfg.Shares = next
	if err := config.Save(filepath.Join(dir, config.FileName), cfg); err != nil {
		fatal(err)
	}
	fmt.Println("removed", id)
}

func validateShareMode(mode config.SyncMode, source string) error {
	if err := config.ValidateSync(mode, source); err != nil {
		return err
	}
	if mode == config.ReceiveOnly {
		_, err := identity.ParseDeviceID(source)
		return err
	}
	return nil
}

func cmdShareMode(args []string) {
	fs := flag.NewFlagSet("share mode", flag.ExitOnError)
	home := fs.String("home", "", "config directory")
	mode := fs.String("mode", "", "send-receive, send-only, or receive-only")
	source := fs.String("source", "", "source device ID for receive-only")
	_ = fs.Parse(args)
	if *mode == "" || fs.NArg() != 1 {
		fatal(errors.New("usage: peersync share mode --mode MODE [--source DEVICE_ID] SHARE_ID"))
	}
	if err := validateShareMode(config.SyncMode(*mode), *source); err != nil {
		fatal(err)
	}
	dir := mustHome(*home)
	cfg := mustConfig(dir)
	for i := range cfg.Shares {
		if cfg.Shares[i].ShareID != fs.Arg(0) {
			continue
		}
		cfg.Shares[i].Mode, cfg.Shares[i].SourceID = config.SyncMode(*mode), *source
		if err := config.Save(filepath.Join(dir, config.FileName), cfg); err != nil {
			fatal(err)
		}
		fmt.Println("mode saved; restart PeerSync to apply")
		return
	}
	fatal(errors.New("share not found"))
}

func applyEnv(home, serverFlag string) error {
	path := filepath.Join(home, config.FileName)
	cfg, exists, err := config.Load(path)
	if err != nil {
		return err
	}
	if name := os.Getenv("PEERSYNC_NAME"); name != "" && (!exists || cfg.DeviceName == "" || os.Getenv("PEERSYNC_NAME_FORCE") == "1") {
		cfg.DeviceName = name
	}
	if serverFlag != "" {
		cfg.Server = serverFlag
	} else if env := os.Getenv("PEERSYNC_SERVER"); env != "" {
		cfg.Server = env
	}
	syncPath := os.Getenv("PEERSYNC_PATH")
	mode := config.SyncMode(os.Getenv("PEERSYNC_MODE")).Effective()
	source := os.Getenv("PEERSYNC_SOURCE_DEVICE_ID")
	if err := validateShareMode(mode, source); err != nil {
		return err
	}
	if os.Getenv("PEERSYNC_CREATE") == "1" && len(cfg.Shares) == 0 {
		if syncPath == "" {
			syncPath = "/sync"
		}
		abs, err := filepath.Abs(syncPath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return err
		}
		material, err := invite.NewShare()
		if err != nil {
			return err
		}
		material.Server = cfg.Server
		code, err := invite.Encode(material)
		if err != nil {
			return err
		}
		label := os.Getenv("PEERSYNC_LABEL")
		if label == "" {
			label = filepath.Base(abs)
		}
		cfg.Shares = append(cfg.Shares, config.Share{
			Label:    label,
			Path:     abs,
			ShareID:  invite.IDHex(material.ShareID),
			ShareKey: invite.KeyString(material.ShareKey),
			Mode:     mode, SourceID: source,
		})
		fmt.Printf("INVITE=%s\n", code)
	}
	if code := os.Getenv("PEERSYNC_INVITE"); code != "" {
		material, err := invite.Decode(code)
		if err != nil {
			return err
		}
		if env := os.Getenv("PEERSYNC_SERVER"); env != "" {
			material.Server = env
		}
		id := invite.IDHex(material.ShareID)
		have := false
		for _, sh := range cfg.Shares {
			if sh.ShareID == id {
				have = true
			}
		}
		if !have {
			if syncPath == "" {
				syncPath = "/sync"
			}
			abs, err := filepath.Abs(syncPath)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return err
			}
			label := os.Getenv("PEERSYNC_LABEL")
			if label == "" {
				label = filepath.Base(abs)
			}
			if material.Server != "" {
				cfg.Server = material.Server
			}
			cfg.Shares = append(cfg.Shares, config.Share{
				Label:    label,
				Path:     abs,
				ShareID:  id,
				ShareKey: invite.KeyString(material.ShareKey),
				Mode:     mode, SourceID: source,
			})
			fmt.Printf("joined %s at %s\n", id, abs)
		}
	}
	return config.Save(path, cfg)
}

func mustHome(flagHome string) string {
	if flagHome != "" {
		if err := os.MkdirAll(flagHome, 0o700); err != nil {
			fatal(err)
		}
		return flagHome
	}
	dir, err := config.Home()
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fatal(err)
	}
	return dir
}

func mustConfig(home string) config.Config {
	cfg, _, err := config.Load(filepath.Join(home, config.FileName))
	if err != nil {
		fatal(err)
	}
	return cfg
}

func fatal(err error) {
	desktop.ReportError(err)
	os.Exit(1)
}
