// Package proto is the directory sync protocol spoken on a direct peer
// connection. None of these messages are sent to the discovery server.
package proto

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/netcraft/peersync/internal/vclock"
)

const (
	TypeAuth     byte = 1
	TypeIndex    byte = 2
	TypeMetaReq  byte = 3
	TypeMeta     byte = 4
	TypeBlockReq byte = 5
	TypeBlock    byte = 6
	TypePing     byte = 7
	TypePong     byte = 8
	// TypeBlobReq asks a peer to push file bytes. The sender streams
	// TypeBlob frames and does not wait for a request per block.
	TypeBlobReq byte = 9
	TypeBlob    byte = 10
)

const (
	// BlobRaw is a stream of file blocks, either the whole file or a
	// list of ranges. BlobDelta is an rsync-style patch against a basis.
	BlobRaw   byte = 0
	BlobDelta byte = 1

	BlobData    byte = 0
	BlobEOF     byte = 1
	BlobError   byte = 2
	BlobCopy    byte = 3
	BlobLiteral byte = 4
)

// MaxDeltaBlocks is how many full-size basis blocks fit in one signature.
const MaxDeltaBlocks = 20000

// BlockSize is the fixed chunk size used to hash and transfer file contents.
const BlockSize = 128 << 10

// Auth is the first message on a connection. It proves the device key and
// that the peer knows the folder key, and it is bound to this TLS session.
type Auth struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	ShareID  string `json:"share_id"`
	Sig      []byte `json:"sig"`
	Mac      []byte `json:"mac"`
}

// Entry is one path in an index announcement. Block hashes are not included;
// a peer asks for those only when it decides to download the file.
type Entry struct {
	Path    string        `json:"path"`
	Size    int64         `json:"size,omitempty"`
	ModTime int64         `json:"mod_time,omitempty"`
	Mode    uint32        `json:"mode,omitempty"`
	IsDir   bool          `json:"dir,omitempty"`
	Deleted bool          `json:"deleted,omitempty"`
	Sum     string        `json:"sum,omitempty"`
	Version vclock.Vector `json:"version"`
}

// IndexPage is one page of an index announcement.
type IndexPage struct {
	Seq     uint64  `json:"seq"`
	Part    int     `json:"part"`
	Total   int     `json:"total"`
	Entries []Entry `json:"entries"`
}

// MetaReq asks for the block hashes of one file.
type MetaReq struct {
	ReqID uint32 `json:"req_id"`
	Path  string `json:"path"`
}

// BlockReq asks for one block.
type BlockReq struct {
	ReqID uint32 `json:"req_id"`
	Path  string `json:"path"`
	Index int    `json:"index"`
}

// Meta is the full block list of a file.
type Meta struct {
	ReqID  uint32
	Path   string
	Sum    [32]byte
	Blocks [][32]byte
	Err    string
}

// Transcript binds an authentication message to a finished TLS session.
func Transcript(cs tls.ConnectionState, shareID, deviceID string) ([]byte, error) {
	ekm, err := cs.ExportKeyingMaterial("peersync-v1", nil, 32)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("peersync-auth-v1"))
	_, _ = h.Write(ekm)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(shareID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(deviceID))
	return h.Sum(nil), nil
}

// SignAuth fills the signature and MAC.
func SignAuth(priv ed25519.PrivateKey, shareKey []byte, cs tls.ConnectionState, a Auth) (Auth, error) {
	msg, err := Transcript(cs, a.ShareID, a.DeviceID)
	if err != nil {
		return Auth{}, err
	}
	a.Sig = ed25519.Sign(priv, msg)
	m := hmac.New(sha256.New, shareKey)
	_, _ = m.Write(msg)
	a.Mac = m.Sum(nil)
	return a, nil
}

// VerifyAuth checks the device signature and the folder-key MAC.
func VerifyAuth(pub ed25519.PublicKey, shareKey []byte, cs tls.ConnectionState, a Auth) error {
	msg, err := Transcript(cs, a.ShareID, a.DeviceID)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, msg, a.Sig) {
		return errors.New("device signature rejected")
	}
	m := hmac.New(sha256.New, shareKey)
	_, _ = m.Write(msg)
	if !hmac.Equal(m.Sum(nil), a.Mac) {
		return errors.New("folder key rejected")
	}
	return nil
}

