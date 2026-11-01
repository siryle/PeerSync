package desktop

import (
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/node"
	"github.com/netcraft/peersync/internal/ui"
)

func TestMenuState(t *testing.T) {
	st := ui.State{Running: true, Version: "test", Shares: []ui.ShareView{
		{Label: "work", Path: "/work", Discovery: true, Peers: []node.PeerStatus{{ID: "A", Connected: true}, {ID: "B"}}},
		{Label: "docs", Path: "/docs", Discovery: true, Peers: []node.PeerStatus{{ID: "A", Connected: true}, {ID: "C", Connected: true}}},
	}}
	snapshot := makeMenuState(st, false, false)
	if snapshot.Title != "同步运行中" || snapshot.Summary != "2 个文件夹 · 2 台设备已连接" || snapshot.Folders[0].Path != "/docs" {
		t.Fatalf("running state: %+v", snapshot)
	}
	for _, tc := range []struct {
		name, title string
		busy, quit  bool
		mutate      func(*ui.State)
	}{
		{"paused", "同步已暂停", false, false, func(s *ui.State) { s.Running = false }},
		{"error", "同步异常", false, false, func(s *ui.State) { s.Error = "cannot listen" }},
		{"busy", "正在切换同步…", true, false, func(s *ui.State) {}},
		{"quitting", "正在退出…", true, true, func(s *ui.State) {}},
		{"empty", "尚未添加同步文件夹", false, false, func(s *ui.State) { s.Shares = nil }},
		{"connecting", "正在连接发现服务", false, false, func(s *ui.State) { s.Shares = []ui.ShareView{{Label: "docs"}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := st
			tc.mutate(&s)
			got := makeMenuState(s, tc.busy, tc.quit)
			if got.Title != tc.title || (tc.name == "paused" && !strings.Contains(got.Summary, "0 台设备")) {
				t.Fatalf("state: %+v", got)
			}
		})
	}
}

func TestMenuControlsPauseResumeReconnectQuit(t *testing.T) {
	home := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	cfg := config.Default("menu-test")
	cfg.ListenPort = port
	if err := config.Save(filepath.Join(home, config.FileName), cfg); err != nil {
		t.Fatal(err)
	}
	ctrl, err := ui.NewController(home, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctrl.Stop)
	m := &menuControls{ctrl: ctrl}
	wait := func(running bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			s := m.snapshot()
			_, err := os.Stat(config.StatusPath(home))
			if !s.Busy && s.Running == running && (running == (err == nil)) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("menu controls did not settle: %+v", m.snapshot())
	}
	ctrl.Start()
	wait(true)
	m.act(menuToggle, nil)
	wait(false)
	m.act(menuReconnect, nil)
	wait(false)
	m.act(menuToggle, nil)
	wait(true)
	m.act(menuReconnect, nil)
	wait(true)
	done := make(chan struct{})
	var calls atomic.Int32
	quit := func() { calls.Add(1); close(done) }
	m.act(menuReconnect, nil)
	m.act(menuQuit, quit)
	m.act(menuQuit, quit)
	m.act(menuToggle, nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("quit did not stop sync")
	}
	wait(false)
	if calls.Load() != 1 || !m.snapshot().Quitting {
		t.Fatalf("quit state: %+v, callbacks: %d", m.snapshot(), calls.Load())
	}
}

func TestMacMenuNative(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("AppKit window tests need a logged-in macOS desktop")
	}
	bin := filepath.Join(t.TempDir(), "menu-test")
	compile := exec.Command("xcrun", "clang", "-fobjc-arc", "-framework", "Cocoa", "-framework", "WebKit",
		"testdata/menu_test.m", "-o", bin)
	if out, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile native menu test: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, t.TempDir()).CombinedOutput(); err != nil {
		t.Fatalf("native menu test: %v\n%s", err, out)
	}
}
