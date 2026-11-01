package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/netcraft/peersync/internal/ignore"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

// errStale means the file changed after the sync plan was made.
var errStale = errors.New("local file changed while syncing")

// File is one path in the local index.
type File struct {
	Size    int64         `json:"size,omitempty"`
	ModTime int64         `json:"mod_time,omitempty"`
	Mode    uint32        `json:"mode,omitempty"`
	IsDir   bool          `json:"dir,omitempty"`
	Deleted bool          `json:"deleted,omitempty"`
	Sum     [32]byte      `json:"sum,omitempty"`
	Blocks  [][32]byte    `json:"blocks,omitempty"`
	Version vclock.Vector `json:"version"`
}

type indexFile struct {
	Seq   uint64           `json:"seq"`
	Files map[string]*File `json:"files"`
}

// Engine tracks one shared directory and applies peer updates.
type Engine struct {
	Root      string
	IndexPath string
	Self      string
	Log       *slog.Logger
	root      *os.Root

	mu       sync.Mutex
	idx      indexFile
	inflight map[string]int
	subs     []chan struct{}
	kick     chan struct{}
	// batch coalesces index writes. A reconcile used to rewrite the whole
	// index once per file, which dominated syncing a large tree.
	batch int
	dirty bool

	ready     chan struct{}
	readyOnce sync.Once
}

// Open loads an index. A missing or corrupt index starts empty; the next
// scan treats every file as a local edit, which converges with identical
// copies instead of deleting them.
func Open(root, indexPath, self string, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.Default()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	dir, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		Root:      abs,
		root:      dir,
		IndexPath: indexPath,
		Self:      self,
		Log:       log,
		inflight:  map[string]int{},
		kick:      make(chan struct{}, 1),
		idx:       indexFile{Files: map[string]*File{}},
		ready:     make(chan struct{}),
	}
	if err := e.load(); err != nil {
		e.Log.Warn("index unreadable, scanning as a new folder", "err", err, "path", indexPath)
		e.idx = indexFile{Files: map[string]*File{}}
	}
	_ = dir.RemoveAll(tmpDirName)
	return e, nil
}

// Close releases the shared directory handle after its workers have stopped.
func (e *Engine) Close() error { return e.root.Close() }

func (e *Engine) load() error {
	b, err := os.ReadFile(e.IndexPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var idx indexFile
	if err := json.Unmarshal(b, &idx); err != nil {
		return err
	}
	if idx.Files == nil {
		idx.Files = map[string]*File{}
	}
	e.idx = idx
	return nil
}

// Ready is closed after the first scan that could read the share root.
// Peers wait for it so a stale tombstone index is not announced first.
func (e *Engine) Ready() <-chan struct{} {
	return e.ready
}

func (e *Engine) markReady() {
	e.readyOnce.Do(func() { close(e.ready) })
}

// Kick asks the run loop to scan soon.
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Subscribe reports local index changes. The channel has buffer 1.
func (e *Engine) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	e.mu.Lock()
	e.subs = append(e.subs, ch)
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		for i, sub := range e.subs {
			if sub == ch {
				e.subs = append(e.subs[:i], e.subs[i+1:]...)
				break
			}
		}
	}
}

func (e *Engine) notify() {
	for _, ch := range e.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Run scans on demand and on a timer until ctx is cancelled.
func (e *Engine) Run(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		every = 20 * time.Second
	}
	if err := e.Scan(); err != nil {
		e.Log.Warn("scan", "err", err, "root", e.Root)
	}
	verified := time.Now()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	e.watchAll(watcher)

	timer := time.NewTimer(every)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-e.kick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(300 * time.Millisecond)
		case _, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			e.Kick()
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			e.Log.Debug("watch", "err", err)
		case <-timer.C:
			// Periodically verify bytes even when a writer preserved size and mtime.
			full := time.Since(verified) >= 5*time.Minute
			if err := e.scan(full); err != nil {
				e.Log.Warn("scan", "err", err, "root", e.Root)
			} else if full {
				verified = time.Now()
			}
			e.watchAll(watcher)
			timer.Reset(every)
		}
	}
}

