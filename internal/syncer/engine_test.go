package syncer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func TestScanEditAndDelete(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(t.TempDir(), "index.json")
	e, err := Open(root, index, "device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
	_, entries := e.Entries()
	if !hasPath(entries, "a.txt") || !hasPath(entries, "sub/b.txt") || !hasPath(entries, "sub") {
		t.Fatalf("entries: %v", entryPaths(entries))
	}
	if versionOf(t, e, "a.txt")["device-a"] != 1 {
		t.Fatal("new file should start at 1")
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
	if versionOf(t, e, "a.txt")["device-a"] != 2 {
		t.Fatal("edit should bump the clock")
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	deleted := e.idx.Files["a.txt"].Deleted
	clock := e.idx.Files["a.txt"].Version["device-a"]
	e.mu.Unlock()
	if !deleted || clock != 3 {
		t.Fatalf("tombstone deleted=%v clock=%d", deleted, clock)
	}
}

func TestScanUnreadableDirDoesNotTombstone(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(t.TempDir(), "index.json")
	e, err := Open(root, index, "device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if f, err := os.Open(sub); err == nil {
		f.Close()
		t.Skip("process can still read a mode 000 directory")
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := e.Scan(); err == nil {
		t.Fatal("scan should report the unreadable directory")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if a := e.idx.Files["a.txt"]; a == nil || !a.Deleted {
		t.Fatalf("removed file should be tombstoned: %+v", a)
	}
	if b := e.idx.Files["sub/b.txt"]; b == nil || b.Deleted {
		t.Fatalf("file in an unreadable directory was tombstoned: %+v", b)
	}
	if d := e.idx.Files["sub"]; d == nil || d.Deleted || !d.IsDir {
		t.Fatalf("unreadable directory was tombstoned: %+v", d)
	}
}

func TestScanUnreadableRootDoesNotWipe(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(t.TempDir(), "index.json")
	e, err := Open(root, index, "device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	if f, err := os.Open(root); err == nil {
		f.Close()
		t.Skip("process can still read a mode 000 directory")
	}
	if err := e.Scan(); err == nil {
		t.Fatal("scan should fail when the root cannot be read")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if a := e.idx.Files["a.txt"]; a == nil || a.Deleted {
		t.Fatalf("root read failure wiped the index: %+v", a)
	}
}

func TestFirstScanFailureDoesNotAnnounce(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(t.TempDir(), "index.json")
	e, err := Open(root, index, "device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	if f, err := os.Open(root); err == nil {
		f.Close()
		t.Skip("process can still read a mode 000 directory")
	}
	if err := e.Scan(); err == nil {
		t.Fatal("scan should fail when the root cannot be read")
	}
	select {
	case <-e.Ready():
		t.Fatal("index must not be announced before a readable scan")
	default:
	}
}

func TestBatchWritesIndexOnce(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(t.TempDir(), "index.json")
	e, err := Open(root, index, "device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
	body := []byte("abc")
	sum, blocks, size, err := HashReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, "part-body")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	task := Task{
		Path:    "a.txt",
		Version: vclock.Vector{"peer": 1},
		Remote: proto.Entry{
			Path: "a.txt", Size: size, ModTime: time.Now().UnixNano(), Mode: 0o644,
			Sum: proto.EncodeSum(sum, true), Version: vclock.Vector{"peer": 1},
		},
	}
	e.BeginBatch()
	if err := e.CommitDownload(task, tmp, blocks); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(index); !os.IsNotExist(err) {
		t.Fatalf("batched commit wrote the index early: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	e.EndBatch()
	saved, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(saved, []byte("a.txt")) {
		t.Fatalf("index missing a.txt: %s", saved)
	}

	body = []byte("second")
	sum, blocks, size, err = HashReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	tmp = filepath.Join(root, "part-two")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	task.Path = "b.txt"
	task.Remote.Path = "b.txt"
	task.Remote.Size = size
	task.Remote.Sum = proto.EncodeSum(sum, true)
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CommitDownload(task, tmp, blocks); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) || !bytes.Contains(after, []byte("b.txt")) {
		t.Fatal("commit outside a batch should write the index immediately")
	}
}

func hasPath(entries []proto.Entry, path string) bool {
	for _, e := range entries {
		if e.Path == path && !e.Deleted {
			return true
		}
	}
	return false
}

func entryPaths(entries []proto.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

func versionOf(t *testing.T, e *Engine, path string) map[string]uint64 {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.idx.Files[path]
	if f == nil {
		t.Fatalf("missing %s", path)
	}
	return f.Version
}
