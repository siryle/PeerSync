package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/netcraft/peersync/internal/frame"
	"github.com/netcraft/peersync/internal/proto"
)

// Serve runs directory sync on an already authenticated connection until it
// fails or ctx is cancelled.
func Serve(ctx context.Context, e *Engine, conn *frame.Conn, log *slog.Logger, note func(Note)) error {
	if log == nil {
		log = e.Log
	}
	s := &session{
		ctx:        ctx,
		e:          e,
		conn:       conn,
		log:        log,
		note:       note,
		pending:    map[uint32]*waiter{},
		indexReady: make(chan []proto.Entry, 1),
		blobSem:    make(chan struct{}, 16),
	}
	sub, unsub := e.Subscribe()
	defer unsub()
	defer s.failAll(errors.New("connection closed"))

	errc := make(chan error, 3)
	go func() { errc <- s.readLoop() }()
	go func() { errc <- s.syncLoop(sub) }()
	go func() { errc <- s.pingLoop() }()
	select {
	case <-ctx.Done():
		_ = conn.Close()
		<-errc
		return ctx.Err()
	case err := <-errc:
		_ = conn.Close()
		return err
	}
}

type waiter struct {
	kind  byte
	meta  chan proto.Meta
	data  chan blockResult
	blobs chan blobPiece
	done  chan struct{}
	asm   *metaAsm
}

type blockResult struct {
	data []byte
	err  error
}

type session struct {
	ctx        context.Context
	e          *Engine
	conn       *frame.Conn
	log        *slog.Logger
	note       func(Note)
	next       atomic.Uint32
	indexReady chan []proto.Entry
	blobSem    chan struct{}

	pendMu  sync.Mutex
	pending map[uint32]*waiter

	asmMu sync.Mutex
	asm   indexAsm
}

type indexAsm struct {
	seq   uint64
	total int
	parts map[int][]proto.Entry
}

func (s *session) readLoop() error {
	for {
		typ, payload, err := s.conn.Read(time.Now().Add(50 * time.Second))
		if err != nil {
			return err
		}
		switch typ {
		case proto.TypePing:
			if err := s.conn.Write(proto.TypePong, nil); err != nil {
				return err
			}
		case proto.TypePong:
		case proto.TypeIndex:
			s.onIndex(payload)
		case proto.TypeMetaReq:
			// Answer off the read loop. A multi-megabyte block list must not
			// stall the frames of a file that is already streaming.
			body := append([]byte(nil), payload...)
			go s.onMetaReq(body)
		case proto.TypeMeta:
			s.onMeta(payload)
		case proto.TypeBlockReq:
			s.onBlockReq(payload)
		case proto.TypeBlock:
			s.onBlock(payload)
		case proto.TypeBlobReq:
			body := append([]byte(nil), payload...)
			s.onBlobReq(body)
		case proto.TypeBlob:
			s.onBlob(payload)
		}
	}
}

func (s *session) pingLoop() error {
	// Keepalive stays off the reconcile loop. A streamed transfer does not
	// send per-block requests, so without this the sender's read would time
	// out while a large file is still moving.
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-ping.C:
			if err := s.conn.Write(proto.TypePing, nil); err != nil {
				return err
			}
		}
	}
}

func (s *session) syncLoop(sub <-chan struct{}) error {
	var remote []proto.Entry
	var have bool
	// nil once the first readable scan has finished. Until then the index
	// may still be the previous run's tombstones, and announcing it would
	// delete the peer's copies.
	ready := s.e.Ready()
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-ready:
			ready = nil
			if err := s.sendIndex(); err != nil {
				return err
			}
			if have {
				s.reconcile(remote)
			}
		case <-sub:
			if ready != nil {
				continue
			}
			if err := s.sendIndex(); err != nil {
				return err
			}
			if have {
				s.reconcile(remote)
			}
		case entries := <-s.indexReady:
			remote = entries
			have = true
			if ready != nil {
				continue
			}
			s.reconcile(remote)
		}
	}
}

