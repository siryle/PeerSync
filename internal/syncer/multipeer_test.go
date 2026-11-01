package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func waitIndexes(t *testing.T, live int, engines ...*Engine) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var snapshots []string
	for time.Now().Before(deadline) {
		snapshots = nil
		complete := true
		for _, e := range engines {
			_, entries := e.Entries()
			byPath := map[string]proto.Entry{}
			count := 0
			for _, entry := range entries {
				if entry.Deleted {
					entry.Size, entry.ModTime, entry.Mode, entry.Sum = 0, 0, 0, ""
				}
				byPath[entry.Path] = entry
				if !entry.Deleted {
					count++
				}
			}
			body, err := json.Marshal(byPath)
			if err != nil {
				t.Fatal(err)
			}
			snapshots = append(snapshots, string(body))
			if count != live || snapshots[0] != string(body) {
				complete = false
			}
		}
		if complete {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("indexes did not converge: %v", snapshots)
}

func TestDirectoryReplacedByFileAcrossThreePeers(t *testing.T) {
	a, b, c := testEngine(t, "A"), testEngine(t, "B"), testEngine(t, "C")
	putFile(t, filepath.Join(a.Root, "docs", "old.txt"), "old")
	for _, e := range []*Engine{a, b, c} {
		scan(t, e)
	}
	connectEngines(t, a, b, nil)
	connectEngines(t, b, c, nil)
	waitIndexes(t, 2, a, b, c)
	if err := os.RemoveAll(filepath.Join(a.Root, "docs")); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(a.Root, "docs"), "replacement")
	scan(t, a)
	for _, e := range []*Engine{b, c} {
		waitBytes(t, filepath.Join(e.Root, "docs"), []byte("replacement"))
	}
	waitIndexes(t, 1, a, b, c)
	for _, e := range []*Engine{a, b, c} {
		scan(t, e)
	}
	waitIndexes(t, 1, a, b, c)
}

func TestDirectoryReplacementPreservesUnsyncedChild(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "docs", "local.txt"), "keep")
	scan(t, e)
	r, blocks := remoteFile(t, "docs", "replacement", vclock.Vector{"A": 1, "B": 1})
	task := e.Plan([]proto.Entry{r})[0]
	tmp := filepath.Join(e.Root, "part")
	putFile(t, tmp, "replacement")
	if err := e.CommitDownload(task, tmp, blocks); err == nil {
		t.Fatal("replaced a nonempty directory")
	}
	waitBytes(t, filepath.Join(e.Root, "docs", "local.txt"), []byte("keep"))
}

func TestScanDetectsChangeWithSameSizeAndTime(t *testing.T) {
	a, b := testEngine(t, "A"), testEngine(t, "B")
	name := filepath.Join(a.Root, "a.txt")
	putFile(t, name, "old")
	scan(t, a)
	scan(t, b)
	connectEngines(t, a, b, nil)
	waitIndexes(t, 1, a, b)
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, name, "new")
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := a.scan(false); err != nil {
		t.Fatal(err)
	}
	if versionOf(t, a, "a.txt")["A"] != 1 {
		t.Fatal("test must exercise the metadata cache")
	}
	scan(t, a)
	waitBytes(t, filepath.Join(b.Root, "a.txt"), []byte("new"))
	waitIndexes(t, 1, a, b)
}

func TestMetadataChangesAcrossThreePeers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support Unix executable permissions")
	}
	a, b, c := testEngine(t, "A"), testEngine(t, "B"), testEngine(t, "C")
	putFile(t, filepath.Join(a.Root, "docs", "script.sh"), "echo hello")
	for _, e := range []*Engine{a, b, c} {
		scan(t, e)
	}
	connectEngines(t, a, b, nil)
	connectEngines(t, b, c, nil)
	waitIndexes(t, 2, a, b, c)
	file := filepath.Join(c.Root, "docs", "script.sh")
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(c.Root, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Use the cached scan to cover chmod without a content rehash.
	if err := c.scan(false); err != nil {
		t.Fatal(err)
	}
	waitIndexes(t, 2, a, b, c)
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	scan(t, c)
	waitIndexes(t, 2, a, b, c)
	for _, e := range []*Engine{a, b, c} {
		info, err := os.Stat(filepath.Join(e.Root, "docs", "script.sh"))
		if err != nil || info.Mode().Perm() != 0o755 || !info.ModTime().Equal(stamp) {
			t.Fatalf("file metadata on %s: %v, %v", e.Self, info, err)
		}
		dir, err := os.Stat(filepath.Join(e.Root, "docs"))
		if err != nil || dir.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode on %s: %v, %v", e.Self, dir, err)
		}
		scan(t, e)
	}
	waitIndexes(t, 2, a, b, c)
}

