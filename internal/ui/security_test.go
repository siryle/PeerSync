package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalAPIRejectsForeignHostAndOrigin(t *testing.T) {
	c, _ := testCtrl(t)
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:8384", "", http.StatusOK},
		{"127.0.0.1:8384", "http://127.0.0.1:8384", http.StatusOK},
		{"localhost:8384", "http://localhost:8384", http.StatusOK},
		{"[::1]:8384", "http://[::1]:8384", http.StatusOK},
		{"attacker.example:8384", "http://attacker.example:8384", http.StatusForbidden},
		{"127.0.0.1:8384", "http://attacker.example:8384", http.StatusForbidden},
		{"127.0.0.1:8384", "http://127.0.0.1:9999", http.StatusForbidden},
		{"127.0.0.1:8384", "null", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/api/state", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("X-PeerSync", "1")
		w := httptest.NewRecorder()
		handler(c, "").ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("host=%q origin=%q: status %d, want %d", tc.host, tc.origin, w.Code, tc.want)
		}
	}
}

func TestCreateShareRejectsSameDirectoryThroughSymlink(t *testing.T) {
	ctrl, _ := testCtrl(t)
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip(err)
	}
	if _, _, _, _, err := ctrl.CreateShare(root, "one"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := ctrl.CreateShare(alias, "two"); err == nil {
		t.Fatal("aliased shared root was accepted")
	}
	if got := len(ctrl.State().Shares); got != 1 {
		t.Fatalf("failed creation changed shares: %d", got)
	}
}