func (s *session) sendIndex() error {
	_, entries := s.e.Entries()
	const pageSize = 40
	total := (len(entries) + pageSize - 1) / pageSize
	if total == 0 {
		total = 1
	}
	seq := uint64(time.Now().UnixNano())
	for part := 0; part < total; part++ {
		var chunk []proto.Entry
		if len(entries) > 0 {
			from := part * pageSize
			to := from + pageSize
			if to > len(entries) {
				to = len(entries)
			}
			chunk = entries[from:to]
		}
		body, err := json.Marshal(proto.IndexPage{Seq: seq, Part: part, Total: total, Entries: chunk})
		if err != nil {
			return err
		}
		if err := s.conn.Write(proto.TypeIndex, body); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) onIndex(payload []byte) {
	var page proto.IndexPage
	if err := json.Unmarshal(payload, &page); err != nil || page.Total <= 0 || page.Total > 100000 || page.Part < 0 || page.Part >= page.Total {
		return
	}
	s.asmMu.Lock()
	defer s.asmMu.Unlock()
	if s.asm.seq != page.Seq || s.asm.total != page.Total || s.asm.parts == nil {
		s.asm = indexAsm{seq: page.Seq, total: page.Total, parts: map[int][]proto.Entry{}}
	}
	s.asm.parts[page.Part] = page.Entries
	if len(s.asm.parts) < page.Total {
		return
	}
	all := make([]proto.Entry, 0)
	for i := 0; i < page.Total; i++ {
		part, ok := s.asm.parts[i]
		if !ok {
			return
		}
		all = append(all, part...)
	}
	s.asm.parts = nil
	select {
	case s.indexReady <- all:
	default:
		select {
		case <-s.indexReady:
		default:
		}
		select {
		case s.indexReady <- all:
		default:
		}
	}
}

func (s *session) reconcile(remote []proto.Entry) {
	tasks := s.e.Plan(remote)
	s.e.BeginBatch()
	defer s.e.EndBatch()
	var pulls []Task
	for _, task := range tasks {
		if s.ctx.Err() != nil {
			return
		}
		switch task.Op {
		case opPull:
			pulls = append(pulls, task)
			continue
		case opConflict:
			err := s.e.StageConflict(task)
			if err != nil {
				if !errors.Is(err, errStale) {
					s.log.Warn("sync step failed", "op", task.Op, "path", task.Path, "err", err)
				}
				s.done(task, err)
				continue
			}
			pulls = append(pulls, task)
			continue
		}
		var err error
		switch task.Op {
		case opDelete:
			err = s.e.ApplyDelete(task)
		case opMerge:
			err = s.e.ApplyMerge(task)
		case opDir:
			err = s.e.ApplyDir(task)
		}
		if err != nil && !errors.Is(err, errStale) {
			s.log.Warn("sync step failed", "op", task.Op, "path", task.Path, "err", err)
		}
		s.done(task, err)
	}
	s.pullAll(pulls)
}

func (s *session) done(task Task, err error) {
	if s.note == nil || errors.Is(err, errStale) {
		return
	}
	if err != nil {
		s.note(Note{Op: "fail", Path: task.Path, Detail: err.Error()})
		return
	}
	if task.Op == opMerge {
		return
	}
	s.note(Note{Op: task.Op, Path: task.Path})
}

func (s *session) onMetaReq(payload []byte) {
	var req proto.MetaReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return
	}
	sum, blocks, _, err := s.e.Meta(req.Path)
	if err != nil {
		s.sendMeta(proto.MetaChunk{Total: 0, Meta: proto.Meta{ReqID: req.ReqID, Path: req.Path, Err: "not available"}})
		return
	}
	if s.note != nil {
		s.note(Note{Op: "send", Path: req.Path})
	}
	const chunk = 200
	if len(blocks) == 0 {
		s.sendMeta(proto.MetaChunk{Offset: 0, Total: 0, Meta: proto.Meta{ReqID: req.ReqID, Path: req.Path, Sum: sum}})
		return
	}
	for off := 0; off < len(blocks); off += chunk {
		end := off + chunk
		if end > len(blocks) {
			end = len(blocks)
		}
		s.sendMeta(proto.MetaChunk{
			Offset: off,
			Total:  len(blocks),
			Meta:   proto.Meta{ReqID: req.ReqID, Path: req.Path, Sum: sum, Blocks: blocks[off:end]},
		})
	}
}

func (s *session) sendMeta(c proto.MetaChunk) {
	body, err := proto.EncodeMetaChunk(c)
	if err != nil {
		s.log.Debug("meta encode", "err", err)
		return
	}
	_ = s.conn.Write(proto.TypeMeta, body)
}

func (s *session) onBlockReq(payload []byte) {
	var req proto.BlockReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return
	}
	data, err := s.e.ReadBlock(req.Path, req.Index)
	var text string
	if err != nil {
		text = "not available"
		data = nil
	}
	_ = s.conn.Write(proto.TypeBlock, proto.EncodeBlock(req.ReqID, req.Index, data, text))
}

