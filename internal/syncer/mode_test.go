package syncer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/config"
)

func waitBackups(t *testing.T, e *Engine, count int) []Conflict {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items, err := ListConflicts(e.IndexPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == count {
			return items
		}
		time.Sleep(20 * time.Millisecond)
	}
	items, _ := ListConflicts(e.IndexPath)
	t.Fatalf("got %d backups, want %d: %+v", len(items), count, items)
	return nil
}

func TestReceiveOnlyMirrorsSourceAndBacksUpLocalEdits(t *testing.T) {
	a := testEngine(t, "A", Options{Mode: config.SendOnly})
	b := testEngine(t, "B", Options{Mode: config.ReceiveOnly, SourceID: "A"})
	putFile(t, filepath.Join(a.Root, "main.go"), "source")
	putFile(t, filepath.Join(b.Root, "main.go"), "local before joining")
	putFile(t, filepath.Join(b.Root, "extra", "test.go"), "local extra")
	scan(t, a)
	scan(t, b)
	connectEngines(t, a, b, nil)
	waitBytes(t, filepath.Join(b.Root, "main.go"), []byte("source"))
	items := waitBackups(t, b, 2)
	for _, item := range items {
		view, err := b.ViewConflict(item.ID)
		if err != nil || !view.TextAvailable {
			t.Fatalf("preview: %+v, %v", view, err)
		}
		switch item.Path {
		case "main.go":
			if view.BackupText != "local before joining" {
				t.Fatalf("wrong backup: %+v", view)
			}
		case "extra/test.go":
			if view.BackupText != "local extra" {
				t.Fatalf("wrong extra backup: %+v", view)
			}
		default:
			t.Fatal(item.Path)
		}
	}
	if _, err := os.Stat(filepath.Join(b.Root, "extra")); !os.IsNotExist(err) {
		t.Fatalf("receiver kept a directory absent from source: %v", err)
	}
	// A normal source update should not create a backup on the receiver.
	putFile(t, filepath.Join(a.Root, "main.go"), "source update")
	scan(t, a)
	waitBytes(t, filepath.Join(b.Root, "main.go"), []byte("source update"))
	waitBackups(t, b, 2)
	putFile(t, filepath.Join(b.Root, "main.go"), "receiver edit")
	scan(t, b)
	waitBytes(t, filepath.Join(b.Root, "main.go"), []byte("source update"))
	waitBackups(t, b, 3)
	waitBytes(t, filepath.Join(a.Root, "main.go"), []byte("source update"))
	if _, _, _, err := b.Meta("main.go"); err == nil {
		t.Fatal("receive-only folder served file data")
	}
	// A local deletion must be repaired even when the source clock is unchanged.
	if err := os.Remove(filepath.Join(b.Root, "main.go")); err != nil {
		t.Fatal(err)
	}
	scan(t, b)
	waitBytes(t, filepath.Join(b.Root, "main.go"), []byte("source update"))
	waitBackups(t, b, 3)
	// Deleting an unmodified received file needs no backup.
	if err := os.Remove(filepath.Join(a.Root, "main.go")); err != nil {
		t.Fatal(err)
	}
	scan(t, a)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(b.Root, "main.go")); os.IsNotExist(err) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(b.Root, "main.go")); !os.IsNotExist(err) {
		t.Fatalf("source deletion did not reach receiver: %v", err)
	}
	waitBackups(t, b, 3)
}

func TestReceiveOnlyIgnoresOtherSources(t *testing.T) {
	for _, mode := range []config.SyncMode{config.SendOnly, config.SendReceive} {
		t.Run(string(mode), func(t *testing.T) {
			a := testEngine(t, "A", Options{Mode: mode})
			source := "another-source"
			if mode == config.SendReceive {
				source = "A"
			}
			b := testEngine(t, "B", Options{Mode: config.ReceiveOnly, SourceID: source})
			putFile(t, filepath.Join(a.Root, "main.go"), "wrong source")
			putFile(t, filepath.Join(b.Root, "main.go"), "local")
			scan(t, a)
			scan(t, b)
			stop := connectEngines(t, a, b, nil)
			time.Sleep(200 * time.Millisecond)
			stop()
			waitBytes(t, filepath.Join(a.Root, "main.go"), []byte("wrong source"))
			waitBytes(t, filepath.Join(b.Root, "main.go"), []byte("local"))
			waitBackups(t, b, 0)
		})
	}
}

