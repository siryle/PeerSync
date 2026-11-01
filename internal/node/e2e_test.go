package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/discover"
	"github.com/netcraft/peersync/internal/identity"
	"github.com/netcraft/peersync/internal/invite"
	"github.com/netcraft/peersync/internal/syncer"
)

func TestDirectorySyncDoesNotUseServer(t *testing.T) {
	t.Run("bidirectional", func(t *testing.T) { testDirectorySync(t, false) })
	t.Run("one-way", func(t *testing.T) { testDirectorySync(t, true) })
}

func testDirectorySync(t *testing.T, oneWay bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &discover.Server{Listen: "127.0.0.1:0", Stun: "127.0.0.1:0"}
	go func() { _ = srv.Run(ctx) }()
	if err := srv.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	serverURL := "ws://" + srv.HTTPAddr()

	material, err := invite.NewShare()
	if err != nil {
		t.Fatal(err)
	}
	shareID := invite.IDHex(material.ShareID)
	key := invite.KeyString(material.ShareKey)

	homeA := t.TempDir()
	homeB := t.TempDir()
	dirA := t.TempDir()
	dirB := t.TempDir()
	idA, err := identity.LoadOrCreate(filepath.Join(homeA, config.IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	save := func(home, path, name string) {
		cfg := config.Default(name)
		cfg.Server = serverURL
		cfg.ListenPort = 0
		cfg.Shares = []config.Share{{
			Label: "docs", Path: path, ShareID: shareID, ShareKey: key,
		}}
		if oneWay {
			cfg.Shares[0].Mode = config.SendOnly
			if home == homeB {
				cfg.Shares[0].Mode = config.ReceiveOnly
				cfg.Shares[0].SourceID = idA.DeviceID()
			}
		}
		if err := config.Save(filepath.Join(home, config.FileName), cfg); err != nil {
			t.Fatal(err)
		}
	}
	save(homeA, dirA, "alpha")
	save(homeB, dirB, "beta")

	errc := make(chan error, 2)
	go func() {
		errc <- Run(ctx, Options{
			Home: homeA, Server: serverURL, Stun: srv.STUNAddr(),
			RandomListen: true, ScanEvery: 200 * time.Millisecond,
		})
	}()
	go func() {
		errc <- Run(ctx, Options{
			Home: homeB, Server: serverURL, Stun: srv.STUNAddr(),
			RandomListen: true, ScanEvery: 200 * time.Millisecond,
		})
	}()

	payload := bytesOf(300 << 10)
	if err := os.MkdirAll(filepath.Join(dirA, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "nested", "note.txt"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(dirB, "nested", "note.txt"), payload)

	edited := append(append([]byte{}, payload...), []byte("-edited")...)
	if err := os.WriteFile(filepath.Join(dirB, "nested", "note.txt"), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if oneWay {
		waitFile(t, filepath.Join(dirB, "nested", "note.txt"), payload)
		waitFile(t, filepath.Join(dirA, "nested", "note.txt"), payload)
		items, err := syncer.ListConflicts(config.IndexPath(homeB, shareID))
		if err != nil || len(items) != 1 {
			t.Fatalf("receiver backup: %+v, %v", items, err)
		}
		waitFile(t, filepath.Join(syncer.ConflictDir(config.IndexPath(homeB, shareID)), items[0].ID+".data"), edited)
	} else {
		waitFile(t, filepath.Join(dirA, "nested", "note.txt"), edited)
	}

	if err := os.Remove(filepath.Join(dirA, "nested", "note.txt")); err != nil {
		t.Fatal(err)
	}
	waitGone(t, filepath.Join(dirB, "nested", "note.txt"))

	if got := srv.SignalBytes.Load(); got > 256<<10 {
		t.Fatalf("discovery forwarded %d bytes; directory contents must not pass through the server", got)
	}

	cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-errc:
		case <-time.After(5 * time.Second):
			t.Fatal("node did not stop")
		}
	}
}

func bytesOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func waitFile(t *testing.T, path string, want []byte) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(path)
		if err == nil && string(got) == string(want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	got, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %s (%d bytes, have %d)", path, len(want), len(got))
}

func waitGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to be deleted", path)
}