// EncodeMeta serializes a block list. Large lists are split by the caller
// via Meta.Slice.
func EncodeMeta(m Meta) ([]byte, error) {
	if len(m.Blocks) > 4096 {
		return nil, errors.New("meta chunk too large")
	}
	if len(m.Path) > 4096 || len(m.Err) > 1024 {
		return nil, errors.New("meta field too large")
	}
	buf := make([]byte, 0, 48+len(m.Path)+len(m.Err)+len(m.Blocks)*32)
	var hdr [16]byte
	binary.BigEndian.PutUint32(hdr[0:4], m.ReqID)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(m.Blocks)))
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(m.Path)))
	binary.BigEndian.PutUint32(hdr[12:16], uint32(len(m.Err)))
	buf = append(buf, hdr[:]...)
	buf = append(buf, m.Sum[:]...)
	buf = append(buf, m.Path...)
	buf = append(buf, m.Err...)
	for _, b := range m.Blocks {
		buf = append(buf, b[:]...)
	}
	return buf, nil
}

// DecodeMeta parses EncodeMeta.
func DecodeMeta(b []byte) (Meta, error) {
	if len(b) < 16+32 {
		return Meta{}, errors.New("short meta")
	}
	var m Meta
	m.ReqID = binary.BigEndian.Uint32(b[0:4])
	nBlocks := binary.BigEndian.Uint32(b[4:8])
	nPath := binary.BigEndian.Uint32(b[8:12])
	nErr := binary.BigEndian.Uint32(b[12:16])
	if nBlocks > 4096 || nPath > 4096 || nErr > 1024 {
		return Meta{}, errors.New("meta field too large")
	}
	need := 16 + 32 + int(nPath) + int(nErr) + int(nBlocks)*32
	if len(b) != need {
		return Meta{}, fmt.Errorf("meta length %d want %d", len(b), need)
	}
	copy(m.Sum[:], b[16:48])
	off := 48
	m.Path = string(b[off : off+int(nPath)])
	off += int(nPath)
	m.Err = string(b[off : off+int(nErr)])
	off += int(nErr)
	m.Blocks = make([][32]byte, nBlocks)
	for i := range m.Blocks {
		copy(m.Blocks[i][:], b[off:off+32])
		off += 32
	}
	return m, nil
}

// MetaChunk is one slice of a block list plus the absolute offset.
type MetaChunk struct {
	Meta
	Offset int
	Total  int
}

// EncodeMetaChunk serializes one slice of a block list.
func EncodeMetaChunk(c MetaChunk) ([]byte, error) {
	body, err := EncodeMeta(c.Meta)
	if err != nil {
		return nil, err
	}
	var extra [8]byte
	binary.BigEndian.PutUint32(extra[0:4], uint32(c.Offset))
	binary.BigEndian.PutUint32(extra[4:8], uint32(c.Total))
	return append(extra[:], body...), nil
}

// DecodeMetaChunk parses EncodeMetaChunk.
func DecodeMetaChunk(b []byte) (MetaChunk, error) {
	if len(b) < 8 {
		return MetaChunk{}, errors.New("short meta chunk")
	}
	c := MetaChunk{
		Offset: int(binary.BigEndian.Uint32(b[0:4])),
		Total:  int(binary.BigEndian.Uint32(b[4:8])),
	}
	m, err := DecodeMeta(b[8:])
	if err != nil {
		return MetaChunk{}, err
	}
	c.Meta = m
	return c, nil
}

// EncodeBlock serializes a block response. status 0 carries data, status 1
// carries an error string.
func EncodeBlock(reqID uint32, index int, data []byte, errText string) []byte {
	buf := make([]byte, 10, 10+len(data)+len(errText))
	binary.BigEndian.PutUint32(buf[0:4], reqID)
	binary.BigEndian.PutUint32(buf[4:8], uint32(index))
	if errText != "" {
		binary.BigEndian.PutUint16(buf[8:10], 1)
		buf = append(buf, errText...)
		return buf
	}
	binary.BigEndian.PutUint16(buf[8:10], 0)
	buf = append(buf, data...)
	return buf
}

// DecodeBlock parses a block response.
func DecodeBlock(b []byte) (reqID uint32, index int, data []byte, errText string, err error) {
	if len(b) < 10 {
		return 0, 0, nil, "", errors.New("short block")
	}
	reqID = binary.BigEndian.Uint32(b[0:4])
	index = int(binary.BigEndian.Uint32(b[4:8]))
	status := binary.BigEndian.Uint16(b[8:10])
	rest := b[10:]
	if status == 1 {
		return reqID, index, nil, string(rest), nil
	}
	if status != 0 {
		return 0, 0, nil, "", errors.New("bad block status")
	}
	return reqID, index, rest, "", nil
}

// BlobSpan is a contiguous run of blocks inside a raw transfer.
type BlobSpan struct {
	Start int
	Count int
}

// BasisSig is the rsync signature of one full-size basis block.
type BasisSig struct {
	Weak   uint32
	Strong [32]byte
}

// BlobReq asks the peer to push bytes for Path.
// Entire is only used with BlobRaw. Sigs is only used with BlobDelta.
type BlobReq struct {
	ReqID  uint32
	Mode   byte
	Path   string
	Entire bool
	Spans  []BlobSpan
	Sigs   []BasisSig
}

