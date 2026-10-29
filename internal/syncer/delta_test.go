package syncer

import (
	"bytes"
	"crypto/sha256"
	"math/rand"
	"testing"

	"github.com/netcraft/peersync/internal/proto"
)

func TestRollChecksumMatchesRecompute(t *testing.T) {
	// Hand check: window [1,2,3] rolled by 4.
	sum := Checksum1([]byte{1, 2, 3})
	if got, want := RollChecksum(sum, 3, 1, 4), Checksum1([]byte{2, 3, 4}); got != want {
		t.Fatalf("roll %x want %x", got, want)
	}
	rng := rand.New(rand.NewSource(1))
	buf := make([]byte, proto.BlockSize+400)
	rng.Read(buf)
	sum = Checksum1(buf[:proto.BlockSize])
	for i := 0; i < 400; i++ {
		sum = RollChecksum(sum, proto.BlockSize, buf[i], buf[i+proto.BlockSize])
		want := Checksum1(buf[i+1 : i+1+proto.BlockSize])
		if sum != want {
			t.Fatalf("step %d: roll %x want %x", i, sum, want)
		}
	}
}

func TestMatchDeltaReconstructs(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	basis := make([]byte, 12*proto.BlockSize)
	rng.Read(basis)
	sigs := sigsOf(t, basis)

	same := append([]byte(nil), basis...)
	if got := rebuild(t, basis, same, sigs); !bytes.Equal(got, same) {
		t.Fatal("identical file was not rebuilt from copies")
	}

	shifted := append([]byte{0xab}, basis...)
	got := rebuild(t, basis, shifted, sigs)
	if !bytes.Equal(got, shifted) {
		t.Fatalf("prepend rebuilt %d bytes, want %d", len(got), len(shifted))
	}

	edited := append([]byte(nil), basis...)
	edited[proto.BlockSize] ^= 0xff
	ops := collectOps(t, edited, sigs)
	var lit int
	for _, op := range ops {
		lit += len(op.Lit)
	}
	if lit > proto.BlockSize+8 {
		t.Fatalf("one changed block sent %d literal bytes", lit)
	}
	if !bytes.Equal(rebuild(t, basis, edited, sigs), edited) {
		t.Fatal("edited file mismatch")
	}
}

func TestBlobRoundTrip(t *testing.T) {
	req := proto.BlobReq{
		ReqID: 7,
		Mode:  proto.BlobDelta,
		Path:  "dir/a.bin",
		Sigs: []proto.BasisSig{{
			Weak:   0x1234,
			Strong: sha256.Sum256([]byte("block")),
		}},
	}
	raw, err := proto.EncodeBlobReq(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proto.DecodeBlobReq(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReqID != 7 || got.Path != req.Path || len(got.Sigs) != 1 || got.Sigs[0].Weak != 0x1234 || got.Sigs[0].Strong != req.Sigs[0].Strong {
		t.Fatalf("req %+v", got)
	}
	spans := proto.BlobReq{
		ReqID: 3, Mode: proto.BlobRaw, Path: "b",
		Spans: []proto.BlobSpan{{Start: 1, Count: 2}, {Start: 8, Count: 1}},
	}
	raw, err = proto.EncodeBlobReq(spans)
	if err != nil {
		t.Fatal(err)
	}
	got, err = proto.DecodeBlobReq(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entire || len(got.Spans) != 2 || got.Spans[1].Start != 8 {
		t.Fatalf("spans %+v", got)
	}
	body, err := proto.EncodeBlobData(9, 4, []byte("xyz"))
	if err != nil {
		t.Fatal(err)
	}
	bl, err := proto.DecodeBlob(body)
	if err != nil {
		t.Fatal(err)
	}
	if bl.Kind != proto.BlobData || bl.Index != 4 || string(bl.Data) != "xyz" {
		t.Fatalf("data %+v", bl)
	}
}

func sigsOf(t *testing.T, basis []byte) []proto.BasisSig {
	t.Helper()
	n := len(basis) / proto.BlockSize
	out := make([]proto.BasisSig, n)
	for i := 0; i < n; i++ {
		chunk := basis[i*proto.BlockSize : (i+1)*proto.BlockSize]
		out[i] = proto.BasisSig{Weak: Checksum1(chunk), Strong: sha256.Sum256(chunk)}
	}
	return out
}

func collectOps(t *testing.T, file []byte, sigs []proto.BasisSig) []DeltaOp {
	t.Helper()
	var ops []DeltaOp
	err := MatchDelta(bytes.NewReader(file), int64(len(file)), sigs, func(op DeltaOp) error {
		op.Lit = append([]byte(nil), op.Lit...)
		ops = append(ops, op)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func rebuild(t *testing.T, basis, file []byte, sigs []proto.BasisSig) []byte {
	t.Helper()
	var out []byte
	for _, op := range collectOps(t, file, sigs) {
		if len(op.Lit) > 0 {
			out = append(out, op.Lit...)
			continue
		}
		off := op.Start * proto.BlockSize
		n := op.Count * proto.BlockSize
		if off < 0 || n < 0 || off+n > len(basis) {
			t.Fatalf("copy %d+%d outside basis %d", off, n, len(basis))
		}
		out = append(out, basis[off:off+n]...)
	}
	return out
}
