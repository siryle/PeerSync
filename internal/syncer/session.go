package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/frame"
	"github.com/netcraft/peersync/internal/proto"
)

// Serve runs directory sync on an already authenticated connection until it
// fails or ctx is cancelled.
func Serve(ctx context.Context, e *Engine, conn *frame.Conn, log *slog.Logger, note func(Note), peerID ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
		indexReady: make(chan indexUpdate, 1),
		blobSem:    make(chan struct{}, 16),
	}
	if len(peerID) > 0 {
		s.peerID = peerID[0]
	}
	sub, unsub := e.Subscribe()
	defer unsub()
	errc := make(chan error, 3)
	var loops sync.WaitGroup
	for _, run := range []func() error{s.readLoop, func() error { return s.syncLoop(sub) }, s.pingLoop} {
		loops.Go(func() { errc <- run() })
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errc:
	}
	cancel()
	_ = conn.Close()
	s.failAll(errors.New("connection closed"))
	loops.Wait()
	s.work.Wait()
	return err
}

type waiter struct {
	kind  byte
	meta  chan proto.Meta
	blobs chan blobPiece
	done  chan struct{}
	asm   *metaAsm
}

type session struct {
	ctx        context.Context
	e          *Engine
	conn       *frame.Conn
	log        *slog.Logger
	note       func(Note)
	next       atomic.Uint32
	indexReady chan indexUpdate
	peerID     string
	blobSem    chan struct{}
	retry      atomic.Bool
	work       sync.WaitGroup

	pendMu  sync.Mutex
	pending map[uint32]*waiter

	asmMu sync.Mutex
	asm   indexAsm
}

type indexAsm struct {
	seq   uint64
	total int
	parts map[int][]proto.Entry
	mode  config.SyncMode
}

type indexUpdate struct {
	entries []proto.Entry
	mode    config.SyncMode
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
			s.work.Go(func() { s.onMetaReq(body) })
		case proto.TypeMeta:
			s.onMeta(payload)
		case proto.TypeBlockReq:
			s.onBlockReq(payload)
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
	var mode config.SyncMode
	var have bool
	var sourceWarned bool
	reconcile := func() {
		if s.e.Mode == config.ReceiveOnly {
			if s.peerID != s.e.SourceID {
				return
			}
			if mode != config.SendOnly {
				if !sourceWarned && s.note != nil {
					s.note(Note{Op: "fail", Path: "同步模式", Detail: "来源设备需要设为仅发送，暂未接收文件"})
				}
				sourceWarned = true
				return
			}
			sourceWarned = false
		}
		s.reconcile(remote)
	}
	// nil once the first readable scan has finished. Until then the index
	// may still be the previous run's tombstones, and announcing it would
	// delete the peer's copies.
	ready := s.e.Ready()
	retry := time.NewTicker(2 * time.Second)
	defer retry.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-retry.C:
			if ready == nil && have && s.retry.Swap(false) {
				reconcile()
			}
		case <-ready:
			ready = nil
			if err := s.sendIndex(); err != nil {
				return err
			}
			if have {
				reconcile()
			}
		case <-sub:
			if ready != nil {
				continue
			}
			if err := s.sendIndex(); err != nil {
				return err
			}
			if have {
				reconcile()
			}
		case update := <-s.indexReady:
			remote, mode = update.entries, update.mode
			have = true
			if ready != nil {
				continue
			}
			reconcile()
		}
	}
}

func (s *session) sendIndex() error {
	var entries []proto.Entry
	if s.e.Mode != config.ReceiveOnly {
		_, entries = s.e.Entries()
	}
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
		body, err := json.Marshal(proto.IndexPage{Seq: seq, Part: part, Total: total, Entries: chunk, Mode: string(s.e.Mode)})
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
		s.asm = indexAsm{seq: page.Seq, total: page.Total, parts: map[int][]proto.Entry{}, mode: config.SyncMode(page.Mode).Effective()}
	}
	if s.asm.mode != config.SyncMode(page.Mode).Effective() {
		return
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
	case s.indexReady <- indexUpdate{entries: all, mode: s.asm.mode}:
	default:
		select {
		case <-s.indexReady:
		default:
		}
		select {
		case s.indexReady <- indexUpdate{entries: all, mode: s.asm.mode}:
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
		case opPull, opConflict:
			if task.Remote.IsDir {
				break
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
		case opDir, opConflict:
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
	if errors.Is(err, errIgnored) {
		return
	}
	if err != nil {
		s.retry.Store(true)
	}
	if s.note == nil || errors.Is(err, errStale) {
		return
	}
	if err != nil {
		s.note(Note{Op: "fail", Path: task.Path, Detail: err.Error()})
		return
	}
	if task.Backup {
		s.note(Note{Op: "backup", Path: task.Path})
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
	s.work.Go(func() { s.serveBlob(req) })
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
