//go:build unix

package syncer

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestScanSkipsNamedPipe(t *testing.T) {
	e := testEngine(t, "A")
	filename := filepath.Join(e.Root, "pipe")
	if err := syscall.Mkfifo(filename, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Scan() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		f, err := os.OpenFile(filename, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = f.Close()
		}
		t.Fatal("scan blocked on a named pipe")
	}
	if _, err := e.openFile("pipe"); err == nil {
		t.Fatal("named pipe opened for transfer")
	}
	_, entries := e.Entries()
	if len(entries) != 0 {
		t.Fatalf("special file indexed: %+v", entries)
	}
}