// Blob is one frame of a pushed transfer.
type Blob struct {
	ReqID uint32
	Kind  byte
	Index int
	Count int
	Data  []byte
	Err   string
}

// EncodeBlobReq serializes a push request.
func EncodeBlobReq(r BlobReq) ([]byte, error) {
	if len(r.Path) > 4096 || (r.Mode != BlobRaw && r.Mode != BlobDelta) {
		return nil, errors.New("bad blob request")
	}
	n := 7 + len(r.Path)
	switch r.Mode {
	case BlobRaw:
		n++
		if !r.Entire {
			if len(r.Spans) == 0 || len(r.Spans) > 4096 {
				return nil, errors.New("bad blob spans")
			}
			n += 2 + len(r.Spans)*8
		}
	case BlobDelta:
		if len(r.Sigs) == 0 || len(r.Sigs) > MaxDeltaBlocks {
			return nil, errors.New("bad blob signature")
		}
		n += 4 + len(r.Sigs)*36
	}
	buf := make([]byte, 0, n)
	var hdr [7]byte
	binary.BigEndian.PutUint32(hdr[0:4], r.ReqID)
	hdr[4] = r.Mode
	binary.BigEndian.PutUint16(hdr[5:7], uint16(len(r.Path)))
	buf = append(buf, hdr[:]...)
	buf = append(buf, r.Path...)
	switch r.Mode {
	case BlobRaw:
		if r.Entire {
			buf = append(buf, 1)
			break
		}
		buf = append(buf, 0)
		var nb [2]byte
		binary.BigEndian.PutUint16(nb[:], uint16(len(r.Spans)))
		buf = append(buf, nb[:]...)
		var spb [8]byte
		for _, sp := range r.Spans {
			if sp.Start < 0 || sp.Count <= 0 {
				return nil, errors.New("bad blob span")
			}
			binary.BigEndian.PutUint32(spb[0:4], uint32(sp.Start))
			binary.BigEndian.PutUint32(spb[4:8], uint32(sp.Count))
			buf = append(buf, spb[:]...)
		}
	case BlobDelta:
		var nb [4]byte
		binary.BigEndian.PutUint32(nb[:], uint32(len(r.Sigs)))
		buf = append(buf, nb[:]...)
		var weak [4]byte
		for _, sg := range r.Sigs {
			binary.BigEndian.PutUint32(weak[:], sg.Weak)
			buf = append(buf, weak[:]...)
			buf = append(buf, sg.Strong[:]...)
		}
	}
	return buf, nil
}

// DecodeBlobReq parses EncodeBlobReq.
func DecodeBlobReq(b []byte) (BlobReq, error) {
	if len(b) < 7 {
		return BlobReq{}, errors.New("short blob request")
	}
	var r BlobReq
	r.ReqID = binary.BigEndian.Uint32(b[0:4])
	r.Mode = b[4]
	nPath := int(binary.BigEndian.Uint16(b[5:7]))
	if nPath > 4096 || len(b) < 7+nPath {
		return BlobReq{}, errors.New("bad blob path")
	}
	r.Path = string(b[7 : 7+nPath])
	rest := b[7+nPath:]
	switch r.Mode {
	case BlobRaw:
		if len(rest) < 1 {
			return BlobReq{}, errors.New("short blob request")
		}
		r.Entire = rest[0] == 1
		if rest[0] > 1 {
			return BlobReq{}, errors.New("bad blob request")
		}
		rest = rest[1:]
		if r.Entire {
			if len(rest) != 0 {
				return BlobReq{}, errors.New("trailing blob request")
			}
			return r, nil
		}
		if len(rest) < 2 {
			return BlobReq{}, errors.New("short blob spans")
		}
		n := int(binary.BigEndian.Uint16(rest[0:2]))
		rest = rest[2:]
		if n == 0 || n > 4096 || len(rest) != n*8 {
			return BlobReq{}, errors.New("bad blob spans")
		}
		r.Spans = make([]BlobSpan, n)
		for i := range r.Spans {
			r.Spans[i].Start = int(binary.BigEndian.Uint32(rest[0:4]))
			r.Spans[i].Count = int(binary.BigEndian.Uint32(rest[4:8]))
			if r.Spans[i].Count <= 0 {
				return BlobReq{}, errors.New("bad blob span")
			}
			rest = rest[8:]
		}
		return r, nil
	case BlobDelta:
		if len(rest) < 4 {
			return BlobReq{}, errors.New("short blob signature")
		}
		n := int(binary.BigEndian.Uint32(rest[0:4]))
		rest = rest[4:]
		if n == 0 || n > MaxDeltaBlocks || len(rest) != n*36 {
			return BlobReq{}, errors.New("bad blob signature")
		}
		r.Sigs = make([]BasisSig, n)
		for i := range r.Sigs {
			r.Sigs[i].Weak = binary.BigEndian.Uint32(rest[0:4])
			copy(r.Sigs[i].Strong[:], rest[4:36])
			rest = rest[36:]
		}
		return r, nil
	default:
		return BlobReq{}, errors.New("bad blob mode")
	}
}

