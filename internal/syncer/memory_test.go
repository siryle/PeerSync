package syncer

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/frame"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func memoryFixture(b *testing.B) *Engine {
	b.Helper()
	e, err := Open(b.TempDir(), filepath.Join(b.TempDir(), "index.json"), "PS"+strings.Repeat("A", 52), nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { e.Close() })
	for i := 0; i < 200000; i++ {
		e.idx.Files[fmt.Sprintf("projects/project-%04d/src/module-%04d/file-%06d.txt", i/1000, i/100, i)] = &File{Size: 1024, ModTime: 1700000000000000000, Mode: 0600, Sum: [32]byte{1, 2, 3, 4}, Blocks: [][32]byte{{1, 2, 3, 4}}, Version: vclock.Vector{e.Self: 1, "PS" + strings.Repeat("B", 52): 1}}
	}
	e.idx.Seq = 1
	e.scanComplete = true
	return e
}

func BenchmarkIndexMemory200k(b *testing.B) {
	e := memoryFixture(b)
	if err := e.saveSnapshot(e.idx); err != nil {
		b.Fatal(err)
	}
	b.Run("Save", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := e.saveSnapshot(e.idx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Load", func(b *testing.B) {
		b.ReportAllocs()
		before := retainedHeap()
		var loaded Engine
		loaded.IndexPath = e.IndexPath
		for b.Loop() {
			if err := loaded.load(); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(retainedHeap()-before), "retained-B")
		runtime.KeepAlive(&loaded)
	})
	b.Run("Send", func(b *testing.B) {
		s := &session{e: e, conn: frame.New(discardStream{})}
		b.ReportAllocs()
		for b.Loop() {
			if err := s.sendIndex(); err != nil {
				b.Fatal(err)
			}
		}
	})
	_, entries := e.Entries()
	const pageSize = 40
	total := (len(entries) + pageSize - 1) / pageSize
	pages := make([][]byte, total)
	for i := range pages {
		var err error
		pages[i], err = json.Marshal(proto.IndexPage{Seq: 1, Part: i, Total: total, Entries: entries[i*pageSize : min((i+1)*pageSize, len(entries))], Mode: string(config.SendReceive)})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.Run("Receive", func(b *testing.B) {
		b.ReportAllocs()
		before := retainedHeap()
		var update indexUpdate
		for b.Loop() {
			s := &session{indexReady: make(chan indexUpdate, 1)}
			for _, page := range pages {
				s.onIndex(page)
			}
			update = <-s.indexReady
			if len(update.entries) != len(entries) {
				b.Fatal("incomplete index")
			}
		}
		b.ReportMetric(float64(retainedHeap()-before), "retained-B")
		runtime.KeepAlive(update)
		runtime.KeepAlive(pages)
		runtime.KeepAlive(entries)
	})
}

// Two collections also discard encoding/json's pooled scratch buffers.
func retainedHeap() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

type discardStream struct{}

func (discardStream) Write(p []byte) (int, error)      { return len(p), nil }
func (discardStream) Read([]byte) (int, error)         { return 0, io.EOF }
func (discardStream) Close() error                     { return nil }
func (discardStream) SetReadDeadline(time.Time) error  { return nil }
func (discardStream) SetWriteDeadline(time.Time) error { return nil }
