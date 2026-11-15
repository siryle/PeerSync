package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/frame"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func TestCorruptIndexStopsShareWithoutChangingFiles(t *testing.T) {
	root, index := t.TempDir(), filepath.Join(t.TempDir(), "index.json")
	putFile(t, filepath.Join(root, "important.txt"), "new offline edit")
	putFile(t, index, `{"seq":5,"files":`)
	if e, err := Open(root, index, "A", nil); err == nil {
		e.Close()
		t.Fatal("corrupt index must stop sync instead of resetting version clocks")
	}
	waitBytes(t, filepath.Join(root, "important.txt"), []byte("new offline edit"))
	waitBytes(t, index, []byte(`{"seq":5,"files":`))
}

func TestRemoteDirectoryDeletionPreservesIgnoredFiles(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, ".peersyncignore"), "*.secret\n")
	putFile(t, filepath.Join(e.Root, "docs", "local.secret"), "must survive")
	scan(t, e)
	task := e.Plan([]proto.Entry{{Path: "docs", IsDir: true, Deleted: true, Version: vclock.Vector{"A": 1, "B": 1}}})[0]
	if err := e.ApplyDelete(task); err == nil {
		t.Fatal("nonempty directory must not be removed")
	}
	waitBytes(t, filepath.Join(e.Root, "docs", "local.secret"), []byte("must survive"))
	if e.idx.Files["docs"].Deleted {
		t.Fatal("failed deletion became a tombstone")
	}
}

func TestInternalSymlinkCannotRedirectSync(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, ".peersyncignore"), "secret/\n")
	putFile(t, filepath.Join(e.Root, "secret", "victim"), "private")
	if err := os.Symlink("secret", filepath.Join(e.Root, "alias")); err != nil {
		t.Skip(err)
	}
	scan(t, e)
	r, blocks := remoteFile(t, "alias/new.txt", "remote", vclock.Vector{"B": 1})
	putFile(t, filepath.Join(e.Root, "part"), "remote")
	if err := e.CommitDownload(e.Plan([]proto.Entry{r})[0], "part", blocks); err == nil {
		t.Fatal("write followed a directory symlink")
	}
	if _, err := e.openFile("alias/victim"); err == nil {
		t.Fatal("read followed a directory symlink")
	}
	task := e.Plan([]proto.Entry{{Path: "alias/sub", IsDir: true, Version: r.Version}})[0]
	if err := e.ApplyDir(task); err == nil {
		t.Fatal("mkdir followed a directory symlink")
	}
	if _, err := os.Stat(filepath.Join(e.Root, "secret", "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("unexpected file: %v", err)
	}
}

func TestUnreadableIgnoreRulesBlockScanningAndServing(t *testing.T) {
	e := testEngine(t, "A")
	rules := filepath.Join(e.Root, ".peersyncignore")
	putFile(t, rules, "secret.txt\n")
	putFile(t, filepath.Join(e.Root, "secret.txt"), "private")
	scan(t, e)
	if err := os.Chmod(rules, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(rules, 0o600)
	if _, err := os.ReadFile(rules); err == nil {
		t.Skip("process can read mode 000")
	}
	if err := e.Scan(); err == nil {
		t.Fatal("unreadable ignore rules reported a successful scan")
	}
	if _, _, _, err := e.Meta("secret.txt"); err == nil {
		t.Fatal("excluded file became available")
	}
	r, _ := remoteFile(t, "other.txt", "remote", vclock.Vector{"B": 1})
	if tasks := e.Plan([]proto.Entry{r}); len(tasks) != 0 {
		t.Fatal("unreadable ignore rules allowed incoming changes")
	}
	if err := os.Chmod(rules, 0o600); err != nil {
		t.Fatal(err)
	}
	scan(t, e)
	if _, _, _, err := e.Meta("secret.txt"); err == nil {
		t.Fatal("excluded file entered the index")
	}
}

func TestReadOnlyDirectoriesAcceptUpdatesAndKeepPermissions(t *testing.T) {
	e := testEngine(t, "A")
	scan(t, e)
	for _, name := range []string{"docs", "docs/sub"} {
		r := proto.Entry{Path: name, IsDir: true, Mode: 0o555, Version: vclock.Vector{"B": 1}}
		if err := e.ApplyDir(e.Plan([]proto.Entry{r})[0]); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		os.Chmod(filepath.Join(e.Root, "docs"), 0o755)
		os.Chmod(filepath.Join(e.Root, "docs", "sub"), 0o755)
	})
	for i, body := range []string{"first", "updated"} {
		r, blocks := remoteFile(t, "docs/sub/file.txt", body, vclock.Vector{"B": uint64(i + 1)})
		r.Mode = 0o444
		putFile(t, filepath.Join(e.Root, "part"), body)
		if err := e.CommitDownload(e.Plan([]proto.Entry{r})[0], "part", blocks); err != nil {
			t.Fatal(err)
		}
		waitBytes(t, filepath.Join(e.Root, r.Path), []byte(body))
	}
	task := e.Plan([]proto.Entry{{Path: "docs/sub/file.txt", Deleted: true, Version: vclock.Vector{"B": 3}}})[0]
	if err := e.ApplyDelete(task); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docs", "docs/sub"} {
		info, err := os.Stat(filepath.Join(e.Root, name))
		if err != nil || info.Mode().Perm()&0o200 != 0 {
			t.Fatalf("directory permissions were not restored: %v, %v", info, err)
		}
	}
}

