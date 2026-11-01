package syncer

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/frame"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func connectEngines(t *testing.T, a, b *Engine, note func(Note)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	left, right := net.Pipe()
	stopped := make(chan error, 2)
	go func() { stopped <- Serve(ctx, a, frame.New(left), nil, nil) }()
	go func() { stopped <- Serve(ctx, b, frame.New(right), nil, note) }()
	t.Cleanup(func() {
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
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		complete := true
		for _, e := range []*Engine{a, b} {
			_, entries := e.Entries()
			if len(entries) != 2 {
				complete = false
				continue
			}
			for _, entry := range entries {
				if entry.Path == "a.txt" {
					if vclock.Compare(entry.Version, vclock.Vector{"A": 1, "B": 1}) != 0 {
						complete = false
					}
				} else {
					if !strings.Contains(entry.Path, ".sync-conflict-") {
						t.Fatalf("unexpected path %q", entry.Path)
					}
					body, err := os.ReadFile(filepath.Join(e.Root, entry.Path))
					if err != nil || string(body) != loser {
						complete = false
					}
				}
			}
		}
		if complete {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, ae := a.Entries()
	_, be := b.Entries()
	t.Fatalf("conflict did not converge: A=%+v B=%+v", ae, be)
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
	_, entries := b.Entries()
	for _, entry := range entries {
		if strings.Contains(entry.Path, ".sync-conflict-") {
			t.Fatal("failed download created a conflict backup")
		}
	}
	if err := os.Remove(filepath.Join(b.Root, tmpDirName)); err != nil {
		t.Fatal(err)
	}
	scan(t, b)
	waitBytes(t, filepath.Join(b.Root, "a.txt"), []byte(winner))
}
