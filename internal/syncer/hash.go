package syncer

import (
	"crypto/sha256"
	"errors"
	"io"

	"github.com/netcraft/peersync/internal/proto"
)

// HashReader hashes a file as fixed-size blocks.
func HashReader(r io.Reader) (sum [32]byte, blocks [][32]byte, size int64, err error) {
	buf := make([]byte, proto.BlockSize)
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			size += int64(n)
			blocks = append(blocks, sha256.Sum256(buf[:n]))
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return sum, nil, 0, rerr
		}
	}
	return SumBlocks(blocks), blocks, size, nil
}

// SumBlocks is the content id of a file.
func SumBlocks(blocks [][32]byte) [32]byte {
	h := sha256.New()
	for _, b := range blocks {
		_, _ = h.Write(b[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ReadBlock reads one chunk. The last chunk may be short.
func ReadBlock(r io.ReaderAt, index int, size int64) ([]byte, error) {
	if index < 0 {
		return nil, errors.New("bad block index")
	}
	off := int64(index) * int64(proto.BlockSize)
	if size == 0 || off >= size {
		return nil, io.ErrUnexpectedEOF
	}
	want := int64(proto.BlockSize)
	if off+want > size {
		want = size - off
	}
	buf := make([]byte, want)
	_, err := r.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

// BlocksForSize is how many chunks a file of size n has.
func BlocksForSize(n int64) int {
	if n <= 0 {
		return 0
	}
	return int((n + int64(proto.BlockSize) - 1) / int64(proto.BlockSize))
}

// BlockLen is the byte length of chunk index inside a file of size n.
func BlockLen(n int64, index int) int {
	if index < 0 || n <= 0 {
		return 0
	}
	off := int64(index) * int64(proto.BlockSize)
	if off >= n {
		return 0
	}
	want := n - off
	if want > int64(proto.BlockSize) {
		want = int64(proto.BlockSize)
	}
	return int(want)
}
