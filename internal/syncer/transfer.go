package syncer

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/netcraft/peersync/internal/proto"
)

const (
	// alignedKeep is the fraction of remote blocks already present at the
	// same offset. Above it, copying those blocks and fetching the gaps is
	// cheaper than a rolling scan.
	alignedKeep = 0.20

	rawFrameBlocks = 7
	smallParallel  = 8
	largeParallel  = 2
	largeFile      = 1 << 20
	blobIdle       = 45 * time.Second
)

// errNeedWhole means a patch could not be applied and the caller should
// stream the whole file instead.
var errNeedWhole = errors.New("need whole file")

type planKind int

const (
	planAll planKind = iota
	planRanges
	planDelta
	planLocal
)

type xferPlan struct {
	kind      planKind
	keep      []int
	spans     []proto.BlobSpan
	sigs      []proto.BasisSig
	basis     string
	basisFull int
}

type blobPiece struct {
	kind  byte
	index int
	count int
	data  []byte
	err   error
	eof   bool
}

func (s *session) pullAll(tasks []Task) {
	var small, large []Task
	for _, task := range tasks {
		if task.Remote.Size >= largeFile {
			large = append(large, task)
		} else {
			small = append(small, task)
		}
	}
	s.pullParallel(small, smallParallel)
	s.pullParallel(large, largeParallel)
}

