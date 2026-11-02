package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/netcraft/peersync/internal/proto"
)

func TestHashReaderReusesBufferWithoutSharingResults(t *testing.T) {
	for _, size := range []int{0, 1, proto.BlockSize - 1, proto.BlockSize, proto.BlockSize + 1} {
		body := bytes.Repeat([]byte{'a'}, size)
		sum, blocks, n, err := HashReader(bytes.NewReader(body))
		if err != nil || n != int64(size) || len(blocks) != BlocksForSize(n) {
			t.Fatalf("hash %d bytes: size=%d blocks=%d err=%v", size, n, len(blocks), err)
		}
		if _, _, _, err := HashReader(bytes.NewReader(bytes.Repeat([]byte{'b'}, size))); err != nil {
			t.Fatal(err)
		}
		for i, block := range blocks {
			start := i * proto.BlockSize
			if block != sha256.Sum256(body[start:min(start+proto.BlockSize, size)]) {
				t.Fatal("later hash changed an earlier block")
			}
		}
		if sum != SumBlocks(blocks) {
			t.Fatal("incorrect content checksum")
		}
	}
}

func TestScanUpdatesWatchesWhenIgnoreRulesChange(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "nested", "a.txt"), "a")
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := e.scanContext(context.Background(), true, w); err != nil {
		t.Fatal(err)
	}
	want := 2
	if kqueueOS() {
		want = 1
	}
	if n := len(w.WatchList()); n != want {
		t.Fatalf("watched %d directories, want %d", n, want)
	}
	putFile(t, filepath.Join(e.Root, ".peersyncignore"), "nested/\n")
	if err := e.scanContext(context.Background(), false, w); err != nil {
		t.Fatal(err)
	}
	if paths := w.WatchList(); len(paths) != 1 || paths[0] != e.Root {
		t.Fatalf("ignored directory is still watched: %v", paths)
	}
}

type scanLogWriter func([]byte) (int, error)

func (w scanLogWriter) Write(p []byte) (int, error) { return w(p) }

func TestRunFiltersIgnoredFileEvents(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, ".peersyncignore"), "*.log\ncache/\n")
	putFile(t, filepath.Join(e.Root, "app.log"), "initial")
	scans := make(chan struct{}, 20)
	e.Log = slog.New(slog.NewTextHandler(scanLogWriter(func(p []byte) (int, error) {
		if strings.Contains(string(p), `msg="scan complete"`) {
			scans <- struct{}{}
		}
		return len(p), nil
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, time.Hour) }()
	t.Cleanup(func() { cancel(); <-done })
	await := func() {
		t.Helper()
		select {
		case <-scans:
		case <-time.After(3 * time.Second):
			t.Fatal("file event did not trigger a scan")
		}
	}
	await()
	putFile(t, filepath.Join(e.Root, "app.log"), "changed")
	select {
	case <-scans:
		t.Fatal("ignored file triggered a directory scan")
	case <-time.After(700 * time.Millisecond):
	}
	// A directory-only ignore must not hide a regular file of the same name.
	putFile(t, filepath.Join(e.Root, "cache"), "regular file")
	await()
	if _, _, _, err := e.Meta("cache"); err != nil {
		t.Fatalf("directory ignore hid a regular file: %v", err)
	}
	if !kqueueOS() {
		putFile(t, filepath.Join(e.Root, "nested", "note.txt"), "first")
		await()
		putFile(t, filepath.Join(e.Root, "nested", "note.txt"), "second")
		await()
	}
}

func TestRunVerificationDeadlineIsIndependentOfPolling(t *testing.T) {
	e := testEngine(t, "A", Options{VerifyEvery: 50 * time.Millisecond})
	name := filepath.Join(e.Root, "a.txt")
	putFile(t, name, "old")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, time.Hour) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-e.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("initial scan did not finish")
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, name, "new")
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	want, _, _, _ := HashReader(strings.NewReader("new"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sum, _, _, err := e.Meta("a.txt"); err == nil && sum == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("content verification waited for the one-hour polling interval")
}

func TestCancelledScanPreservesIndex(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "a.txt"), "keep")
	scan(t, e)
	if err := os.Remove(filepath.Join(e.Root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.scanContext(ctx, true, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan: %v", err)
	}
	if files, _ := e.Stats(); files != 1 {
		t.Fatal("cancelled scan tombstoned a file")
	}
	scan(t, e)
	if files, _ := e.Stats(); files != 0 {
		t.Fatal("cached file count was not invalidated after deletion")
	}
}
