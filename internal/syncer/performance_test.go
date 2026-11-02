package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/netcraft/peersync/internal/vclock"
)

func BenchmarkIdleScan10k(b *testing.B) {
	root := b.TempDir()
	data := make([]byte, 1024)
	for i := 0; i < 10000; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%05d", i)), data, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	e, err := Open(root, filepath.Join(b.TempDir(), "index.json"), "self", nil)
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	if err := e.Scan(); err != nil {
		b.Fatal(err)
	}
	for _, full := range []bool{false, true} {
		b.Run(fmt.Sprintf("full=%t", full), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := e.scan(full); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkIdleIndex200k(b *testing.B) {
	e, err := Open(b.TempDir(), filepath.Join(b.TempDir(), "index.json"), "self", nil)
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	for i := 0; i < 200000; i++ {
		e.idx.Files[fmt.Sprintf("dir-%04d/file-%06d", i/100, i)] = &File{
			Version: vclock.Vector{"self": 1, "peer": 1}, Mode: 0o600,
		}
	}
	_, remote := e.Entries()
	b.Run("Stats", func(b *testing.B) {
		e.Stats()
		b.ReportAllocs()
		for b.Loop() {
			e.Stats()
		}
	})
	b.Run("PlanUnchanged", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if tasks := e.Plan(remote); len(tasks) != 0 {
				b.Fatal("unchanged index produced work")
			}
		}
	})
}