func (s *session) pullParallel(tasks []Task, n int) {
	if len(tasks) == 0 {
		return
	}
	if n < 1 {
		n = 1
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, task := range tasks {
		select {
		case <-s.ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(task Task) {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.download(task)
			if err != nil && !errors.Is(err, errStale) {
				s.log.Warn("sync step failed", "op", task.Op, "path", task.Path, "err", err)
			}
			s.done(task, err)
		}(task)
	}
	wg.Wait()
}

func (s *session) download(task Task) error {
	s.e.Begin(task.Path)
	defer s.e.End(task.Path)
	meta, err := s.fetchMeta(task)
	if err != nil {
		return err
	}
	tmp, tmpName, err := s.createTemp()
	if err != nil {
		return err
	}
	closed := false
	closeTmp := func() error {
		if closed {
			return nil
		}
		closed = true
		return tmp.Close()
	}
	defer func() { _ = os.Remove(tmpName) }()
	defer closeTmp()

	plan := s.planTransfer(task.Path, meta.Blocks, task.Remote.Size)
	s.log.Debug("transfer", "path", task.Path, "mode", plan.kind, "blocks", len(meta.Blocks), "missing", missingCount(plan, len(meta.Blocks)))
	var xerr error
	switch plan.kind {
	case planLocal:
		xerr = s.fillFromLocal(tmp, plan.basis, task.Remote.Size, meta.Blocks)
	case planRanges:
		xerr = s.fillRanges(tmp, task, meta, plan)
	case planDelta:
		xerr = s.fillDelta(tmp, task, meta, plan)
	default:
		xerr = s.fillRaw(tmp, task, meta)
	}
	if errors.Is(xerr, errNeedWhole) {
		s.log.Debug("whole file fallback", "path", task.Path, "err", xerr)
		if err := rewind(tmp); err != nil {
			return err
		}
		xerr = s.fillRaw(tmp, task, meta)
	}
	if xerr != nil {
		return xerr
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := closeTmp(); err != nil {
		return err
	}
	return s.e.CommitDownload(task, tmpName, meta.Blocks)
}

func missingCount(plan xferPlan, blocks int) int {
	switch plan.kind {
	case planLocal:
		return 0
	case planRanges:
		n := blocks - len(plan.keep)
		if n < 0 {
			return 0
		}
		return n
	case planAll:
		return blocks
	default:
		return -1
	}
}

func (s *session) fetchMeta(task Task) (proto.Meta, error) {
	id := s.allocID()
	body, _ := json.Marshal(proto.MetaReq{ReqID: id, Path: task.Path})
	meta, err := s.awaitMeta(id, proto.TypeMetaReq, body, 30*time.Second)
	if err != nil {
		return proto.Meta{}, err
	}
	if meta.Err != "" {
		return proto.Meta{}, errors.New(meta.Err)
	}
	want, ok := proto.DecodeSum(task.Remote.Sum)
	if !ok || want != meta.Sum || SumBlocks(meta.Blocks) != meta.Sum {
		return proto.Meta{}, errors.New("peer metadata does not match its index")
	}
	if BlocksForSize(task.Remote.Size) != len(meta.Blocks) {
		return proto.Meta{}, errors.New("peer block count does not match size")
	}
	return meta, nil
}

func (s *session) createTemp() (*os.File, string, error) {
	dir := filepath.Join(s.e.Root, tmpDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	f, err := os.CreateTemp(dir, "part-*")
	if err != nil {
		return nil, "", err
	}
	return f, f.Name(), nil
}

func (s *session) planTransfer(rel string, blocks [][32]byte, remoteSize int64) xferPlan {
	all := xferPlan{kind: planAll}
	if len(blocks) == 0 {
		return all
	}
	_, localBlocks, localSize, err := s.e.Meta(rel)
	if err != nil || localSize <= 0 {
		return all
	}
	abs := filepath.Join(s.e.Root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() || info.Size() != localSize {
		return all
	}
	var missing []int
	matched := 0
	for i, b := range blocks {
		if i < len(localBlocks) && localBlocks[i] == b {
			matched++
		} else {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return xferPlan{kind: planLocal, basis: abs}
	}
	if float64(matched)/float64(len(blocks)) >= alignedKeep {
		spans := spansOf(missing)
		if len(spans) > 0 && len(spans) <= 4096 {
			keep := make([]int, 0, matched)
			skip := make([]bool, len(blocks))
			for _, i := range missing {
				skip[i] = true
			}
			for i := range blocks {
				if !skip[i] {
					keep = append(keep, i)
				}
			}
			return xferPlan{kind: planRanges, keep: keep, spans: spans, basis: abs}
		}
	}
	nFull := int(localSize / int64(proto.BlockSize))
	if nFull >= 1 && nFull <= proto.MaxDeltaBlocks && sizesClose(localSize, remoteSize) {
		sigs, err := basisSigs(abs, nFull)
		if err == nil {
			return xferPlan{kind: planDelta, sigs: sigs, basis: abs, basisFull: nFull}
		}
	}
	return all
}

func sizesClose(local, remote int64) bool {
	if local <= 0 || remote <= 0 {
		return false
	}
	block := int64(proto.BlockSize)
	if remote > local*2+block {
		return false
	}
	if local > remote*2+block {
		return false
	}
	return true
}

func spansOf(idxs []int) []proto.BlobSpan {
	if len(idxs) == 0 {
		return nil
	}
	out := make([]proto.BlobSpan, 0, 1)
	start, prev := idxs[0], idxs[0]
	for _, i := range idxs[1:] {
		if i == prev+1 {
			prev = i
			continue
		}
		out = append(out, proto.BlobSpan{Start: start, Count: prev - start + 1})
		start, prev = i, i
	}
	out = append(out, proto.BlobSpan{Start: start, Count: prev - start + 1})
	return out
}

func basisSigs(path string, nFull int) ([]proto.BasisSig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, proto.BlockSize)
	out := make([]proto.BasisSig, nFull)
	for i := 0; i < nFull; i++ {
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, err
		}
		out[i] = proto.BasisSig{Weak: Checksum1(buf), Strong: sha256.Sum256(buf)}
	}
	return out, nil
}

func (s *session) fillFromLocal(dst *os.File, srcPath string, n int64, want [][32]byte) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	hw := newBlockHasher()
	if _, err := io.CopyN(io.MultiWriter(dst, hw), src, n); err != nil {
		return errNeedWhole
	}
	got, total := hw.Finish()
	if total != n || !sameBlocks(got, want) {
		return errNeedWhole
	}
	return nil
}

func (s *session) fillRanges(dst *os.File, task Task, meta proto.Meta, plan xferPlan) error {
	if err := dst.Truncate(task.Remote.Size); err != nil {
		return err
	}
	if err := writeKept(dst, plan.basis, task.Remote.Size, meta.Blocks, plan.keep); err != nil {
		return err
	}
	var last error
	for try := 0; try < 4; try++ {
		if err := s.waitTry(try); err != nil {
			return err
		}
		if try > 0 {
			if err := writeKept(dst, plan.basis, task.Remote.Size, meta.Blocks, plan.keep); err != nil {
				return err
			}
		}
		req := proto.BlobReq{ReqID: s.allocID(), Mode: proto.BlobRaw, Path: task.Path, Spans: plan.spans}
		err := s.recvRaw(dst, task.Remote.Size, meta.Blocks, req)
		if err == nil {
			return nil
		}
		last = err
		if !isBusy(err) {
			return err
		}
	}
	return last
}

func (s *session) fillDelta(dst *os.File, task Task, meta proto.Meta, plan xferPlan) error {
	var last error
	for try := 0; try < 4; try++ {
		if err := s.waitTry(try); err != nil {
			return err
		}
		if try > 0 {
			if err := rewind(dst); err != nil {
				return err
			}
		}
		req := proto.BlobReq{
			ReqID: s.allocID(),
			Mode:  proto.BlobDelta,
			Path:  task.Path,
			Sigs:  plan.sigs,
		}
		err := s.recvDelta(dst, task, meta, plan, req)
		if err == nil {
			return nil
		}
		last = err
		if !isBusy(err) {
			return err
		}
	}
	return last
}

func (s *session) fillRaw(dst *os.File, task Task, meta proto.Meta) error {
	var last error
	for try := 0; try < 4; try++ {
		if err := s.waitTry(try); err != nil {
			return err
		}
		if try > 0 {
			if err := rewind(dst); err != nil {
				return err
			}
		}
		req := proto.BlobReq{ReqID: s.allocID(), Mode: proto.BlobRaw, Path: task.Path, Entire: true}
		err := s.recvRaw(dst, task.Remote.Size, meta.Blocks, req)
		if err == nil {
			return nil
		}
		last = err
		if !isBusy(err) {
			return err
		}
	}
	return last
}

func (s *session) waitTry(try int) error {
	if try == 0 {
		return s.ctx.Err()
	}
	timer := time.NewTimer(time.Duration(try) * 30 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isBusy(err error) bool {
	return err != nil && err.Error() == "busy"
}

func (s *session) beginBlob(req proto.BlobReq) (<-chan blobPiece, <-chan struct{}, error) {
	body, err := proto.EncodeBlobReq(req)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan blobPiece, 4)
	done := make(chan struct{})
	s.pendMu.Lock()
	s.pending[req.ReqID] = &waiter{kind: proto.TypeBlob, blobs: ch, done: done}
	s.pendMu.Unlock()
	if err := s.conn.Write(proto.TypeBlobReq, body); err != nil {
		s.forget(req.ReqID)
		return nil, nil, err
	}
	return ch, done, nil
}

func (s *session) readBlob(ch <-chan blobPiece, done <-chan struct{}) (blobPiece, error) {
	timer := time.NewTimer(blobIdle)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return blobPiece{}, s.ctx.Err()
	case <-done:
		return blobPiece{}, errors.New("transfer cancelled")
	case <-timer.C:
		return blobPiece{}, errors.New("transfer timed out")
	case p := <-ch:
		if p.err != nil {
			return blobPiece{}, p.err
		}
		return p, nil
	}
}

func (s *session) recvRaw(dst *os.File, remoteSize int64, want [][32]byte, req proto.BlobReq) error {
	ch, done, err := s.beginBlob(req)
	if err != nil {
		return err
	}
	defer s.forget(req.ReqID)

	spans := req.Spans
	if req.Entire && len(want) > 0 {
		spans = []proto.BlobSpan{{Start: 0, Count: len(want)}}
	}
	var expect int64
	if req.Entire {
		expect = remoteSize
	} else {
		for _, sp := range spans {
			for i := 0; i < sp.Count; i++ {
				expect += int64(BlockLen(remoteSize, sp.Start+i))
			}
		}
	}
	spanPos, within := 0, 0
	var got int64
	takeIndex := func() (int, error) {
		if spanPos >= len(spans) {
			return 0, errors.New("extra block")
		}
		idx := spans[spanPos].Start + within
		within++
		if within == spans[spanPos].Count {
			spanPos++
			within = 0
		}
		return idx, nil
	}
	for {
		p, err := s.readBlob(ch, done)
		if err != nil {
			return err
		}
		if p.eof {
			break
		}
		if p.kind != proto.BlobData {
			return errors.New("unexpected blob")
		}
		data := p.data
		index := p.index
		for len(data) > 0 {
			wantIdx, err := takeIndex()
			if err != nil {
				return err
			}
			if index != wantIdx {
				return errors.New("block out of order")
			}
			n := BlockLen(remoteSize, index)
			if n == 0 || len(data) < n {
				return errors.New("short block")
			}
			if sha256.Sum256(data[:n]) != want[index] {
				return errors.New("block hash mismatch")
			}
			var werr error
			if req.Entire {
				_, werr = dst.Write(data[:n])
			} else {
				_, werr = dst.WriteAt(data[:n], int64(index)*int64(proto.BlockSize))
			}
			if werr != nil {
				return werr
			}
			data = data[n:]
			got += int64(n)
			index++
		}
	}
	if spanPos != len(spans) || within != 0 || got != expect {
		return errors.New("incomplete file")
	}
	return nil
}

func (s *session) recvDelta(dst *os.File, task Task, meta proto.Meta, plan xferPlan, req proto.BlobReq) error {
	ch, done, err := s.beginBlob(req)
	if err != nil {
		return err
	}
	defer s.forget(req.ReqID)
	basis, err := os.Open(plan.basis)
	if err != nil {
		return errNeedWhole
	}
	defer basis.Close()
	info, err := basis.Stat()
	if err != nil || int(info.Size()/int64(proto.BlockSize)) < plan.basisFull {
		return errNeedWhole
	}
	hw := newBlockHasher()
	out := io.MultiWriter(dst, hw)
	buf := make([]byte, 256<<10)
	for {
		p, err := s.readBlob(ch, done)
		if err != nil {
			return err
		}
		if p.eof {
			break
		}
		switch p.kind {
		case proto.BlobLiteral:
			if _, err := out.Write(p.data); err != nil {
				return err
			}
		case proto.BlobCopy:
			if p.index < 0 || p.count <= 0 || p.index+p.count > plan.basisFull {
				return errNeedWhole
			}
			off := int64(p.index) * int64(proto.BlockSize)
			n := int64(p.count) * int64(proto.BlockSize)
			copied, err := io.CopyBuffer(out, io.NewSectionReader(basis, off, n), buf)
			if err != nil {
				return err
			}
			if copied != n {
				return errNeedWhole
			}
		default:
			return errNeedWhole
		}
	}
	got, total := hw.Finish()
	if total != task.Remote.Size || !sameBlocks(got, meta.Blocks) {
		return errNeedWhole
	}
	return nil
}

func writeKept(dst *os.File, srcPath string, remoteSize int64, want [][32]byte, keep []int) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	buf := make([]byte, proto.BlockSize)
	for _, i := range keep {
		n := BlockLen(remoteSize, i)
		if n == 0 || i >= len(want) {
			return errNeedWhole
		}
		off := int64(i) * int64(proto.BlockSize)
		if err := readFullAt(src, buf[:n], off); err != nil {
			return err
		}
		if sha256.Sum256(buf[:n]) != want[i] {
			return errNeedWhole
		}
		if _, err := dst.WriteAt(buf[:n], off); err != nil {
			return err
		}
	}
	return nil
}

func sameBlocks(a, b [][32]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func rewind(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.Seek(0, io.SeekStart)
	return err
}

func (s *session) serveBlob(req proto.BlobReq) {
	defer func() { <-s.blobSem }()
	var err error
	switch req.Mode {
	case proto.BlobRaw:
		err = s.serveRaw(req)
	case proto.BlobDelta:
		err = s.serveDelta(req)
	default:
		err = errors.New("bad blob mode")
	}
	if err != nil && s.ctx.Err() == nil {
		s.log.Debug("blob", "path", req.Path, "err", err)
		_ = s.conn.Write(proto.TypeBlob, proto.EncodeBlobErr(req.ReqID, "not available"))
	}
}

func (s *session) serveRaw(req proto.BlobReq) error {
	_, blocks, size, err := s.e.Meta(req.Path)
	if err != nil {
		return err
	}
	spans := req.Spans
	if req.Entire {
		if len(blocks) == 0 {
			return s.conn.Write(proto.TypeBlob, proto.EncodeBlobEOF(req.ReqID))
		}
		spans = []proto.BlobSpan{{Start: 0, Count: len(blocks)}}
	}
	if !validSpans(spans, len(blocks)) {
		return errors.New("bad spans")
	}
	f, err := os.Open(filepath.Join(s.e.Root, filepath.FromSlash(req.Path)))
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return errors.New("file changed")
	}
	buf := make([]byte, rawFrameBlocks*proto.BlockSize)
	for _, sp := range spans {
		block := sp.Start
		end := sp.Start + sp.Count
		for block < end {
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			nblocks := end - block
			if nblocks > rawFrameBlocks {
				nblocks = rawFrameBlocks
			}
			nbytes := 0
			for i := 0; i < nblocks; i++ {
				nbytes += BlockLen(size, block+i)
			}
			if err := readFullAt(f, buf[:nbytes], int64(block)*int64(proto.BlockSize)); err != nil {
				return err
			}
			off := 0
			for i := 0; i < nblocks; i++ {
				n := BlockLen(size, block+i)
				if sha256.Sum256(buf[off:off+n]) != blocks[block+i] {
					return errors.New("file changed")
				}
				off += n
			}
			body, err := proto.EncodeBlobData(req.ReqID, block, buf[:nbytes])
			if err != nil {
				return err
			}
			if err := s.conn.Write(proto.TypeBlob, body); err != nil {
				return err
			}
			block += nblocks
		}
	}
	return s.conn.Write(proto.TypeBlob, proto.EncodeBlobEOF(req.ReqID))
}

func validSpans(spans []proto.BlobSpan, nBlocks int) bool {
	if len(spans) == 0 || nBlocks <= 0 {
		return false
	}
	prev := 0
	total := 0
	for i, sp := range spans {
		if sp.Start < 0 || sp.Count <= 0 || sp.Start+sp.Count > nBlocks {
			return false
		}
		if i > 0 && sp.Start < prev {
			return false
		}
		prev = sp.Start + sp.Count
		total += sp.Count
		if total > nBlocks {
			return false
		}
	}
	return true
}

func (s *session) serveDelta(req proto.BlobReq) error {
	_, _, size, err := s.e.Meta(req.Path)
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(s.e.Root, filepath.FromSlash(req.Path)))
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return errors.New("file changed")
	}
	err = MatchDelta(f, size, req.Sigs, func(op DeltaOp) error {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		if len(op.Lit) > 0 {
			body, err := proto.EncodeBlobLiteral(req.ReqID, op.Lit)
			if err != nil {
				return err
			}
			return s.conn.Write(proto.TypeBlob, body)
		}
		body, err := proto.EncodeBlobCopy(req.ReqID, op.Start, op.Count)
		if err != nil {
			return err
		}
		return s.conn.Write(proto.TypeBlob, body)
	})
	if err != nil {
		return err
	}
	return s.conn.Write(proto.TypeBlob, proto.EncodeBlobEOF(req.ReqID))
}
