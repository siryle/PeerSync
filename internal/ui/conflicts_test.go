package ui

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/identity"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/syncer"
	"github.com/netcraft/peersync/internal/vclock"
)

func TestShareModeAPI(t *testing.T) {
	ctrl, _ := testCtrl(t)
	page := testPage(t, ctrl)
	_, id, _, _, err := ctrl.CreateShare(t.TempDir(), "source", syncer.Options{Mode: config.SendOnly})
	if err != nil {
		t.Fatal(err)
	}
	source, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode, source string
		ok           bool
	}{
		{"invalid", "", false},
		{"receive-only", "", false},
		{"receive-only", ctrl.deviceID, false},
		{"receive-only", "../source", false},
		{"receive-only", source.DeviceID(), true},
		{"send-only", "", true},
		{"send-receive", "", true},
	} {
		status, body := call(t, page, "/api/shares/mode", map[string]string{
			"share_id": id, "mode": tc.mode, "source_device_id": tc.source,
		})
		if (status == 200) != tc.ok {
			t.Fatalf("%+v: %d %s", tc, status, body)
		}
		if tc.ok {
			sh := ctrl.State().Shares[0]
			if string(sh.Mode) != tc.mode || sh.SourceID != tc.source {
				t.Fatalf("wrong mode state: %+v", sh)
			}
		}
	}
	status, body := call(t, page, "/api/shares", map[string]string{"path": t.TempDir(), "mode": "invalid"})
	if status == 200 {
		t.Fatalf("created a share with invalid mode: %s", body)
	}
}

func TestConflictAPICompareDownloadAndRestore(t *testing.T) {
	ctrl, home := testCtrl(t)
	page := testPage(t, ctrl)
	_, id, _, root, err := ctrl.CreateShare(t.TempDir(), "project")
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "main.go")
	if err := os.WriteFile(filename, []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := syncer.Open(root, config.IndexPath(home, id), ctrl.deviceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
	_, entries := e.Entries()
	sum, blocks, size, err := syncer.HashReader(bytes.NewBufferString("package current\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := proto.Entry{Path: "main.go", Size: size, Mode: 0o644, ModTime: time.Now().UnixNano(),
		Sum: proto.EncodeSum(sum, true), Version: vclock.Vector{"other": 1}}
	task := syncer.Task{Op: "conflict", Path: r.Path, Remote: r, Had: true, Seen: entries[0].Version,
		Version: vclock.Merge(entries[0].Version, r.Version)}
	tmp := filepath.Join(root, "download.tmp")
	if err := os.WriteFile(tmp, []byte("package current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.CommitDownload(task, tmp, blocks); err != nil {
		t.Fatal(err)
	}
	status, body := call(t, page, "/api/conflicts?share_id="+id, nil)
	var items []syncer.Conflict
	if status != 200 || json.Unmarshal(body, &items) != nil || len(items) != 1 {
		t.Fatalf("backup list: %d %s", status, body)
	}
	endpoint := "/api/conflicts/" + items[0].ID
	status, body = call(t, page, endpoint+"?share_id="+id, nil)
	var view syncer.ConflictView
	if status != 200 || json.Unmarshal(body, &view) != nil || view.BackupText != "package old\n" || view.CurrentText != "package current\n" {
		t.Fatalf("compare: %d %s", status, body)
	}
	status, body = call(t, page, endpoint+"/download?share_id="+id, nil)
	if status != 200 || string(body) != "package old\n" {
		t.Fatalf("download: %d %s", status, body)
	}
	res, err := http.Get(page + "api/conflicts/" + items[0].ID + "/download?share_id=" + id)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatal("backup download bypassed API authentication")
	}
	if err := os.WriteFile(filename, []byte("package newer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, body = call(t, page, endpoint+"/restore", map[string]string{"share_id": id, "expected_sum": view.CurrentSum})
	if status == 200 {
		t.Fatalf("stale compare restored over new edit: %s", body)
	}
	status, body = call(t, page, endpoint+"?share_id="+id, nil)
	if status != 200 || json.Unmarshal(body, &view) != nil {
		t.Fatal(string(body))
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := ctrl.UpdateSettings("test", "ws://127.0.0.1:9", port); err != nil {
		t.Fatal(err)
	}
	ctrl.Start()
	t.Cleanup(ctrl.Stop)
	status, body = call(t, page, endpoint+"/restore", map[string]string{"share_id": id, "expected_sum": view.CurrentSum})
	if status != 200 {
		t.Fatalf("restore: %d %s", status, body)
	}
	if ctrl.State().Running {
		t.Fatal("restore resumed sync before the user could inspect the file")
	}
	got, err := os.ReadFile(filename)
	if err != nil || string(got) != "package old\n" {
		t.Fatalf("restored bytes: %q, %v", got, err)
	}
	items, err = ctrl.Conflicts(id)
	if err != nil || len(items) != 2 {
		t.Fatalf("restore did not back up current file: %+v, %v", items, err)
	}
}