// EncodeBlobData is a raw run of blocks starting at firstIndex.
func EncodeBlobData(reqID uint32, firstIndex int, data []byte) ([]byte, error) {
	if firstIndex < 0 || len(data) == 0 || 9+len(data) > 1<<20 {
		return nil, errors.New("bad blob data")
	}
	buf := make([]byte, 9+len(data))
	binary.BigEndian.PutUint32(buf[0:4], reqID)
	buf[4] = BlobData
	binary.BigEndian.PutUint32(buf[5:9], uint32(firstIndex))
	copy(buf[9:], data)
	return buf, nil
}

// EncodeBlobLiteral is a delta literal.
func EncodeBlobLiteral(reqID uint32, data []byte) ([]byte, error) {
	if len(data) == 0 || 5+len(data) > 1<<20 {
		return nil, errors.New("bad blob literal")
	}
	buf := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(buf[0:4], reqID)
	buf[4] = BlobLiteral
	copy(buf[5:], data)
	return buf, nil
}

// EncodeBlobCopy references count consecutive basis blocks.
func EncodeBlobCopy(reqID uint32, start, count int) ([]byte, error) {
	if start < 0 || count <= 0 {
		return nil, errors.New("bad blob copy")
	}
	buf := make([]byte, 13)
	binary.BigEndian.PutUint32(buf[0:4], reqID)
	buf[4] = BlobCopy
	binary.BigEndian.PutUint32(buf[5:9], uint32(start))
	binary.BigEndian.PutUint32(buf[9:13], uint32(count))
	return buf, nil
}

// EncodeBlobEOF marks the end of a push.
func EncodeBlobEOF(reqID uint32) []byte {
	buf := make([]byte, 5)
	binary.BigEndian.PutUint32(buf[0:4], reqID)
	buf[4] = BlobEOF
	return buf
}

// EncodeBlobErr reports a push failure. The text is truncated.
func EncodeBlobErr(reqID uint32, text string) []byte {
	if len(text) > 1024 {
		text = text[:1024]
	}
	if text == "" {
		text = "not available"
	}
	buf := make([]byte, 5+len(text))
	binary.BigEndian.PutUint32(buf[0:4], reqID)
	buf[4] = BlobError
	copy(buf[5:], text)
	return buf
}

// DecodeBlob parses one push frame.
func DecodeBlob(b []byte) (Blob, error) {
	if len(b) < 5 {
		return Blob{}, errors.New("short blob")
	}
	bl := Blob{ReqID: binary.BigEndian.Uint32(b[0:4]), Kind: b[4]}
	rest := b[5:]
	switch bl.Kind {
	case BlobEOF:
		if len(rest) != 0 {
			return Blob{}, errors.New("trailing blob")
		}
	case BlobError:
		if len(rest) == 0 || len(rest) > 1024 {
			return Blob{}, errors.New("bad blob error")
		}
		bl.Err = string(rest)
	case BlobLiteral:
		if len(rest) == 0 {
			return Blob{}, errors.New("empty blob literal")
		}
		bl.Data = rest
	case BlobData:
		if len(rest) < 4 {
			return Blob{}, errors.New("short blob data")
		}
		bl.Index = int(binary.BigEndian.Uint32(rest[0:4]))
		bl.Data = rest[4:]
		if len(bl.Data) == 0 {
			return Blob{}, errors.New("empty blob data")
		}
	case BlobCopy:
		if len(rest) != 8 {
			return Blob{}, errors.New("bad blob copy")
		}
		bl.Index = int(binary.BigEndian.Uint32(rest[0:4]))
		bl.Count = int(binary.BigEndian.Uint32(rest[4:8]))
		if bl.Count <= 0 {
			return Blob{}, errors.New("bad blob copy")
		}
	default:
		return Blob{}, errors.New("bad blob kind")
	}
	return bl, nil
}

// EncodeSum renders a content sum. The zero sum (directories, tombstones)
// becomes an empty string.
func EncodeSum(sum [32]byte, present bool) string {
	if !present {
		return ""
	}
	return hex.EncodeToString(sum[:])
}

// DecodeSum parses a content sum. ok is false when the text is not 32 bytes.
func DecodeSum(s string) (sum [32]byte, ok bool) {
	if s == "" {
		return sum, false
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return sum, false
	}
	copy(sum[:], raw)
	return sum, true
}

// MustJSON marshals v or panics. Callers use it for small protocol structs
// whose shape cannot fail.
func MustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
