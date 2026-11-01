package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestFolderBrowser(t *testing.T) {
	ctrl, _ := testCtrl(t)
	page := testPage(t, ctrl)
	root := t.TempDir()
	child := filepath.Join(root, "资料 & '备份'")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PEERSYNC_PATH", root)
	status, body := call(t, page, "/api/pick", map[string]string{})
	var picked struct {
		Browse bool `json:"browse"`
	}
	if err := json.Unmarshal(body, &picked); err != nil || status != 200 || !picked.Browse {
		t.Fatalf("headless picker: %d %s (%v)", status, body, err)
	}
	status, body = call(t, page, "/api/folders", nil)
	var listing folderList
	if err := json.Unmarshal(body, &listing); err != nil || status != 200 {
		t.Fatalf("listing: %d %s (%v)", status, body, err)
	}
	if listing.Path != root || listing.Parent != filepath.Dir(root) || len(listing.Folders) != 1 || listing.Folders[0].Path != child {
		t.Fatalf("wrong listing: %+v", listing)
	}
	status, body = call(t, page, "/api/folders?path="+url.QueryEscape(child), nil)
	if err := json.Unmarshal(body, &listing); err != nil || status != 200 {
		t.Fatalf("child listing: %d %s (%v)", status, body, err)
	}
	if listing.Path != child || listing.Parent != root || listing.Folders == nil || len(listing.Folders) != 0 {
		t.Fatalf("wrong empty child listing: %+v", listing)
	}
	missing := filepath.Join(root, "missing")
	for _, path := range []string{missing, file} {
		status, body = call(t, page, "/api/folders?path="+url.QueryEscape(path), nil)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid path %q: %d %s", path, status, body)
		}
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("browsing must not create folders: %v", err)
	}
}

func TestFolderBrowserRequiresAuth(t *testing.T) {
	ctrl, _ := testCtrl(t)
	t.Setenv("PEERSYNC_PATH", t.TempDir())
	for _, path := range []string{"/api/pick", "/api/folders"} {
		method := http.MethodGet
		if path == "/api/pick" {
			method = http.MethodPost
		}
		for _, token := range []string{"", "wrong", "test-passphrase"} {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("X-PeerSync-Token", token)
			rec := httptest.NewRecorder()
			handler(ctrl, "test-passphrase").ServeHTTP(rec, req)
			want := http.StatusUnauthorized
			if token == "test-passphrase" {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("%s with token %q: %d", path, token, rec.Code)
			}
		}
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		handler(ctrl, "").ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s without local API header: %d", path, rec.Code)
		}
	}
}

func TestNativeFolderPicker(t *testing.T) {
	ctrl, _ := testCtrl(t)
	page := testPage(t, ctrl)
	for _, tc := range []struct {
		name string
		path string
		err  error
		code int
	}{
		{name: "selected", path: t.TempDir(), code: 200},
		{name: "cancelled", err: ErrCancelled, code: 200},
		{name: "failed", err: errors.New("picker failed"), code: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl.SetPicker(func() (string, error) { return tc.path, tc.err })
			status, body := call(t, page, "/api/pick", map[string]string{})
			var result map[string]string
			if err := json.Unmarshal(body, &result); err != nil || status != tc.code {
				t.Fatalf("native picker: %d %s (%v)", status, body, err)
			}
			if result["path"] != tc.path {
				t.Fatalf("native path changed: %s", body)
			}
		})
	}
}

func TestFolderBrowserSymlinks(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(child, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "broken")); err != nil {
		t.Fatal(err)
	}
	listing, err := listFolders(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Folders) != 2 || listing.Folders[0].Path != child || listing.Folders[1].Path != link {
		t.Fatalf("wrong symlink listing: %+v", listing)
	}
}
