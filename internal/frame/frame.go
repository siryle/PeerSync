// Package frame is the length-prefixed stream used after two peers have a
// direct connection. The discovery server never sees these frames.
package frame

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"
)

// MaxPayload is the largest single frame. A raw push packs several 128 KiB
// blocks into one frame, and index pages are much smaller.
const MaxPayload = 1 << 20

var (
	ErrTooLarge = errors.New("frame exceeds 1 MiB")
	ErrClosed   = errors.New("frame connection closed")
)

// Stream is a reliable byte pipe with deadlines, either a TLS TCP connection
// or one QUIC stream.
type Stream interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// Conn reads and writes frames. Writes are safe from multiple goroutines.
type Conn struct {
	s   Stream
	r   *bufio.Reader
	wmu sync.Mutex
}

// New wraps a stream. The caller must not use s after this.
func New(s Stream) *Conn {
	return &Conn{s: s, r: bufio.NewReaderSize(s, 256<<10)}
}

// Write sends one frame in a single write, so a block push is not split
// into a header syscall and a body syscall.
func (c *Conn) Write(typ byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrTooLarge
	}
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(payload)))
	buf[4] = typ
	copy(buf[5:], payload)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.s.SetWriteDeadline(time.Now().Add(60 * time.Second))
	_, err := c.s.Write(buf)
	return err
}

// Read returns the next frame. The deadline bounds how long we wait.
func (c *Conn) Read(deadline time.Time) (byte, []byte, error) {
	if err := c.s.SetReadDeadline(deadline); err != nil {
		return 0, nil, err
	}
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n > MaxPayload {
		return 0, nil, ErrTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[4], payload, nil
}

// Close closes the underlying stream.
func (c *Conn) Close() error {
	return c.s.Close()
}
