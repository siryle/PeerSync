package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCtrl(t *testing.T) (*Controller, string) {
	t.Helper()
	home := t.TempDir()
	ctrl, err := NewController(home, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return ctrl, home
}

func testPage(t *testing.T, ctrl *Controller) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	page, err := Serve(ctx, ctrl)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func call(t *testing.T, page, path string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, page+strings.TrimPrefix(path, "/"), r)
	if err != nil {
		t.Fatal(err)
	}
	if body == nil && !strings.Contains(path, "state") && path != "/" {
		req, err = http.NewRequest(http.MethodGet, page+strings.TrimPrefix(path, "/"), nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if path == "/api/state" || path == "/" || path == "/app.js" {
		req, err = http.NewRequest(http.MethodGet, page+strings.TrimPrefix(path, "/"), nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	req.Header.Set("X-PeerSync", "1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, b
}

func TestWindowAPI(t *testing.T) {
	ctrl, home := testCtrl(t)
	page := testPage(t, ctrl)

	status, body := call(t, page, "/", nil)
	if status != 200 || !bytes.Contains(body, []byte("PeerSync")) {
		t.Fatalf("page %d %s", status, body)
	}
	status, body = call(t, page, "/logo.png", nil)
	if status != 200 || len(body) < 1000 {
		t.Fatalf("logo %d len %d", status, len(body))
	}

	req, _ := http.NewRequest(http.MethodGet, page+"api/state", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("missing header status %d", res.StatusCode)
	}

	status, body = call(t, page, "/api/state", nil)
	if status != 200 {
		t.Fatal(string(body))
	}
	var st State
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.DeviceID, "PS") || st.Version == "" {
		t.Fatalf("state %+v", st)
	}

	dir := filepath.Join(home, "docs")
	status, body = call(t, page, "/api/shares", map[string]string{"path": dir, "label": "文档"})
	if status != 200 {
		t.Fatal(string(body))
	}
	var created struct {
		Invite  string `json:"invite"`
		ShareID string `json:"share_id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Invite, "peersync1_") {
		t.Fatal(created.Invite)
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("share_key")) {
		t.Fatal("invite response leaked the folder key field")
	}
	if !bytes.Contains(raw, []byte("share_key")) {
		t.Fatal("config lost the folder key")
	}

	status, body = call(t, page, "/api/shares", map[string]string{"path": filepath.Join(dir, "nested"), "label": "nope"})
	if status == 200 {
		t.Fatalf("nested share was accepted: %s", body)
	}

	other := filepath.Join(home, "copy")
	status, body = call(t, page, "/api/shares/join", map[string]string{
		"invite": created.Invite,
		"path":   other,
	})
	if status == 200 {
		t.Fatalf("joining the same folder twice was accepted: %s", body)
	}
	status, body = call(t, page, "/api/shares/join", map[string]string{
		"invite": "peersync1_nope",
		"path":   other,
	})
	if status == 200 {
		t.Fatal("bad invite accepted")
	}

	status, body = call(t, page, "/api/shares/invite", map[string]string{"share_id": created.ShareID})
	if status != 200 || !bytes.Contains(body, []byte("peersync1_")) {
		t.Fatalf("invite %d %s", status, body)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	status, body = call(t, page, "/api/settings", map[string]any{
		"device_name": "书房",
		"server":      "ws://127.0.0.1:9",
		"listen_port": port,
	})
	if status != 200 {
		t.Fatal(string(body))
	}

	ctrl.Start()
	deadline := time.Now().Add(5 * time.Second)
	var live State
	for time.Now().Before(deadline) {
		live = ctrl.State()
		if live.Error != "" || live.Listen != "" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if live.Error != "" {
		t.Fatal(live.Error)
	}
	if !live.Running || live.DeviceName != "书房" {
		t.Fatalf("not running: %+v", live)
	}
	ctrl.Stop()
	if ctrl.State().Running {
		t.Fatal("still running")
	}

	status, body = call(t, page, "/api/shares/remove", map[string]string{"share_id": created.ShareID})
	if status != 200 {
		t.Fatal(string(body))
	}
	if len(ctrl.State().Shares) != 0 {
		t.Fatal("share remained")
	}
}

func TestExposedUIRequiresToken(t *testing.T) {
	ctrl, _ := testCtrl(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	page, token, err := Listen(ctx, ctrl, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		t.Fatalf("loopback UI should not need a token, page %s", page)
	}

	t.Setenv("PEERSYNC_UI_TOKEN", "")
	if _, _, err = Listen(ctx, ctrl, "0.0.0.0:0"); err == nil {
		t.Fatal("exposed UI started without PEERSYNC_UI_TOKEN")
	}
	t.Setenv("PEERSYNC_UI_TOKEN", "short")
	if _, _, err = Listen(ctx, ctrl, "0.0.0.0:0"); err == nil {
		t.Fatal("exposed UI accepted a short token")
	}
	const want = "demo-passphrase"
	t.Setenv("PEERSYNC_UI_TOKEN", want)
	page, token, err = Listen(ctx, ctrl, "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	if token != want {
		t.Fatalf("token %q", token)
	}
	u, err := http.NewRequest(http.MethodGet, page+"api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(u.URL.Host)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsUnspecified() {
		t.Fatalf("page host %s", host)
	}
	u.URL.Host = net.JoinHostPort("127.0.0.1", port)
	res, err := http.DefaultClient.Do(u)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", res.StatusCode)
	}
	u.Header.Set("X-PeerSync-Token", token)
	res, err = http.DefaultClient.Do(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
}