func TestIncomingBackupRecoversUnpublishedData(t *testing.T) {
	e := testEngine(t, "A")
	e.caseFold = true
	putFile(t, filepath.Join(e.Root, "notes.txt"), "local")
	scan(t, e)
	r, blocks := remoteFile(t, "Notes.txt", "remote", vclock.Vector{"B": 1})
	task := e.Plan([]proto.Entry{r})[0]
	sum, _ := proto.DecodeSum(r.Sum)
	h := sha256.New()
	h.Write([]byte(r.Path + "\x00"))
	h.Write(sum[:])
	id := hex.EncodeToString(h.Sum(nil))
	putFile(t, filepath.Join(ConflictDir(e.IndexPath), id+".data"), "remote")
	putFile(t, filepath.Join(e.Root, "part"), "remote")
	if err := e.CommitDownload(task, "part", blocks); err != nil {
		t.Fatal(err)
	}
	items, err := ListConflicts(e.IndexPath)
	if err != nil || len(items) != 1 || items[0].ID != id {
		t.Fatalf("backup was not published: %+v, %v", items, err)
	}
	waitBytes(t, filepath.Join(e.Root, "notes.txt"), []byte("local"))
}

func TestConcurrentCommitsPreserveConflictBackup(t *testing.T) {
	for round := 0; round < 4; round++ {
		e := testEngine(t, "A")
		name := "large.bin"
		body := bytes.Repeat([]byte("x"), 16<<20)
		if err := os.WriteFile(filepath.Join(e.Root, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
		scan(t, e)
		oldSum := e.idx.Files[name].Sum
		r, blocks := remoteFile(t, name, "remote", vclock.Vector{"B": 1})
		tasks := e.Plan([]proto.Entry{r})
		if len(tasks) != 1 || tasks[0].Op != opConflict {
			t.Fatal("expected a conflicting winning remote version")
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, part := range []string{"part1", "part2"} {
			putFile(t, filepath.Join(e.Root, part), "remote")
			wg.Go(func() {
				<-start
				results <- e.CommitDownload(tasks[0], part, blocks)
			})
		}
		close(start)
		wg.Wait()
		a, b := <-results, <-results
		if a != nil && b != nil {
			t.Fatalf("neither download committed: %v; %v", a, b)
		}
		items, err := ListConflicts(e.IndexPath)
		if err != nil || len(items) != 1 {
			t.Fatalf("backup list: %+v, %v", items, err)
		}
		_, f, err := OpenConflict(e.IndexPath, items[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		sum, _, size, err := HashReader(f)
		f.Close()
		if err != nil || sum != oldSum || size != int64(len(body)) {
			t.Fatalf("local version lost: size=%d, err=%v", size, err)
		}
	}
}

func TestFailedIndexSaveRetriesOnUnchangedScan(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "a.txt"), "old")
	scan(t, e)
	e.BeginBatch()
	task := e.Plan([]proto.Entry{{Path: "a.txt", Deleted: true, Version: vclock.Vector{"A": 1, "B": 1}}})[0]
	if err := e.ApplyDelete(task); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.IndexPath+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	e.EndBatch()
	if !e.dirty {
		t.Fatal("failed save lost its pending flag")
	}
	if err := os.Remove(e.IndexPath + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := e.scan(false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved indexFile
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if e.dirty || saved.Files["a.txt"] == nil || !saved.Files["a.txt"].Deleted {
		t.Fatal("idle scan did not save the pending deletion")
	}
}

func TestIncompleteSourceCannotDeleteReceiverFiles(t *testing.T) {
	a := testEngine(t, "A", Options{Mode: config.SendOnly})
	b := testEngine(t, "B", Options{Mode: config.ReceiveOnly, SourceID: "A"})
	name := "important.txt"
	source := filepath.Join(a.Root, name)
	putFile(t, source, "source content")
	putFile(t, filepath.Join(b.Root, name), "receiver content")
	if err := os.Chmod(source, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(source, 0o600)
	if _, err := os.ReadFile(source); err == nil {
		t.Skip("process can read mode 000")
	}
	if err := a.Scan(); err == nil {
		t.Fatal("unreadable file did not fail the scan")
	}
	select {
	case <-a.Ready():
		t.Fatal("incomplete first scan became ready")
	default:
	}
	scan(t, b)
	connectEngines(t, a, b, nil)
	time.Sleep(150 * time.Millisecond)
	waitBytes(t, filepath.Join(b.Root, name), []byte("receiver content"))
	if err := os.Chmod(source, 0o600); err != nil {
		t.Fatal(err)
	}
	scan(t, a)
	waitBytes(t, filepath.Join(b.Root, name), []byte("source content"))
}

func TestIncompleteScanCannotBeClearedByUnrelatedFileEvent(t *testing.T) {
	e := testEngine(t, "A", Options{Mode: config.SendOnly})
	scan(t, e)
	blocked := filepath.Join(e.Root, "blocked.txt")
	putFile(t, blocked, "unreadable source")
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocked, 0o600)
	if _, err := os.ReadFile(blocked); err == nil {
		t.Skip("process can read mode 000")
	}
	if err := e.Scan(); err == nil {
		t.Fatal("expected an incomplete scan")
	}
	putFile(t, filepath.Join(e.Root, "other.txt"), "other")
	if err := e.scanContext(context.Background(), false, nil, "other.txt"); err == nil {
		t.Fatal("unrelated event hid the unreadable source file")
	}
	stream := &indexCounter{}
	s := &session{e: e, conn: frame.New(stream)}
	if err := s.sendIndex(); err != nil || stream.bytes != 0 {
		t.Fatalf("incomplete source index was sent: %d bytes, %v", stream.bytes, err)
	}
	if err := os.Chmod(blocked, 0o600); err != nil {
		t.Fatal(err)
	}
	scan(t, e)
	if err := s.sendIndex(); err != nil || stream.bytes == 0 {
		t.Fatalf("recovered source index was not sent: %v", err)
	}
}

type indexCounter struct {
	discardStream
	bytes int
}

func (s *indexCounter) Write(p []byte) (int, error) {
	s.bytes += len(p)
	return len(p), nil
}

func TestCaseProbeDoesNotFollowTemporaryDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	putFile(t, filepath.Join(root, "secret", "CaseProbe"), "keep")
	if err := os.Symlink("secret", filepath.Join(root, tmpDirName)); err != nil {
		t.Skip(err)
	}
	e, err := Open(root, filepath.Join(t.TempDir(), "index.json"), "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	waitBytes(t, filepath.Join(root, "secret", "CaseProbe"), []byte("keep"))
}

func TestCaseOnlyRenameConverges(t *testing.T) {
	a, b := testEngine(t, "A"), testEngine(t, "B")
	a.caseFold, b.caseFold = true, false
	putFile(t, filepath.Join(a.Root, "notes.txt"), "body")
	scan(t, a)
	scan(t, b)
	connectEngines(t, a, b, nil)
	waitIndexes(t, 1, a, b)
	if err := os.Rename(filepath.Join(a.Root, "notes.txt"), filepath.Join(a.Root, "Notes.txt")); err != nil {
		t.Fatal(err)
	}
	scan(t, a)
	waitIndexes(t, 1, a, b)
	for _, e := range []*Engine{a, b} {
		_, entries := e.Entries()
		if hasPath(entries, "notes.txt") || !hasPath(entries, "Notes.txt") {
			t.Fatalf("rename did not replace the old name: %+v", entries)
		}
		waitBytes(t, filepath.Join(e.Root, "Notes.txt"), []byte("body"))
	}
}
