package frame

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ca := New(deadlinePipe{a})
	cb := New(deadlinePipe{b})
	done := make(chan error, 1)
	go func() {
		typ, payload, err := cb.Read(time.Now().Add(2 * time.Second))
		if err != nil {
			done <- err
			return
		}
		if typ != 7 || !bytes.Equal(payload, []byte("hello")) {
			done <- io.ErrUnexpectedEOF
			return
		}
		done <- nil
	}()
	if err := ca.Write(7, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type deadlinePipe struct{ net.Conn }

func (d deadlinePipe) SetReadDeadline(t time.Time) error  { return d.Conn.SetReadDeadline(t) }
func (d deadlinePipe) SetWriteDeadline(t time.Time) error { return d.Conn.SetWriteDeadline(t) }