func (e *Engine) watchAll(w *fsnotify.Watcher) {
	_ = w.Add(e.Root)
	// kqueue opens one descriptor per file inside each watched directory.
	// A large tree (this share has held ~200k files) exhausts the process
	// limit. The next scan then cannot read the folder; treating that as
	// deletion wipes the index and the peer. Nested edits are picked up
	// by the periodic scan instead.
	if kqueueOS() {
		return
	}
	ig := e.ignores()
	_ = filepath.WalkDir(e.Root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || !d.IsDir() || abs == e.Root {
			return nil
		}
		rel, err := relOf(e.Root, abs)
		if err != nil || ig.Match(rel, true) {
			return filepath.SkipDir
		}
		_ = w.Add(abs)
		return nil
	})
}

func kqueueOS() bool {
	switch runtime.GOOS {
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		return true
	default:
		return false
	}
}

// Scan refreshes the index and verifies all file contents.
func (e *Engine) Scan() error {
	return e.scan(true)
}

func (e *Engine) scan(full bool) error {
	ig := e.ignores()
	type item struct {
		rel   string
		size  int64
		mtime int64
		mode  uint32
		dir   bool
	}
	var items []item
	// Paths under a directory that could not be read. They must not be
	// tombstoned: a failed listing is not evidence that the files are gone.
	var unread []string
	var rootErr error
	err := fs.WalkDir(e.root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			if rel == "." {
				rootErr = err
				return filepath.SkipAll
			}
			if ValidRel(rel) {
				unread = append(unread, rel)
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "." {
			return nil
		}
		if d == nil {
			return nil
		}
		if !ValidRel(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ig.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			unread = append(unread, rel)
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		items = append(items, item{
			rel:   rel,
			size:  info.Size(),
			mtime: info.ModTime().UnixNano(),
			mode:  uint32(info.Mode().Perm()),
			dir:   info.IsDir(),
		})
		return nil
	})
	if rootErr != nil {
		return fmt.Errorf("scan %s: %w", e.Root, rootErr)
	}
	if err != nil {
		return err
	}

	e.mu.Lock()
	snap := make(map[string]*File, len(e.idx.Files))
	for k, v := range e.idx.Files {
		cp := *v
		cp.Version = vclock.Clone(v.Version)
		snap[k] = &cp
	}
	inflight := make(map[string]int, len(e.inflight))
	for k, v := range e.inflight {
		inflight[k] = v
	}
	e.mu.Unlock()

	type hashed struct {
		sum    [32]byte
		blocks [][32]byte
		size   int64
	}
	hashes := map[string]hashed{}
	for _, it := range items {
		if it.dir || inflight[it.rel] > 0 {
			continue
		}
		prev := snap[it.rel]
		if !full && prev != nil && !prev.Deleted && !prev.IsDir && prev.Size == it.size && prev.ModTime == it.mtime && len(prev.Blocks) == BlocksForSize(it.size) {
			continue
		}
		sum, blocks, size, err := e.hashFile(it.rel)
		if err != nil {
			e.Log.Debug("hash", "path", it.rel, "err", err)
			continue
		}
		hashes[it.rel] = hashed{sum: sum, blocks: blocks, size: size}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	seen := map[string]bool{}
	for _, it := range items {
		if e.inflight[it.rel] > 0 {
			seen[it.rel] = true
			continue
		}
		seen[it.rel] = true
		if cur, prev := e.idx.Files[it.rel], snap[it.rel]; (cur == nil) != (prev == nil) || (cur != nil && vclock.Compare(cur.Version, prev.Version) != 0) {
			e.Kick()
			continue
		}
		if it.dir {
			if e.upsertDir(it.rel, it.mode) {
				changed = true
			}
			continue
		}
		if h, ok := hashes[it.rel]; ok {
			if e.upsertFile(it.rel, h.size, it.mtime, it.mode, h.sum, h.blocks) {
				changed = true
			}
			continue
		}
		prev := e.idx.Files[it.rel]
		if prev != nil && !prev.Deleted && prev.Mode != it.mode {
			prev.Mode = it.mode
			prev.Version = vclock.Bump(prev.Version, e.Self)
			changed = true
		}
	}
	for rel, f := range e.idx.Files {
		if seen[rel] || f.Deleted || e.inflight[rel] > 0 || unreadCovers(unread, rel) {
			continue
		}
		if ig.Match(rel, f.IsDir) {
			delete(e.idx.Files, rel)
			changed = true
			continue
		}
		f.Deleted = true
		f.Blocks = nil
		f.Sum = [32]byte{}
		f.Size = 0
		f.Version = vclock.Bump(f.Version, e.Self)
		changed = true
	}
	if changed {
		e.idx.Seq++
		if err := e.persistLocked(); err != nil {
			return err
		}
	}
	e.markReady()
	if len(unread) > 0 {
		return fmt.Errorf("scan incomplete, left %s unchanged", unread[0])
	}
	return nil
}

// unreadCovers reports whether rel is a path the walk could not list.
func unreadCovers(unread []string, rel string) bool {
	for _, u := range unread {
		if rel == u || strings.HasPrefix(rel, u+"/") {
			return true
		}
	}
	return false
}

func (e *Engine) ignores() ignore.Set {
	s := ignore.Defaults()
	f, err := e.openFile(".peersyncignore")
	if err != nil {
		return s
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return s
	}
	return s.Merge(ignore.Parse(string(b)))
}

func (e *Engine) upsertDir(rel string, mode uint32) bool {
	prev := e.idx.Files[rel]
	if prev != nil && !prev.Deleted && prev.IsDir {
		if prev.Mode == mode {
			return false
		}
		prev.Mode = mode
		prev.Version = vclock.Bump(prev.Version, e.Self)
		return true
	}
	ver := vclock.Vector{}
	if prev != nil {
		ver = prev.Version
	}
	e.idx.Files[rel] = &File{IsDir: true, Mode: mode, Version: vclock.Bump(ver, e.Self)}
	return true
}

func (e *Engine) upsertFile(rel string, size, mtime int64, mode uint32, sum [32]byte, blocks [][32]byte) bool {
	prev := e.idx.Files[rel]
	if prev != nil && !prev.Deleted && !prev.IsDir && prev.Sum == sum && prev.ModTime == mtime && prev.Mode == mode {
		prev.Size = size
		prev.ModTime = mtime
		prev.Mode = mode
		prev.Blocks = blocks
		return false
	}
	ver := vclock.Vector{}
	if prev != nil {
		ver = prev.Version
	}
	e.idx.Files[rel] = &File{
		Size: size, ModTime: mtime, Mode: mode,
		Sum: sum, Blocks: blocks, Version: vclock.Bump(ver, e.Self),
	}
	return true
}

// Entries is a snapshot of the index for announcement to peers.
func (e *Engine) Entries() (uint64, []proto.Entry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]proto.Entry, 0, len(e.idx.Files))
	for p, f := range e.idx.Files {
		out = append(out, proto.Entry{
			Path:    p,
			Size:    f.Size,
			ModTime: f.ModTime,
			Mode:    f.Mode,
			IsDir:   f.IsDir,
			Deleted: f.Deleted,
			Sum:     proto.EncodeSum(f.Sum, !f.Deleted && !f.IsDir),
			Version: vclock.Clone(f.Version),
		})
	}
	return e.idx.Seq, out
}