func TestSendOnlyRejectsIncomingEditsAndDeletes(t *testing.T) {
	a := testEngine(t, "A", Options{Mode: config.SendOnly})
	b := testEngine(t, "B")
	putFile(t, filepath.Join(a.Root, "a.txt"), "source")
	putFile(t, filepath.Join(a.Root, "keep.txt"), "keep")
	scan(t, a)
	scan(t, b)
	connectEngines(t, a, b, nil)
	waitIndexes(t, 2, a, b)
	putFile(t, filepath.Join(b.Root, "a.txt"), "remote edit")
	putFile(t, filepath.Join(b.Root, "extra.txt"), "remote extra")
	if err := os.Remove(filepath.Join(b.Root, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	scan(t, b)
	_, entries := b.Entries()
	if tasks := a.Plan(entries); len(tasks) != 0 {
		t.Fatalf("send-only planned incoming updates: %+v", tasks)
	}
	waitBytes(t, filepath.Join(a.Root, "a.txt"), []byte("source"))
	waitBytes(t, filepath.Join(a.Root, "keep.txt"), []byte("keep"))
	if _, err := os.Stat(filepath.Join(a.Root, "extra.txt")); !os.IsNotExist(err) {
		t.Fatalf("remote addition reached sender: %v", err)
	}
}

func TestReceiveOnlyBacksUpBeforeSourceDelete(t *testing.T) {
	a := testEngine(t, "A", Options{Mode: config.SendOnly})
	b := testEngine(t, "B", Options{Mode: config.ReceiveOnly, SourceID: "A"})
	putFile(t, filepath.Join(a.Root, "a.txt"), "source")
	scan(t, a)
	scan(t, b)
	stop := connectEngines(t, a, b, nil)
	waitIndexes(t, 1, a, b)
	stop()
	putFile(t, filepath.Join(b.Root, "a.txt"), "offline receiver edit")
	if err := os.Remove(filepath.Join(a.Root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	scan(t, a)
	scan(t, b)
	connectEngines(t, a, b, nil)
	waitIndexes(t, 0, a, b)
	items := waitBackups(t, b, 1)
	waitBytes(t, filepath.Join(ConflictDir(b.IndexPath), items[0].ID+".data"), []byte("offline receiver edit"))
}

func TestReceiveOnlyTypeReplacementPreservesLocalChildren(t *testing.T) {
	a := testEngine(t, "A", Options{Mode: config.SendOnly})
	b := testEngine(t, "B", Options{Mode: config.ReceiveOnly, SourceID: "A"})
	putFile(t, filepath.Join(a.Root, "docs"), "source file")
	putFile(t, filepath.Join(b.Root, "docs", "local.go"), "local child")
	scan(t, a)
	scan(t, b)
	connectEngines(t, a, b, nil)
	waitBytes(t, filepath.Join(b.Root, "docs"), []byte("source file"))
	items := waitBackups(t, b, 1)
	waitBytes(t, filepath.Join(ConflictDir(b.IndexPath), items[0].ID+".data"), []byte("local child"))
}

func TestReceiveOnlyDoesNotArchiveIdenticalContent(t *testing.T) {
	a := testEngine(t, "A", Options{Mode: config.SendOnly})
	b := testEngine(t, "B", Options{Mode: config.ReceiveOnly, SourceID: "A"})
	for _, e := range []*Engine{a, b} {
		putFile(t, filepath.Join(e.Root, "a.go"), "identical content")
	}
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(filepath.Join(b.Root, "a.go"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	scan(t, a)
	scan(t, b)
	connectEngines(t, a, b, nil)
	waitIndexes(t, 1, a, b)
	waitBackups(t, b, 0)
}
