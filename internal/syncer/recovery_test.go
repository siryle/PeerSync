package syncer

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/frame"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func connectEngines(t *testing.T, a, b *Engine, note func(Note)) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	left, right := net.Pipe()
	stopped := make(chan error, 2)
	go func() { stopped <- Serve(ctx, a, frame.New(left), nil, nil, b.Self) }()
	go func() { stopped <- Serve(ctx, b, frame.New(right), nil, note, a.Self) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = left.Close()
			_ = right.Close()
			for i := 0; i < 2; i++ {
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Error("session did not stop")
				}
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestConcurrentEditsConverge(t *testing.T) {
	a, b := testEngine(t, "A"), testEngine(t, "B")
	putFile(t, filepath.Join(a.Root, "a.txt"), "alpha")
	putFile(t, filepath.Join(b.Root, "a.txt"), "bravo")
	scan(t, a)
	scan(t, b)
	ar, _ := remoteFile(t, "a.txt", "alpha", vclock.Vector{"A": 1})
	br, _ := remoteFile(t, "a.txt", "bravo", vclock.Vector{"B": 1})
	as, _ := proto.DecodeSum(ar.Sum)
	bs, _ := proto.DecodeSum(br.Sum)
	winner, loser := "alpha", "bravo"
	if bytes.Compare(as[:], bs[:]) < 0 {
		winner, loser = loser, winner
	}
	connectEngines(t, a, b, nil)
	for _, e := range []*Engine{a, b} {
		waitBytes(t, filepath.Join(e.Root, "a.txt"), []byte(winner))
	}
	waitIndexes(t, 1, a, b)
	backups := 0
	for _, e := range []*Engine{a, b} {
		items, err := ListConflicts(e.IndexPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			backups++
			waitBytes(t, filepath.Join(ConflictDir(e.IndexPath), item.ID+".data"), []byte(loser))
		}
		files, err := os.ReadDir(e.Root)
		if err != nil {
			t.Fatalf("backup leaked into share: %v, %v", files, err)
		}
		for _, f := range files {
			if f.Name() != "a.txt" && f.Name() != tmpDirName {
				t.Fatalf("backup leaked into share: %s", f.Name())
			}
		}
	}
	if backups != 1 {
		t.Fatalf("got %d external backups, want 1", backups)
	}
}

func TestDownloadRetriesAfterTemporaryFailure(t *testing.T) {
	a, b := testEngine(t, "A"), testEngine(t, "B")
	winner, loser := "alpha", "bravo"
	ar, _ := remoteFile(t, "a.txt", winner, nil)
	br, _ := remoteFile(t, "a.txt", loser, nil)
	as, _ := proto.DecodeSum(ar.Sum)
	bs, _ := proto.DecodeSum(br.Sum)
	if bytes.Compare(as[:], bs[:]) < 0 {
		winner, loser = loser, winner
	}
	putFile(t, filepath.Join(a.Root, "a.txt"), winner)
	putFile(t, filepath.Join(b.Root, "a.txt"), loser)
	putFile(t, filepath.Join(b.Root, tmpDirName), "temporary directory is blocked")
	scan(t, a)
	scan(t, b)
	failed := make(chan struct{}, 1)
	connectEngines(t, a, b, func(note Note) {
		if note.Op == "fail" {
			select {
			case failed <- struct{}{}:
			default:
			}
		}
	})
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("first download never attempted")
	}
	items, err := ListConflicts(b.IndexPath)
	if err != nil || len(items) != 0 {
		t.Fatalf("failed download created a backup: %v, %v", items, err)
	}
	if err := os.Remove(filepath.Join(b.Root, tmpDirName)); err != nil {
		t.Fatal(err)
	}
	scan(t, b)
	waitBytes(t, filepath.Join(b.Root, "a.txt"), []byte(winner))
}