// Plan builds work items for a remote index.
func (e *Engine) Plan(remote []proto.Entry) []Task {
	ig := e.ignores()
	e.mu.Lock()
	defer e.mu.Unlock()
	filtered := make([]proto.Entry, 0, len(remote))
	for _, entry := range remote {
		cur := e.idx.Files[entry.Path]
		if !ig.Match(entry.Path, entry.IsDir || (cur != nil && cur.IsDir)) {
			filtered = append(filtered, entry)
		}
	}
	local := make(map[string]*File, len(e.idx.Files))
	for k, v := range e.idx.Files {
		cp := *v
		cp.Version = vclock.Clone(v.Version)
		cp.Blocks = nil
		local[k] = &cp
	}
	return Plan(e.Self, local, filtered)
}

// Meta returns the block list of a non-deleted file.
func (e *Engine) Meta(rel string) (sum [32]byte, blocks [][32]byte, size int64, err error) {
	if !ValidRel(rel) {
		return sum, nil, 0, errors.New("bad path")
	}
	if e.ignores().Match(rel, false) {
		return sum, nil, 0, os.ErrNotExist
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.idx.Files[rel]
	if f == nil || f.Deleted || f.IsDir {
		return sum, nil, 0, os.ErrNotExist
	}
	blocks = append([][32]byte(nil), f.Blocks...)
	return f.Sum, blocks, f.Size, nil
}

// ReadBlock returns one chunk of a file that still matches the index.
func (e *Engine) ReadBlock(rel string, index int) ([]byte, error) {
	sum, blocks, size, err := e.Meta(rel)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(blocks) {
		return nil, errors.New("block out of range")
	}
	f, err := e.openFile(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := ReadBlock(f, index, size)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(data) != blocks[index] {
		return nil, errors.New("file changed")
	}
	if SumBlocks(blocks) != sum {
		return nil, errors.New("index checksum mismatch")
	}
	return data, nil
}

// Begin marks a path so a scan does not tombstone it while it is downloading.
func (e *Engine) Begin(rel string) {
	e.mu.Lock()
	e.inflight[rel]++
	e.mu.Unlock()
}

// End clears Begin.
func (e *Engine) End(rel string) {
	e.mu.Lock()
	e.inflight[rel]--
	if e.inflight[rel] <= 0 {
		delete(e.inflight, rel)
	}
	e.mu.Unlock()
}

// ApplyDelete records a remote deletion only after removing the disk entry.
func (e *Engine) ApplyDelete(task Task) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkLocalLocked(task); err != nil {
		return err
	}
	if err := e.root.Remove(task.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	e.idx.Files[task.Path] = &File{Deleted: true, IsDir: task.Remote.IsDir, Version: vclock.Clone(task.Version)}
	e.idx.Seq++
	return e.persistLocked()
}

// ApplyMerge stores a version that already describes the bytes on disk.
func (e *Engine) ApplyMerge(task Task) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkLocalLocked(task); err != nil {
		return err
	}
	cur := e.idx.Files[task.Path]
	if cur == nil {
		return nil
	}
	if task.Metadata {
		info, err := e.setMetadata(task.Path, task.Remote)
		if err != nil {
			return err
		}
		cur.Mode = uint32(info.Mode().Perm())
		if !cur.IsDir {
			cur.ModTime = info.ModTime().UnixNano()
		}
	}
	cur.Version = vclock.Clone(task.Version)
	e.idx.Seq++
	return e.persistLocked()
}

// ApplyDir creates a directory from a remote index entry.
func (e *Engine) ApplyDir(task Task) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkLocalLocked(task); err != nil {
		return err
	}
	if cur := e.idx.Files[task.Path]; cur != nil && !cur.Deleted && !cur.IsDir {
		if err := e.stageConflictLocked(task); err != nil {
			return err
		}
		if err := e.root.Remove(task.Path); err != nil {
			return err
		}
	}
	if err := e.root.MkdirAll(task.Path, fileMode(task.Remote.Mode, true)); err != nil {
		return err
	}
	info, err := e.setMetadata(task.Path, task.Remote)
	if err != nil {
		return err
	}
	e.idx.Files[task.Path] = &File{IsDir: true, Mode: uint32(info.Mode().Perm()), Version: vclock.Clone(task.Version)}
	e.idx.Seq++
	return e.persistLocked()
}

