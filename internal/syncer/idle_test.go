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
	if n := len(w.WatchList()); n != want {
		t.Fatalf("watched %d directories, want %d", n, want)
	}
	if err := e.scanContext(context.Background(), false, w, "nested/a.txt"); err != nil {
		t.Fatal(err)
	}
	if n := len(w.WatchList()); n != want {
		t.Fatalf("scoped scan removed unrelated watches: %d, want %d", n, want)
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
	putFile(t, filepath.Join(e.Root, "nested", "note.txt"), "first")
	await()
	putFile(t, filepath.Join(e.Root, "nested", "note.txt"), "second")
	await()
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

func TestScopedScanPreservesOtherPathsAndHandlesSubtreeChanges(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "keep.txt"), "old")
	putFile(t, filepath.Join(e.Root, "changed", "file.txt"), "old")
	putFile(t, filepath.Join(e.Root, "changed", "sub", "child.txt"), "child")
	scan(t, e)
	putFile(t, filepath.Join(e.Root, "keep.txt"), "outside the event scope")
	putFile(t, filepath.Join(e.Root, "changed", "file.txt"), "updated")
	putFile(t, filepath.Join(e.Root, "changed", "new.txt"), "new")
	if err := os.RemoveAll(filepath.Join(e.Root, "changed", "sub")); err != nil {
		t.Fatal(err)
	}
	if err := e.scanContext(context.Background(), false, nil, "changed/file.txt", "changed", "changed"); err != nil {
		t.Fatal(err)
	}
	if got := versionOf(t, e, "keep.txt")["A"]; got != 1 {
		t.Fatalf("scanned a file outside the event scope: version=%d", got)
	}
	if got := versionOf(t, e, "changed/file.txt")["A"]; got != 2 {
		t.Fatalf("overlapping event paths bumped the version %d times", got-1)
	}
	if _, _, _, err := e.Meta("changed/new.txt"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"changed/sub", "changed/sub/child.txt"} {
		if !e.idx.Files[rel].Deleted {
			t.Fatalf("removed subtree entry %s was not tombstoned", rel)
		}
	}
	select {
	case <-e.kick:
		t.Fatal("overlapping paths triggered another complete scan")
	default:
	}
	if err := os.Rename(filepath.Join(e.Root, "changed"), filepath.Join(e.Root, "renamed")); err != nil {
		t.Fatal(err)
	}
	if err := e.scanContext(context.Background(), false, nil, "changed", "renamed"); err != nil {
		t.Fatal(err)
	}
	if !e.idx.Files["changed/file.txt"].Deleted || e.idx.Files["keep.txt"].Deleted {
		t.Fatal("rename deleted the wrong paths")
	}
	if _, _, _, err := e.Meta("renamed/file.txt"); err != nil {
		t.Fatal(err)
	}
	scan(t, e)
	if got := versionOf(t, e, "keep.txt")["A"]; got != 2 {
		t.Fatal("complete scan did not pick up the unrelated edit")
	}
}

func TestRunPollingIsNotDelayedByPendingEvents(t *testing.T) {
	e := testEngine(t, "A")
	scans := make(chan struct{}, 20)
	e.Log = slog.New(slog.NewTextHandler(scanLogWriter(func(p []byte) (int, error) {
		if strings.Contains(string(p), `msg="scan complete"`) {
			scans <- struct{}{}
		}
		return len(p), nil
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, 50*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-scans:
	case <-time.After(3 * time.Second):
		t.Fatal("initial scan did not finish")
	}
	e.Kick()
	select {
	case <-scans:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pending event postponed periodic scanning to the debounce deadline")
	}
}

func TestRunFileEventScansOnlyChangedPath(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "a.txt"), "old")
	putFile(t, filepath.Join(e.Root, "other.txt"), "unrelated")
	scans := make(chan string, 20)
	e.Log = slog.New(slog.NewTextHandler(scanLogWriter(func(p []byte) (int, error) {
		if strings.Contains(string(p), `msg="scan complete"`) {
			scans <- string(p)
		}
		return len(p), nil
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, time.Hour) }()
	t.Cleanup(func() { cancel(); <-done })
	await := func() string {
		t.Helper()
		select {
		case line := <-scans:
			return line
		case <-time.After(3 * time.Second):
			t.Fatal("scan did not finish")
			return ""
		}
	}
	await()
	putFile(t, filepath.Join(e.Root, "a.txt"), "changed")
	if line := await(); !strings.Contains(line, "scoped=true paths=1 ") {
		t.Fatalf("file event scanned unrelated paths: %s", line)
	}
}