func TestReplicatedConflictHasOneBackup(t *testing.T) {
	a, b, c := testEngine(t, "A"), testEngine(t, "B"), testEngine(t, "C")
	winner, loser := "alpha", "bravo"
	ar, _ := remoteFile(t, "a.txt", winner, nil)
	br, _ := remoteFile(t, "a.txt", loser, nil)
	as, _ := proto.DecodeSum(ar.Sum)
	bs, _ := proto.DecodeSum(br.Sum)
	if bytes.Compare(as[:], bs[:]) < 0 {
		winner, loser = loser, winner
	}
	putFile(t, filepath.Join(a.Root, "a.txt"), loser)
	scan(t, a)
	scan(t, c)
	stop := connectEngines(t, a, c, nil)
	waitIndexes(t, 1, a, c)
	stop()
	putFile(t, filepath.Join(b.Root, "a.txt"), winner)
	scan(t, b)
	_, entries := b.Entries()
	// Resolve independently before exchanging backups, forcing both replicas
	// of the losing version to save the same conflict.
	for _, e := range []*Engine{a, c} {
		task := e.Plan(entries)[0]
		if task.Op != opConflict {
			t.Fatalf("expected conflict, got %s", task.Op)
		}
		_, blocks := remoteFile(t, "a.txt", winner, entries[0].Version)
		tmp := filepath.Join(e.Root, "part")
		putFile(t, tmp, winner)
		if err := e.CommitDownload(task, tmp, blocks); err != nil {
			t.Fatal(err)
		}
	}
	connectEngines(t, a, b, nil)
	connectEngines(t, b, c, nil)
	connectEngines(t, a, c, nil)
	waitIndexes(t, 1, a, b, c)
	var backupID string
	for _, e := range []*Engine{a, c} {
		items, err := ListConflicts(e.IndexPath)
		if err != nil || len(items) != 1 {
			t.Fatalf("backups: %v, %v", items, err)
		}
		if backupID != "" && backupID != items[0].ID {
			t.Fatal("same losing version has different backup IDs")
		}
		backupID = items[0].ID
		waitBytes(t, filepath.Join(ConflictDir(e.IndexPath), backupID+".data"), []byte(loser))
		scan(t, e)
	}
	items, err := ListConflicts(b.IndexPath)
	if err != nil || len(items) != 0 {
		t.Fatalf("backups propagated to another device: %v, %v", items, err)
	}
	waitIndexes(t, 1, a, b, c)
}

func TestConcurrentMetadataConverges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support Unix executable permissions")
	}
	a, b, c := testEngine(t, "A"), testEngine(t, "B"), testEngine(t, "C")
	stamp := time.Unix(1700000000, 0)
	for i, e := range []*Engine{a, b, c} {
		file := filepath.Join(e.Root, "docs", "a.txt")
		putFile(t, file, "same content")
		mode := []os.FileMode{0o755, 0o644, 0o700}[i]
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
		mtime := stamp
		if e == b {
			mtime = stamp.Add(time.Second)
		}
		if err := os.Chtimes(file, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(e.Root, "docs"), []os.FileMode{0o700, 0o750, 0o755}[i]); err != nil {
			t.Fatal(err)
		}
		scan(t, e)
	}
	connectEngines(t, a, b, nil)
	connectEngines(t, b, c, nil)
	connectEngines(t, a, c, nil)
	waitIndexes(t, 2, a, b, c)
	for _, e := range []*Engine{a, b, c} {
		info, err := os.Stat(filepath.Join(e.Root, "docs", "a.txt"))
		if err != nil || info.Mode().Perm() != 0o644 || !info.ModTime().Equal(stamp.Add(time.Second)) {
			t.Fatalf("file metadata on %s: %v, %v", e.Self, info, err)
		}
		dir, err := os.Stat(filepath.Join(e.Root, "docs"))
		if err != nil || dir.Mode().Perm() != 0o755 {
			t.Fatalf("directory metadata on %s: %v, %v", e.Self, dir, err)
		}
		scan(t, e)
	}
	waitIndexes(t, 2, a, b, c)
}

func TestMergePreservesUnscannedMetadataEdit(t *testing.T) {
	e := testEngine(t, "A")
	file := filepath.Join(e.Root, "a.txt")
	putFile(t, file, "same")
	scan(t, e)
	r, _ := remoteFile(t, "a.txt", "same", vclock.Vector{"A": 1, "B": 1})
	task := e.Plan([]proto.Entry{r})[0]
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := e.ApplyMerge(task); !errors.Is(err, errStale) {
		t.Fatalf("unscanned metadata edit was overwritten: %v", err)
	}
	info, err := os.Stat(file)
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("local time lost: %v, %v", info, err)
	}
}
