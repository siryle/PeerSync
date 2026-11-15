package syncer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func TestCaseInsensitiveScanRetiresMissingSpelling(t *testing.T) {
	e := testEngine(t, "A")
	e.caseFold = true
	putFile(t, filepath.Join(e.Root, "notes.txt"), "local")
	scan(t, e)

	e.mu.Lock()
	live := e.idx.Files["notes.txt"]
	copied := *live
	e.idx.Files["Notes.txt"] = &copied
	e.idx.Seq++
	e.mu.Unlock()

	scan(t, e)

	_, entries := e.Entries()
	if hasPath(entries, "Notes.txt") || !hasPath(entries, "notes.txt") {
		t.Fatalf("scan retained a spelling absent from the directory: %v", entries)
	}
}

func TestCaseInsensitiveIncomingSavedAsConflict(t *testing.T) {
	e := testEngine(t, "A")
	e.caseFold = true
	filename := filepath.Join(e.Root, "notes.txt")
	putFile(t, filename, "local")
	scan(t, e)

	r, blocks := remoteFile(t, "Notes.txt", "remote", vclock.Vector{"B": 1})
	tasks := e.Plan([]proto.Entry{r})
	if len(tasks) != 1 || tasks[0].Op != opPull {
		t.Fatalf("tasks = %+v", tasks)
	}
	tmp := filepath.Join(e.Root, "part-body")
	putFile(t, tmp, "remote")
	if err := e.CommitDownload(tasks[0], tmp, blocks); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filename)
	if err != nil || string(got) != "local" {
		t.Fatalf("local file overwritten: %q, %v", got, err)
	}
	conflicts, err := ListConflicts(e.IndexPath)
	if err != nil || len(conflicts) != 1 || conflicts[0].Path != "Notes.txt" {
		t.Fatalf("conflicts = %+v, err = %v", conflicts, err)
	}
}

func TestCaseInsensitiveDeleteKeepsAlias(t *testing.T) {
	e := testEngine(t, "A")
	e.caseFold = true
	putFile(t, filepath.Join(e.Root, "notes.txt"), "local")
	scan(t, e)
	tasks := e.Plan([]proto.Entry{
		{Path: "Notes.txt", Deleted: true, Version: vclock.Vector{"A": 1, "B": 1}},
	})
	if len(tasks) != 0 {
		t.Fatalf("delete of case alias scheduled: %+v", tasks)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "notes.txt")); err != nil {
		t.Fatalf("local alias deleted: %v", err)
	}
}
