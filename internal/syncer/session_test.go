package syncer

import (
	"bytes"
	"context"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/frame"
)

func TestStreamAndDeltaTransfer(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	a, err := Open(dirA, filepath.Join(t.TempDir(), "a.json"), "device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := Open(dirB, filepath.Join(t.TempDir(), "b.json"), "device-b", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	// Distinct bytes per block, so a one-byte insert does not collide with an
	// aligned block and the transfer has to use the rolling checksum.
	shift := patterned(2 * protoBlock())
	patch := patterned(3 * protoBlock())
	if err := os.MkdirAll(filepath.Join(dirA, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dirA, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("shift.bin", shift)
	write("patch.bin", patch)
	write("nested/note.txt", []byte("hello"))
	if err := os.WriteFile(filepath.Join(dirA, "empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		write(filepath.Join("nested", "f"+string(rune('a'+i%26))+string(rune('0'+i/26))), []byte("x"))
	}
	if err := a.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := b.Scan(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var fromA atomic.Int64
	errc := make(chan error, 2)
	go func() { errc <- Serve(ctx, a, frame.New(countConn{left, &fromA}), nil, nil) }()
	go func() { errc <- Serve(ctx, b, frame.New(right), nil, nil) }()

	waitBytes(t, filepath.Join(dirB, "shift.bin"), shift)
	waitBytes(t, filepath.Join(dirB, "patch.bin"), patch)
	waitBytes(t, filepath.Join(dirB, "nested", "note.txt"), []byte("hello"))
	waitBytes(t, filepath.Join(dirB, "empty"), nil)

	base := fromA.Load()
	patch[protoBlock()] ^= 0xff
	write("patch.bin", patch)
	if err := a.Scan(); err != nil {
		t.Fatal(err)
	}
	waitBytes(t, filepath.Join(dirB, "patch.bin"), patch)
	moved := fromA.Load() - base
	if moved < 100<<10 || moved > 180<<10 {
		t.Fatalf("one changed block moved %d bytes; a full file would be about %d", moved, len(patch))
	}

	base = fromA.Load()
	shifted := append([]byte{0xab}, shift...)
	write("shift.bin", shifted)
	if err := a.Scan(); err != nil {
		t.Fatal(err)
	}
	waitBytes(t, filepath.Join(dirB, "shift.bin"), shifted)
	moved = fromA.Load() - base
	if moved > 48<<10 {
		t.Fatalf("prepended byte moved %d bytes; rsync delta should stay small", moved)
	}

	if err := os.Remove(filepath.Join(dirA, "nested", "note.txt")); err != nil {
		t.Fatal(err)
	}
	if err := a.Scan(); err != nil {
		t.Fatal(err)
	}
	waitGoneFile(t, filepath.Join(dirB, "nested", "note.txt"))
}

func protoBlock() int { return 128 << 10 }

func patterned(n int) []byte {
	rng := rand.New(rand.NewSource(int64(n)))
	buf := make([]byte, n)
	rng.Read(buf)
	return buf
}

type countConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

func waitBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(path)
		if err == nil && bytes.Equal(got, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %s (%d bytes, have %d)", path, len(want), len(got))
}

func waitGoneFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to be deleted", path)
}