func (s *session) onMeta(payload []byte) {
	chunk, err := proto.DecodeMetaChunk(payload)
	if err != nil {
		return
	}
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	w, ok := s.pending[chunk.ReqID]
	if !ok || w.kind != proto.TypeMeta || w.meta == nil {
		return
	}
	if w.asm == nil {
		w.asm = &metaAsm{blocks: map[int][32]byte{}}
	}
	if chunk.Err != "" {
		select {
		case w.meta <- proto.Meta{ReqID: chunk.ReqID, Path: chunk.Path, Err: chunk.Err}:
		default:
		}
		return
	}
	w.asm.total = chunk.Total
	w.asm.sum = chunk.Sum
	w.asm.path = chunk.Path
	for i, b := range chunk.Blocks {
		w.asm.blocks[chunk.Offset+i] = b
	}
	if chunk.Total == 0 || len(w.asm.blocks) >= chunk.Total {
		blocks := make([][32]byte, chunk.Total)
		for i := 0; i < chunk.Total; i++ {
			b, ok := w.asm.blocks[i]
			if !ok {
				return
			}
			blocks[i] = b
		}
		select {
		case w.meta <- proto.Meta{ReqID: chunk.ReqID, Path: chunk.Path, Sum: chunk.Sum, Blocks: blocks}:
		default:
		}
	}
}

type metaAsm struct {
	total  int
	sum    [32]byte
	path   string
	blocks map[int][32]byte
}

func (s *session) onBlobReq(payload []byte) {
	req, err := proto.DecodeBlobReq(payload)
	if err != nil {
		return
	}
	select {
	case s.blobSem <- struct{}{}:
	default:
		_ = s.conn.Write(proto.TypeBlob, proto.EncodeBlobErr(req.ReqID, "busy"))
		return
	}
	// Streaming from the read loop deadlocks when both peers push a large
	// file at once: neither side reads until its write finishes.
	go s.serveBlob(req)
}

func (s *session) onBlob(payload []byte) {
	bl, err := proto.DecodeBlob(payload)
	if err != nil {
		return
	}
	s.pendMu.Lock()
	w := s.pending[bl.ReqID]
	s.pendMu.Unlock()
	if w == nil || w.kind != proto.TypeBlob || w.blobs == nil {
		return
	}
	piece := blobPiece{kind: bl.Kind, index: bl.Index, count: bl.Count, data: bl.Data}
	switch bl.Kind {
	case proto.BlobEOF:
		piece.eof = true
	case proto.BlobError:
		piece.err = errors.New(bl.Err)
	}
	select {
	case w.blobs <- piece:
	case <-w.done:
	case <-s.ctx.Done():
	}
}

func (s *session) onBlock(payload []byte) {
	reqID, _, data, errText, err := proto.DecodeBlock(payload)
	if err != nil {
		return
	}
	s.pendMu.Lock()
	w, ok := s.pending[reqID]
	s.pendMu.Unlock()
	if !ok || w.kind != proto.TypeBlock {
		return
	}
	res := blockResult{data: data}
	if errText != "" {
		res.err = errors.New(errText)
	}
	select {
	case w.data <- res:
	default:
	}
}

func (s *session) awaitMeta(id uint32, typ byte, body []byte, timeout time.Duration) (proto.Meta, error) {
	ch := make(chan proto.Meta, 1)
	s.pendMu.Lock()
	s.pending[id] = &waiter{kind: proto.TypeMeta, meta: ch}
	s.pendMu.Unlock()
	defer s.forget(id)
	if err := s.conn.Write(typ, body); err != nil {
		return proto.Meta{}, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return proto.Meta{}, s.ctx.Err()
	case <-timer.C:
		return proto.Meta{}, errors.New("metadata timed out")
	case m := <-ch:
		return m, nil
	}
}

func (s *session) awaitBlock(id uint32, body []byte, timeout time.Duration) ([]byte, error) {
	ch := make(chan blockResult, 1)
	s.pendMu.Lock()
	s.pending[id] = &waiter{kind: proto.TypeBlock, data: ch}
	s.pendMu.Unlock()
	defer s.forget(id)
	if err := s.conn.Write(proto.TypeBlockReq, body); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-timer.C:
		return nil, errors.New("block timed out")
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		return res.data, nil
	}
}

func (s *session) forget(id uint32) {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	w := s.pending[id]
	delete(s.pending, id)
	if w != nil && w.done != nil {
		close(w.done)
	}
}

func (s *session) failAll(err error) {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	for id, w := range s.pending {
		if w.meta != nil {
			select {
			case w.meta <- proto.Meta{Err: err.Error()}:
			default:
			}
		}
		if w.data != nil {
			select {
			case w.data <- blockResult{err: err}:
			default:
			}
		}
		if w.blobs != nil {
			select {
			case w.blobs <- blobPiece{err: err}:
			default:
			}
		}
		if w.done != nil {
			close(w.done)
		}
		delete(s.pending, id)
	}
}

func (s *session) allocID() uint32 {
	return s.next.Add(1)
}
