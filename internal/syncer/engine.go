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
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("share path is not a directory")
	}
	e := &Engine{
		Root:      root,
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
	_ = os.RemoveAll(filepath.Join(root, tmpDirName))
	return e, nil
}

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
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if rel, err := relOf(e.Root, ev.Name); err == nil {
				_ = rel
			}
			e.Kick()
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			e.Log.Debug("watch", "err", err)
		case <-timer.C:
			if err := e.Scan(); err != nil {
				e.Log.Warn("scan", "err", err, "root", e.Root)
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

// Scan refreshes the index from disk.
func (e *Engine) Scan() error {
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
	err := filepath.WalkDir(e.Root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			if abs == e.Root {
				rootErr = err
				return filepath.SkipAll
			}
			if rel, relErr := relOf(e.Root, abs); relErr == nil {
				unread = append(unread, rel)
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if abs == e.Root {
			return nil
		}
		if d == nil {
			return nil
		}
		rel, err := relOf(e.Root, abs)
		if err != nil {
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
		snap[k] = v
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
		if prev != nil && !prev.Deleted && !prev.IsDir && prev.Size == it.size && prev.ModTime == it.mtime && len(prev.Blocks) == BlocksForSize(it.size) {
			continue
		}
		sum, blocks, size, err := HashFile(filepath.Join(e.Root, filepath.FromSlash(it.rel)))
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
	b, err := os.ReadFile(filepath.Join(e.Root, ".peersyncignore"))
	if err != nil {
		return s
	}
	return s.Merge(ignore.Parse(string(b)))
}

func (e *Engine) upsertDir(rel string, mode uint32) bool {
	prev := e.idx.Files[rel]
	if prev != nil && !prev.Deleted && prev.IsDir {
		prev.Mode = mode
		return false
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
	if prev != nil && !prev.Deleted && !prev.IsDir && prev.Sum == sum {
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
	e.mu.Lock()
	defer e.mu.Unlock()
	local := make(map[string]*File, len(e.idx.Files))
	for k, v := range e.idx.Files {
		cp := *v
		cp.Version = vclock.Clone(v.Version)
		cp.Blocks = nil
		local[k] = &cp
	}
	return Plan(e.Self, local, remote)
}

// Meta returns the block list of a non-deleted file.
func (e *Engine) Meta(rel string) (sum [32]byte, blocks [][32]byte, size int64, err error) {
	if !ValidRel(rel) {
		return sum, nil, 0, errors.New("bad path")
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
	data, err := ReadBlock(filepath.Join(e.Root, filepath.FromSlash(rel)), index, size)
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

// ApplyDelete records a remote deletion.
func (e *Engine) ApplyDelete(task Task) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.idx.Files[task.Path]
	if !unchanged(cur, task) {
		return errStale
	}
	if cur != nil && !cur.Deleted && !cur.IsDir {
		_ = os.Remove(filepath.Join(e.Root, filepath.FromSlash(task.Path)))
	}
	if cur != nil && cur.IsDir {
		_ = os.Remove(filepath.Join(e.Root, filepath.FromSlash(task.Path)))
	}
	e.idx.Files[task.Path] = &File{Deleted: true, Version: vclock.Clone(task.Version)}
	e.idx.Seq++
	if err := e.persistLocked(); err != nil {
		return err
	}
	return nil
}

// ApplyMerge stores a version that already describes the bytes on disk.
func (e *Engine) ApplyMerge(task Task) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.idx.Files[task.Path]
	if !unchanged(cur, task) {
		return errStale
	}
	if cur == nil {
		return nil
	}
	cur.Version = vclock.Clone(task.Version)
	e.idx.Seq++
	if err := e.persistLocked(); err != nil {
		return err
	}
	return nil
}

// ApplyDir creates a directory from a remote index entry.
func (e *Engine) ApplyDir(task Task) error {
	if !ValidRel(task.Path) {
		return errors.New("bad path")
	}
	abs := filepath.Join(e.Root, filepath.FromSlash(task.Path))
	e.mu.Lock()
	cur := e.idx.Files[task.Path]
	if !unchanged(cur, task) {
		e.mu.Unlock()
		return errStale
	}
	e.mu.Unlock()

	e.Begin(task.Path)
	defer e.End(task.Path)
	if info, err := os.Lstat(abs); err == nil && !info.IsDir() {
		if err := e.StageConflict(task); err != nil {
			return err
		}
		if err := os.Remove(abs); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(abs, fileMode(task.Remote.Mode, true)); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cur = e.idx.Files[task.Path]
	if cur != nil && !cur.Deleted && cur.IsDir && vclock.Compare(cur.Version, task.Version) == 1 {
		return nil
	}
	e.idx.Files[task.Path] = &File{
		IsDir:   true,
		Mode:    task.Remote.Mode,
		Version: vclock.Clone(task.Version),
	}
	e.idx.Seq++
	if err := e.persistLocked(); err != nil {
		return err
	}
	return nil
}

// StageConflict copies a locally edited file aside so the remote version can
// take the original name. The original file is left in place until the
// download commits.
func (e *Engine) StageConflict(task Task) error {
	if !ValidRel(task.Path) {
		return errors.New("bad path")
	}
	e.mu.Lock()
	cur := e.idx.Files[task.Path]
	if !unchanged(cur, task) || cur == nil || cur.Deleted || cur.IsDir {
		e.mu.Unlock()
		return errStale
	}
	ver := vclock.Bump(cur.Version, e.Self)
	mode := cur.Mode
	e.mu.Unlock()

	name := conflictName(task.Path, e.Self, time.Now())
	for i := 0; ; i++ {
		if ValidRel(name) {
			if _, err := os.Lstat(filepath.Join(e.Root, filepath.FromSlash(name))); errors.Is(err, os.ErrNotExist) {
				break
			}
		}
		name = conflictName(task.Path, e.Self, time.Now().Add(time.Duration(i+1)*time.Millisecond))
		if i > 20 {
			return errors.New("could not pick a conflict name")
		}
	}
	e.Begin(name)
	defer e.End(name)
	src := filepath.Join(e.Root, filepath.FromSlash(task.Path))
	dst := filepath.Join(e.Root, filepath.FromSlash(name))
	if err := copyFile(src, dst, fileMode(mode, false)); err != nil {
		return err
	}
	sum, blocks, size, err := HashFile(dst)
	if err != nil {
		_ = os.Remove(dst)
		return err
	}
	info, err := os.Stat(dst)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.idx.Files[name] = &File{
		Size: size, ModTime: info.ModTime().UnixNano(), Mode: uint32(info.Mode().Perm()),
		Sum: sum, Blocks: blocks, Version: ver,
	}
	e.idx.Seq++
	if err := e.persistLocked(); err != nil {
		return err
	}
	return nil
}

// CommitDownload moves a finished temp file into the share and records the
// remote version, without counting it as a new local edit.
func (e *Engine) CommitDownload(task Task, tmp string, blocks [][32]byte) error {
	if !ValidRel(task.Path) {
		return errors.New("bad path")
	}
	sum := SumBlocks(blocks)
	want, ok := proto.DecodeSum(task.Remote.Sum)
	if !ok || want != sum {
		return errors.New("downloaded checksum mismatch")
	}
	if BlocksForSize(task.Remote.Size) != len(blocks) {
		return errors.New("downloaded size mismatch")
	}
	abs := filepath.Join(e.Root, filepath.FromSlash(task.Path))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	mode := fileMode(task.Remote.Mode, false)
	mtime := time.Unix(0, task.Remote.ModTime)
	if task.Remote.ModTime == 0 {
		mtime = time.Now()
	}
	if err := replaceFile(tmp, abs, mode, mtime); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.idx.Files[task.Path]
	if !unchanged(cur, task) {
		return errStale
	}
	e.idx.Files[task.Path] = &File{
		Size:    task.Remote.Size,
		ModTime: mtime.UnixNano(),
		Mode:    uint32(mode),
		Sum:     sum,
		Blocks:  blocks,
		Version: vclock.Clone(task.Version),
	}
	e.idx.Seq++
	if err := e.persistLocked(); err != nil {
		return err
	}
	return nil
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

func conflictName(rel, device string, now time.Time) string {
	ext := path.Ext(rel)
	base := rel[:len(rel)-len(ext)]
	short := device
	if len(short) > 8 {
		short = short[:8]
	}
	return base + ".sync-conflict-" + short + "-" + strconv.FormatInt(now.UnixMilli(), 10) + ext
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func replaceFile(tmp, dest string, mode os.FileMode, mtime time.Time) error {
	if err := os.Chmod(tmp, mode); err != nil && !errors.Is(err, os.ErrPermission) {
		return err
	}
	if info, err := os.Lstat(dest); err == nil && info.IsDir() {
		return errors.New("path is a directory")
	}
	// Unix rename replaces a file. Windows rename does not.
	if runtime.GOOS == "windows" {
		if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	return os.Chtimes(dest, mtime, mtime)
}
