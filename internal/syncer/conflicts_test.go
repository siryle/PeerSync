package syncer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netcraft/peersync/internal/vclock"
)

func saveTestConflict(t *testing.T, e *Engine, name string) Conflict {
	t.Helper()
	e.mu.Lock()
	err := e.stageConflictLocked(Task{Path: name})
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	items, err := ListConflicts(e.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range items {
		if c.Path == name {
			return c
		}
	}
	t.Fatal("backup missing")
	return Conflict{}
}

func TestConflictRestorePreservesCurrentVersionAndRejectsStaleView(t *testing.T) {
	e := testEngine(t, "A")
	name := "internal/main.go"
	filename := filepath.Join(e.Root, name)
	putFile(t, filename, "package old\n")
	scan(t, e)
	c := saveTestConflict(t, e, name)
	putFile(t, filename, "package current\n")
	scan(t, e)
	view, err := e.ViewConflict(c.ID)
	if err != nil || view.BackupText != "package old\n" || view.CurrentText != "package current\n" {
		t.Fatalf("view: %+v, %v", view, err)
	}
	putFile(t, filename, "package newer\n")
	scan(t, e)
	if err := e.RestoreConflict(c.ID, view.CurrentSum); err == nil {
		t.Fatal("stale comparison overwrote a newer edit")
	}
	waitBytes(t, filename, []byte("package newer\n"))
	view, err = e.ViewConflict(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreConflict(c.ID, view.CurrentSum); err != nil {
		t.Fatal(err)
	}
	waitBytes(t, filename, []byte("package old\n"))
	items := waitBackups(t, e, 2)
	found := false
	for _, item := range items {
		if item.ID != c.ID {
			waitBytes(t, filepath.Join(ConflictDir(e.IndexPath), item.ID+".data"), []byte("package newer\n"))
			found = true
		}
	}
	if !found {
		t.Fatal("restore did not preserve current version")
	}
}

func TestConflictBackupFailureDoesNotReplaceFile(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "a.go"), "local")
	scan(t, e)
	r, blocks := remoteFile(t, "a.go", "remote", vclock.Vector{"B": 1})
	task := Task{Op: opConflict, Path: r.Path, Had: true, Seen: versionOf(t, e, r.Path), Remote: r, Version: r.Version}
	putFile(t, ConflictDir(e.IndexPath), "blocked")
	tmp := filepath.Join(e.Root, "part")
	putFile(t, tmp, "remote")
	if err := e.CommitDownload(task, tmp, blocks); err == nil {
		t.Fatal("overwrote a conflict without a backup")
	}
	waitBytes(t, filepath.Join(e.Root, "a.go"), []byte("local"))
}

func TestConflictStoreRejectsPathsAndKeepsSameNamedFilesSeparate(t *testing.T) {
	e := testEngine(t, "A")
	for _, name := range []string{"one/a.go", "two/a.go"} {
		putFile(t, filepath.Join(e.Root, name), "same")
	}
	scan(t, e)
	a := saveTestConflict(t, e, "one/a.go")
	b := saveTestConflict(t, e, "two/a.go")
	if a.ID == b.ID {
		t.Fatal("different paths reused the same backup")
	}
	for _, id := range []string{"../index", "../" + a.ID, strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		if _, _, err := OpenConflict(e.IndexPath, id); err == nil {
			t.Fatalf("invalid ID accepted: %s", id)
		}
	}
	if err := CheckConflictLocation(e.Root, filepath.Join(e.Root, "index.json")); err == nil {
		t.Fatal("allowed backups inside the share")
	}
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(e.Root, linked); err == nil {
		if err := CheckConflictLocation(e.Root, filepath.Join(linked, "index.json")); err == nil {
			t.Fatal("allowed symlinked backups inside the share")
		}
	}
}

func TestConflictPreviewBinaryAndRestoreMissingFile(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "image.png"), "binary\x00data")
	scan(t, e)
	c := saveTestConflict(t, e, "image.png")
	if err := os.Remove(filepath.Join(e.Root, c.Path)); err != nil {
		t.Fatal(err)
	}
	scan(t, e)
	view, err := e.ViewConflict(c.ID)
	if err != nil || view.TextAvailable || view.CurrentExists || view.CurrentSum != "missing" {
		t.Fatalf("binary view: %+v, %v", view, err)
	}
	if err := e.RestoreConflict(c.ID, view.CurrentSum); err != nil {
		t.Fatal(err)
	}
	waitBytes(t, filepath.Join(e.Root, c.Path), []byte("binary\x00data"))
}

func TestCorruptBackupDoesNotOverwriteCurrentFile(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "a.go"), "old")
	scan(t, e)
	c := saveTestConflict(t, e, "a.go")
	putFile(t, filepath.Join(e.Root, "a.go"), "current")
	scan(t, e)
	view, err := e.ViewConflict(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(ConflictDir(e.IndexPath), c.ID+".data"), "tampered")
	if err := e.RestoreConflict(c.ID, view.CurrentSum); err == nil {
		t.Fatal("restored a corrupt backup")
	}
	waitBytes(t, filepath.Join(e.Root, c.Path), []byte("current"))
}

func TestConflictViewSurvivesTypeReplacement(t *testing.T) {
	e := testEngine(t, "A")
	putFile(t, filepath.Join(e.Root, "docs", "a.go"), "nested backup")
	scan(t, e)
	child := saveTestConflict(t, e, "docs/a.go")
	if err := os.RemoveAll(filepath.Join(e.Root, "docs")); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(e.Root, "docs"), "parent backup")
	scan(t, e)
	parent := saveTestConflict(t, e, "docs")
	view, err := e.ViewConflict(child.ID)
	if err != nil || view.CurrentExists || view.BackupText != "nested backup" {
		t.Fatalf("backup under a replaced parent: %+v, %v", view, err)
	}
	if err := os.Remove(filepath.Join(e.Root, "docs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.Root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	view, err = e.ViewConflict(parent.ID)
	if err != nil || !view.CurrentIsDir || view.BackupText != "parent backup" {
		t.Fatalf("backup replaced by a directory: %+v, %v", view, err)
	}
}