// stageConflictLocked saves a verified local version only when its replacement is ready.
func (e *Engine) stageConflictLocked(task Task) error {
	cur := e.idx.Files[task.Path]
	if cur == nil || cur.Deleted || cur.IsDir {
		return errStale
	}
	// A retry must find the same backup instead of producing another conflict.
	base := conflictName(task.Path, cur)
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			ext := path.Ext(base)
			name = strings.TrimSuffix(base, ext) + "-" + strconv.Itoa(i) + ext
		}
		if !ValidRel(name) {
			return errors.New("conflict path too long")
		}
		out, err := e.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, fileMode(cur.Mode, false))
		if errors.Is(err, os.ErrExist) {
			sum, _, _, hashErr := e.hashFile(name)
			if hashErr == nil && sum == cur.Sum {
				return nil
			}
			continue
		}
		if err != nil {
			return err
		}
		in, err := e.openFile(task.Path)
		if err == nil {
			_, err = io.Copy(out, in)
			_ = in.Close()
		}
		if err == nil {
			err = out.Sync()
		}
		if err == nil {
			_, err = out.Seek(0, io.SeekStart)
		}
		var sum [32]byte
		var blocks [][32]byte
		var size int64
		if err == nil {
			sum, blocks, size, err = HashReader(out)
		}
		closeErr := out.Close()
		if err == nil {
			err = closeErr
		}
		var info os.FileInfo
		if err == nil {
			info, err = e.setMetadata(name, proto.Entry{Mode: cur.Mode, ModTime: cur.ModTime})
		}
		if err == nil && (sum != cur.Sum || size != cur.Size) {
			err = errStale
		}
		if err != nil {
			_ = e.root.Remove(name)
			return err
		}
		e.idx.Files[name] = &File{
			Size: size, ModTime: info.ModTime().UnixNano(), Mode: cur.Mode,
			Sum: sum, Blocks: blocks, Version: vclock.Bump(cur.Version, e.Self),
		}
		e.idx.Seq++
		return e.persistLocked()
	}
	return errors.New("could not pick a conflict name")
}

