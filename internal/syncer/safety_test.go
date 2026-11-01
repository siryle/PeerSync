package syncer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func testEngine(t *testing.T, self string, options ...Options) *Engine {
	t.Helper()
	e, err := Open(t.TempDir(), filepath.Join(t.TempDir(), "index.json"), self, nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func putFile(t *testing.T, filename, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func remoteFile(t *testing.T, name, body string, ver vclock.Vector) (proto.Entry, [][32]byte) {
	t.Helper()
	sum, blocks, size, err := HashReader(bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	return proto.Entry{Path: name, Size: size, Sum: proto.EncodeSum(sum, true), Mode: 0o644, ModTime: time.Now().UnixNano(), Version: ver}, blocks
}

func scan(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitPreservesLocalChanges(t *testing.T) {
	for _, timing := range []string{"scanned", "during-download", "same-size-and-time", "unindexed"} {
		t.Run(timing, func(t *testing.T) {
			e := testEngine(t, "A")
			filename := filepath.Join(e.Root, "a.txt")
			if timing != "unindexed" {
				putFile(t, filename, "old")
				scan(t, e)
			}
			r, blocks := remoteFile(t, "a.txt", "remote", vclock.Vector{"A": 1, "B": 1})
			task := e.Plan([]proto.Entry{r})[0]
			var stamp time.Time
			if timing == "same-size-and-time" {
				info, err := os.Stat(filename)
				if err != nil {
					t.Fatal(err)
				}
				stamp = info.ModTime()
			}
			e.Begin("a.txt")
			defer e.End("a.txt")
			local := "new local edit"
			if timing == "same-size-and-time" {
				local = "new"
			}
			putFile(t, filename, local)
			if timing == "scanned" {
				e.End("a.txt")
				scan(t, e)
				e.Begin("a.txt")
			}
			if timing == "during-download" {
				scan(t, e)
			}
			if timing == "same-size-and-time" {
				if err := os.Chtimes(filename, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			tmp := filepath.Join(e.Root, "part-body")
			putFile(t, tmp, "remote")
			if err := e.CommitDownload(task, tmp, blocks); !errors.Is(err, errStale) {
				t.Fatalf("commit error = %v", err)
			}
			got, err := os.ReadFile(filename)
			if err != nil || string(got) != local {
				t.Fatalf("local edit lost: %q, %v", got, err)
			}
		})
	}
}

func TestDeletePreservesUnscannedEdit(t *testing.T) {
	e := testEngine(t, "A")
	filename := filepath.Join(e.Root, "a.txt")
	putFile(t, filename, "old")
	scan(t, e)
	task := e.Plan([]proto.Entry{{Path: "a.txt", Deleted: true, Version: vclock.Vector{"A": 1, "B": 1}}})[0]
	putFile(t, filename, "new local edit")
	if err := e.ApplyDelete(task); !errors.Is(err, errStale) {
		t.Fatalf("delete error = %v", err)
	}
	got, err := os.ReadFile(filename)
	if err != nil || string(got) != "new local edit" {
		t.Fatalf("edit deleted: %q, %v", got, err)
	}
	if got := versionOf(t, e, "a.txt")["A"]; got != 2 {
		t.Fatalf("unscanned edit not recorded: A=%d", got)
	}
}

func TestRootedOperationsRejectSymlinks(t *testing.T) {
	e := testEngine(t, "A")
	outside := t.TempDir()
	putFile(t, filepath.Join(outside, "victim.txt"), "outside")
	if err := os.Symlink(outside, filepath.Join(e.Root, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	scan(t, e)
	r, blocks := remoteFile(t, "linked/victim.txt", "remote", vclock.Vector{"B": 1})
	tmp := filepath.Join(e.Root, "part-body")
	putFile(t, tmp, "remote")
	task := e.Plan([]proto.Entry{r})[0]
	if err := e.CommitDownload(task, tmp, blocks); err == nil {
		t.Fatal("write escaped root")
	}
	if _, err := e.openFile(r.Path); err == nil {
		t.Fatal("read escaped root")
	}
	task = e.Plan([]proto.Entry{{Path: r.Path, Deleted: true, Version: r.Version}})[0]
	if err := e.ApplyDelete(task); err == nil {
		t.Fatal("delete escaped root")
	}
	task = e.Plan([]proto.Entry{{Path: "linked/newdir", IsDir: true, Version: r.Version}})[0]
	if err := e.ApplyDir(task); err == nil {
		t.Fatal("mkdir escaped root")
	}
	if err := os.Symlink(outside, filepath.Join(e.Root, tmpDirName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&session{e: e}).createTemp(); err == nil {
		t.Fatal("temporary file escaped root")
	}
	got, err := os.ReadFile(filepath.Join(outside, "victim.txt"))
	if err != nil || string(got) != "outside" {
		t.Fatalf("outside file changed: %q, %v", got, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected outside entries: %v, %v", entries, err)
	}
}

func TestDirectoryDeletionOrderAndErrors(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordered", true: "failed-parent"}[parentFirst], func(t *testing.T) {
			e := testEngine(t, "A")
			putFile(t, filepath.Join(e.Root, "dir", "child"), "local")
			scan(t, e)
			remote := []proto.Entry{
				{Path: "dir", Deleted: true, IsDir: true, Version: vclock.Vector{"A": 1, "B": 1}},
				{Path: "dir/child", Deleted: true, Version: vclock.Vector{"A": 1, "B": 1}},
			}
			tasks := e.Plan(remote)
			if tasks[0].Path != "dir/child" {
				t.Fatalf("parent scheduled first: %+v", tasks)
			}
			if parentFirst {
				if err := e.ApplyDelete(tasks[1]); err == nil {
					t.Fatal("nonempty directory deletion reported success")
				}
				_, entries := e.Entries()
				for _, entry := range entries {
					if entry.Path == "dir" && entry.Deleted {
						t.Fatal("failed deletion recorded")
					}
				}
			}
			for _, task := range tasks {
				if err := e.ApplyDelete(task); err != nil {
					t.Fatal(err)
				}
			}
			scan(t, e)
			if _, err := os.Stat(filepath.Join(e.Root, "dir")); !os.IsNotExist(err) {
				t.Fatalf("directory resurrected: %v", err)
			}
		})
	}
}

func TestIgnoreAppliesToIncomingChanges(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, ".peersyncignore"), "secret/\n")
	scan(t, e)
	r, _ := remoteFile(t, "secret/data.txt", "remote", vclock.Vector{"B": 1})
	entries := []proto.Entry{r, {Path: "secret", IsDir: true, Version: r.Version}, {Path: "secret", IsDir: true, Deleted: true, Version: r.Version}}
	if tasks := e.Plan(entries); len(tasks) != 0 {
		t.Fatalf("ignored incoming paths: %+v", tasks)
	}
	putFile(t, filepath.Join(e.Root, "a.txt"), "local")
	scan(t, e)
	r, blocks := remoteFile(t, "a.txt", "remote", vclock.Vector{"A": 1, "B": 1})
	task := e.Plan([]proto.Entry{r})[0]
	putFile(t, filepath.Join(e.Root, ".peersyncignore"), "a.txt\n")
	tmp := filepath.Join(e.Root, "part-body")
	putFile(t, tmp, "remote")
	if err := e.CommitDownload(task, tmp, blocks); !errors.Is(err, errIgnored) {
		t.Fatalf("new ignore rule bypassed: %v", err)
	}
	task.Remote.Deleted = true
	if err := e.ApplyDelete(task); !errors.Is(err, errIgnored) {
		t.Fatalf("ignored delete accepted: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(e.Root, "a.txt"))
	if err != nil || string(got) != "local" {
		t.Fatalf("ignored file changed: %q, %v", got, err)
	}
}

func TestConflictBackupIsReused(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "a.txt"), "local")
	scan(t, e)
	e.mu.Lock()
	defer e.mu.Unlock()
	task := Task{Path: "a.txt"}
	for i := 0; i < 2; i++ {
		if err := e.stageConflictLocked(task); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.idx.Files) != 1 {
		t.Fatalf("backup entered the sync index: %d entries", len(e.idx.Files))
	}
	items, err := ListConflicts(e.IndexPath)
	if err != nil || len(items) != 1 {
		t.Fatalf("repeated backup produced %v, %v", items, err)
	}
}
