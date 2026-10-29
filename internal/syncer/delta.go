package syncer

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io"

	"github.com/netcraft/peersync/internal/proto"
)

// deltaChunk is the largest literal we emit before flushing. It stays under
// the 1 MiB frame limit together with the blob header.
const deltaChunk = 7 * proto.BlockSize

// DeltaOp is one step of an rsync patch. A literal and a copy are never set
// on the same op. Count is the number of consecutive basis blocks.
type DeltaOp struct {
	Lit   []byte
	Start int
	Count int
}

// MatchDelta scans r, which holds the new file, and emits a patch that
// rebuilds it from the receiver's basis blocks. sigs[i] is basis block i,
// and every signature block is proto.BlockSize bytes.
func MatchDelta(r io.ReaderAt, size int64, sigs []proto.BasisSig, emit func(DeltaOp) error) error {
	if size < 0 {
		return errors.New("bad size")
	}
	if len(sigs) == 0 || size < int64(proto.BlockSize) {
		return emitLiteralRange(r, 0, size, emit)
	}
	table := make(map[uint32][]int, len(sigs))
	for i, sg := range sigs {
		table[sg.Weak] = append(table[sg.Weak], i)
	}
	src := &byteSrc{r: r, size: size, buf: make([]byte, 0, proto.BlockSize+1<<20)}
	window := proto.BlockSize
	var lit []byte
	copyStart, copyRun := 0, 0

	flushLit := func() error {
		if len(lit) == 0 {
			return nil
		}
		op := DeltaOp{Lit: lit}
		lit = nil
		return emit(op)
	}
	flushCopy := func() error {
		if copyRun == 0 {
			return nil
		}
		err := emit(DeltaOp{Start: copyStart, Count: copyRun})
		copyRun = 0
		return err
	}
	addCopy := func(index int) error {
		if err := flushLit(); err != nil {
			return err
		}
		if copyRun > 0 && index == copyStart+copyRun {
			copyRun++
			return nil
		}
		if err := flushCopy(); err != nil {
			return err
		}
		copyStart = index
		copyRun = 1
		return nil
	}
	addLit := func(p []byte) error {
		if len(p) == 0 {
			return nil
		}
		if err := flushCopy(); err != nil {
			return err
		}
		for len(p) > 0 {
			space := deltaChunk - len(lit)
			if space <= 0 {
				if err := flushLit(); err != nil {
					return err
				}
				space = deltaChunk
			}
			take := len(p)
			if take > space {
				take = space
			}
			lit = append(lit, p[:take]...)
			p = p[take:]
			if len(lit) >= deltaChunk {
				if err := flushLit(); err != nil {
					return err
				}
			}
		}
		return nil
	}

	pos := int64(0)
	chunk, err := src.slice(0, window)
	if err != nil {
		return err
	}
	sum := Checksum1(chunk)
	for pos+int64(window) <= size {
		if idxs := table[sum]; len(idxs) > 0 {
			win, err := src.slice(pos, window)
			if err != nil {
				return err
			}
			strong := sha256.Sum256(win)
			hit := -1
			for _, i := range idxs {
				if sigs[i].Strong == strong {
					hit = i
					break
				}
			}
			if hit >= 0 {
				if err := addCopy(hit); err != nil {
					return err
				}
				pos += int64(window)
				if pos+int64(window) <= size {
					win, err = src.slice(pos, window)
					if err != nil {
						return err
					}
					sum = Checksum1(win)
				}
				continue
			}
		}
		// Include the incoming byte in the same read so the buffer reload
		// keeps the start of this window. The literal is copied out.
		need := window
		var next byte
		var haveNext bool
		if pos+int64(window) < size {
			need = window + 1
			haveNext = true
		}
		win, err := src.slice(pos, need)
		if err != nil {
			return err
		}
		if err := addLit(win[:1]); err != nil {
			return err
		}
		if haveNext {
			next = win[window]
			sum = RollChecksum(sum, window, win[0], next)
		}
		pos++
	}
	if pos < size {
		if err := emitLiteralRange(r, pos, size, func(op DeltaOp) error {
			return addLit(op.Lit)
		}); err != nil {
			return err
		}
	}
	if err := flushLit(); err != nil {
		return err
	}
	return flushCopy()
}

func emitLiteralRange(r io.ReaderAt, from, to int64, emit func(DeltaOp) error) error {
	if to < from {
		return errors.New("bad literal range")
	}
	buf := make([]byte, deltaChunk)
	for from < to {
		n := int(to - from)
		if n > len(buf) {
			n = len(buf)
		}
		if err := readFullAt(r, buf[:n], from); err != nil {
			return err
		}
		chunk := make([]byte, n)
		copy(chunk, buf[:n])
		if err := emit(DeltaOp{Lit: chunk}); err != nil {
			return err
		}
		from += int64(n)
	}
	return nil
}

// blockHasher hashes a reconstructed file the same way HashReader does.
type blockHasher struct {
	blocks [][32]byte
	h      hash.Hash
	filled int
	total  int64
}

func newBlockHasher() *blockHasher {
	return &blockHasher{h: sha256.New()}
}

func (b *blockHasher) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		space := proto.BlockSize - b.filled
		if space == 0 {
			b.finish()
			space = proto.BlockSize
		}
		take := len(p)
		if take > space {
			take = space
		}
		_, _ = b.h.Write(p[:take])
		b.filled += take
		b.total += int64(take)
		p = p[take:]
		if b.filled == proto.BlockSize {
			b.finish()
		}
	}
	return n, nil
}

func (b *blockHasher) finish() {
	var sum [32]byte
	copy(sum[:], b.h.Sum(nil))
	b.h.Reset()
	b.blocks = append(b.blocks, sum)
	b.filled = 0
}

// Finish returns the block hashes, including a short tail.
func (b *blockHasher) Finish() ([][32]byte, int64) {
	if b.filled > 0 {
		b.finish()
	}
	return b.blocks, b.total
}

// byteSrc buffers a ReaderAt so the rolling window does not ReadAt per byte.
// A reload always starts at the earliest byte the caller still needs.
type byteSrc struct {
	r    io.ReaderAt
	size int64
	base int64
	buf  []byte
}

func (b *byteSrc) slice(at int64, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	end := at + int64(n)
	if at < 0 || end > b.size {
		return nil, io.ErrUnexpectedEOF
	}
	if at < b.base || end > b.base+int64(len(b.buf)) {
		if err := b.fill(at); err != nil {
			return nil, err
		}
	}
	if at < b.base || end > b.base+int64(len(b.buf)) {
		return nil, io.ErrUnexpectedEOF
	}
	off := int(at - b.base)
	return b.buf[off : off+n], nil
}

func (b *byteSrc) fill(at int64) error {
	want := cap(b.buf)
	if int64(want) > b.size-at {
		want = int(b.size - at)
	}
	if want <= 0 {
		return io.ErrUnexpectedEOF
	}
	if cap(b.buf) < want {
		b.buf = make([]byte, want)
	} else {
		b.buf = b.buf[:want]
	}
	if err := readFullAt(b.r, b.buf, at); err != nil {
		return err
	}
	b.base = at
	return nil
}

func readFullAt(r io.ReaderAt, buf []byte, off int64) error {
	n := 0
	for n < len(buf) {
		nn, err := r.ReadAt(buf[n:], off+int64(n))
		n += nn
		if n == len(buf) {
			return nil
		}
		if err != nil {
			return err
		}
		if nn == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}