// CommitDownload checks local changes before replacing any bytes on disk.
func (e *Engine) CommitDownload(task Task, tmp string, blocks [][32]byte) error {
	sum := SumBlocks(blocks)
	want, ok := proto.DecodeSum(task.Remote.Sum)
	if !ok || want != sum {
		return errors.New("downloaded checksum mismatch")
	}
	if task.Remote.Size < 0 || BlocksForSize(task.Remote.Size) != len(blocks) {
		return errors.New("downloaded size mismatch")
	}
	if filepath.IsAbs(tmp) {
		var err error
		tmp, err = filepath.Rel(e.Root, tmp)
		if err != nil {
			return err
		}
	}
	if !filepath.IsLocal(tmp) {
		return errors.New("bad temporary path")
	}
	tmp = filepath.ToSlash(tmp)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkLocalLocked(task); err != nil {
		return err
	}
	if task.Op == opConflict {
		if err := e.stageConflictLocked(task); err != nil {
			return err
		}
	}
	if err := e.root.MkdirAll(path.Dir(task.Path), 0o755); err != nil {
		return err
	}
	info, err := e.setMetadata(tmp, task.Remote)
	if err != nil {
		return err
	}
	cur := e.idx.Files[task.Path]
	replacingDir := cur != nil && !cur.Deleted && cur.IsDir
	if replacingDir {
		// Remove only an empty directory; unsynced children must survive.
		if err := e.root.Remove(task.Path); err != nil {
			return err
		}
	}
	if err := e.root.Rename(tmp, task.Path); err != nil {
		if replacingDir {
			return errors.Join(err, e.root.Mkdir(task.Path, fileMode(cur.Mode, true)))
		}
		return err
	}
	e.idx.Files[task.Path] = &File{
		Size: task.Remote.Size, ModTime: info.ModTime().UnixNano(), Mode: uint32(info.Mode().Perm()),
		Sum: sum, Blocks: blocks, Version: vclock.Clone(task.Version),
	}
	e.idx.Seq++
	return e.persistLocked()
}

// Stats counts live files and directories.
func (e *Engine) Stats() (files int, seq uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.idx.Files {
		if !f.Deleted {
			files++
		}
	}
	return files, e.idx.Seq
}

func unchanged(cur *File, task Task) bool {
	if !task.Had {
		return cur == nil || (cur.Deleted && vclock.Compare(cur.Version, task.Seen) == 0)
	}
	if cur == nil {
		return false
	}
	return vclock.Compare(cur.Version, task.Seen) == 0
}

// BeginBatch defers index writes until EndBatch. Calls nest.
func (e *Engine) BeginBatch() {
	e.mu.Lock()
	e.batch++
	e.mu.Unlock()
}

// EndBatch writes the index once if a batched update changed it.
func (e *Engine) EndBatch() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.batch == 0 {
		return
	}
	e.batch--
	if e.batch > 0 || !e.dirty {
		return
	}
	e.dirty = false
	if err := e.saveLocked(); err != nil {
		e.dirty = true
		e.Log.Warn("save index", "err", err)
		return
	}
	e.notify()
}

// persistLocked writes the index unless a batch is open.
func (e *Engine) persistLocked() error {
	if e.batch > 0 {
		e.dirty = true
		return nil
	}
	if err := e.saveLocked(); err != nil {
		return err
	}
	e.notify()
	return nil
}

func (e *Engine) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(e.IndexPath), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(e.idx)
	if err != nil {
		return err
	}
	tmp := e.IndexPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.IndexPath)
}

func relOf(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if !ValidRel(rel) {
		return "", errors.New("ignored path")
	}
	return rel, nil
}

func fileMode(m uint32, dir bool) os.FileMode {
	perm := os.FileMode(m) & 0o777
	if perm == 0 {
		if dir {
			return 0o755
		}
		return 0o644
	}
	return perm
}

func conflictName(rel string, f *File) string {
	ext := path.Ext(rel)
	base := rel[:len(rel)-len(ext)]
	// JSON sorts vector keys, so every replica names the same losing version alike.
	version, _ := json.Marshal(f.Version)
	digest := sha256.Sum256(append(f.Sum[:], version...))
	return base + fmt.Sprintf(".sync-conflict-%x", digest[:16]) + ext
}
