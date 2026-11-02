package syncer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/netcraft/peersync/internal/config"
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
	Local   bool          `json:"local,omitempty"`
}

type indexFile struct {
	Seq   uint64           `json:"seq"`
	Files map[string]*File `json:"files"`
}

// Engine tracks one shared directory and applies peer updates.
type Engine struct {
	Root        string
	IndexPath   string
	Self        string
	Log         *slog.Logger
	root        *os.Root
	Mode        config.SyncMode
	SourceID    string
	verifyEvery time.Duration

	mu       sync.Mutex
	idx      indexFile
	clocks   vclock.Pool
	inflight map[string]int
	subs     []chan struct{}
	kick     chan struct{}
	// batch coalesces index writes. A reconcile used to rewrite the whole
	// index once per file, which dominated syncing a large tree.
	batch      int
	dirty      bool
	statsSeq   uint64
	statsFiles int
	statsValid bool

	ready     chan struct{}
	readyOnce sync.Once
}

type Options struct {
	Mode        config.SyncMode
	SourceID    string
	VerifyEvery time.Duration
}

// Open loads an index. A missing or corrupt index starts empty; the next
// scan treats every file as a local edit, which converges with identical
// copies instead of deleting them.
func Open(root, indexPath, self string, log *slog.Logger, options ...Options) (*Engine, error) {
	if log == nil {
		log = slog.Default()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var opt Options
	if len(options) > 0 {
		opt = options[0]
	}
	if err := config.ValidateSync(opt.Mode, opt.SourceID); err != nil {
		return nil, err
	}
	if opt.VerifyEvery < 0 {
		return nil, errors.New("verification interval must be positive")
	}
	if opt.VerifyEvery == 0 {
		opt.VerifyEvery = 5 * time.Minute
	}
	if err := CheckConflictLocation(abs, indexPath); err != nil {
		return nil, err
	}
	dir, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		Root:        abs,
		root:        dir,
		IndexPath:   indexPath,
		Self:        self,
		Log:         log,
		inflight:    map[string]int{},
		kick:        make(chan struct{}, 1),
		idx:         indexFile{Files: map[string]*File{}},
		ready:       make(chan struct{}),
		Mode:        opt.Mode.Effective(),
		SourceID:    opt.SourceID,
		verifyEvery: opt.VerifyEvery,
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
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	e.Log.Info("scan intervals", "root", e.Root, "scan_every", every, "verify_every", e.verifyEvery)
	verified := time.Time{}
	if err := e.scanContext(ctx, true, watcher); err != nil {
		e.Log.Warn("scan", "err", err, "root", e.Root)
	} else {
		verified = time.Now()
	}
	ig := e.ignores()

	// Polling keeps its own deadline even while file events keep arriving.
	poll := time.NewTicker(min(every, e.verifyEvery))
	defer poll.Stop()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	pending, whole := false, false
	paths := map[string]bool{}
	schedule := func() {
		if !pending {
			timer.Reset(300 * time.Millisecond)
			pending = true
		}
	}
	runScan := func(all bool) {
		timer.Stop()
		pending = false
		full := verified.IsZero() || time.Since(verified) >= e.verifyEvery
		var changed []string
		if !all && !whole && !full {
			for rel := range paths {
				changed = append(changed, rel)
			}
		}
		clear(paths)
		whole = false
		if err := e.scanContext(ctx, full, watcher, changed...); err != nil {
			e.Log.Warn("scan", "err", err, "root", e.Root)
		} else if full {
			verified = time.Now()
		}
		ig = e.ignores()
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-e.kick:
			whole = true
			schedule()
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			rel, err := relOf(e.Root, event.Name)
			if err == nil {
				info, statErr := e.root.Lstat(rel)
				dir := statErr == nil && info.IsDir()
				if statErr != nil {
					e.mu.Lock()
					if f := e.idx.Files[rel]; f != nil {
						dir = f.IsDir
					}
					e.mu.Unlock()
				}
				if ig.Match(rel, dir) {
					continue
				}
				paths[rel] = true
			}
			// kqueue may report only the containing directory; ignore changes
			// and root events also need a complete walk.
			if err != nil || rel == ".peersyncignore" || kqueueOS() {
				whole = true
			}
			schedule()
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			e.Log.Warn("watch", "err", err)
			whole = true
			schedule()
		case <-poll.C:
			runScan(true)
		case <-timer.C:
			runScan(false)
		}
	}
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
	return e.scanContext(context.Background(), full, nil)
}

func (e *Engine) scanContext(ctx context.Context, full bool, watcher *fsnotify.Watcher, paths ...string) error {
	started := time.Now()
	scopes := make(map[string]bool, len(paths))
	for _, rel := range paths {
		if !ValidRel(rel) {
			return errors.New("bad scan path")
		}
		scopes[rel] = true
	}
	paths = nil
	for rel := range scopes {
		covered := false
		for parent := path.Dir(rel); parent != "."; parent = path.Dir(parent) {
			if scopes[parent] {
				covered = true
				break
			}
		}
		if !covered {
			paths = append(paths, rel)
		}
	}
	inScope := func(rel string) bool {
		if len(paths) == 0 {
			return true
		}
		for rel != "." {
			if scopes[rel] {
				return true
			}
			rel = path.Dir(rel)
		}
		return false
	}
	ig := e.ignores()
	watched := map[string]bool{}
	watchFailed := false
	if watcher != nil {
		for _, abs := range watcher.WatchList() {
			if rel, err := relOf(e.Root, abs); (err == nil && inScope(rel)) || (len(paths) == 0 && abs == e.Root) {
				watched[abs] = false
			}
		}
	}
	watch := func(rel string) {
		if watcher == nil || (rel != "." && kqueueOS()) {
			return
		}
		abs := filepath.Join(e.Root, filepath.FromSlash(rel))
		if _, exists := watched[abs]; exists {
			watched[abs] = true
		} else if err := watcher.Add(abs); err != nil {
			if !watchFailed {
				e.Log.Warn("watch directory; periodic scans remain enabled", "path", abs, "err", err)
				watchFailed = true
			}
		} else {
			watched[abs] = true
		}
	}
	if len(paths) == 0 {
		watch(".")
	}
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
	walk := func(rel string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if len(paths) > 0 && errors.Is(err, os.ErrNotExist) {
				return nil
			}
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
		if info.IsDir() {
			watch(rel)
		}
		items = append(items, item{
			rel:   rel,
			size:  info.Size(),
			mtime: info.ModTime().UnixNano(),
			mode:  uint32(info.Mode().Perm()),
			dir:   info.IsDir(),
		})
		return nil
	}
	roots := paths
	if len(roots) == 0 {
		roots = []string{"."}
	}
	for _, root := range roots {
		if err := fs.WalkDir(e.root.FS(), root, walk); err != nil {
			return err
		}
	}
	if rootErr != nil {
		return fmt.Errorf("scan %s: %w", e.Root, rootErr)
	}
	for abs, seen := range watched {
		rel, err := relOf(e.Root, abs)
		if !seen && err == nil && !unreadCovers(unread, rel) {
			if err := watcher.Remove(abs); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
				e.Log.Debug("remove watch", "path", abs, "err", err)
			}
		}
	}

	e.mu.Lock()
	type snapshot struct {
		size, mtime  int64
		blocks       int
		deleted, dir bool
		version      vclock.Vector
	}
	size := len(e.idx.Files)
	if len(paths) > 0 {
		size = len(items)
	}
	snap := make(map[string]snapshot, size)
	for k, v := range e.idx.Files {
		if inScope(k) {
			// Index updates replace version maps instead of mutating them.
			snap[k] = snapshot{v.Size, v.ModTime, len(v.Blocks), v.Deleted, v.IsDir, v.Version}
		}
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
		if err := ctx.Err(); err != nil {
			return err
		}
		if it.dir || inflight[it.rel] > 0 {
			continue
		}
		prev, exists := snap[it.rel]
		if !full && exists && !prev.deleted && !prev.dir && prev.size == it.size && prev.mtime == it.mtime && prev.blocks == BlocksForSize(it.size) {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	changed := false
	seen := map[string]bool{}
	for _, it := range items {
		if e.inflight[it.rel] > 0 {
			seen[it.rel] = true
			continue
		}
		seen[it.rel] = true
		cur := e.idx.Files[it.rel]
		prev, exists := snap[it.rel]
		if (cur != nil) != exists || (cur != nil && vclock.Compare(cur.Version, prev.version) != 0) {
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
		if cur != nil && !cur.Deleted && cur.Mode != it.mode {
			cur.Mode = it.mode
			cur.Version = e.clocks.Intern(vclock.Bump(cur.Version, e.Self))
			cur.Local = e.Mode == config.ReceiveOnly
			changed = true
		}
	}
	for rel, f := range e.idx.Files {
		if !inScope(rel) || seen[rel] || f.Deleted || e.inflight[rel] > 0 || unreadCovers(unread, rel) {
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
		f.Version = e.clocks.Intern(vclock.Bump(f.Version, e.Self))
		f.Local = e.Mode == config.ReceiveOnly
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
	e.Log.Debug("scan complete", "root", e.Root, "full", full, "scoped", len(paths) > 0, "paths", len(items), "changed", changed, "elapsed", time.Since(started))
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
		prev.Version = e.clocks.Intern(vclock.Bump(prev.Version, e.Self))
		prev.Local = e.Mode == config.ReceiveOnly
		return true
	}
	ver := vclock.Vector{}
	if prev != nil {
		ver = prev.Version
	}
	e.idx.Files[rel] = &File{IsDir: true, Mode: mode, Version: e.clocks.Intern(vclock.Bump(ver, e.Self)), Local: e.Mode == config.ReceiveOnly}
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
		Sum: sum, Blocks: blocks, Version: e.clocks.Intern(vclock.Bump(ver, e.Self)),
		Local: e.Mode == config.ReceiveOnly,
	}
	return true
}

// Entries is a snapshot of the index for announcement to peers.
func (e *Engine) Entries() (uint64, []proto.Entry) {
	return e.entries(true)
}

// Internal announcements share immutable versions instead of cloning every map.
func (e *Engine) entries(cloneVersions bool) (uint64, []proto.Entry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]proto.Entry, 0, len(e.idx.Files))
	for p, f := range e.idx.Files {
		version := f.Version
		if cloneVersions {
			version = vclock.Clone(version)
		}
		out = append(out, proto.Entry{
			Path:    p,
			Size:    f.Size,
			ModTime: f.ModTime,
			Mode:    f.Mode,
			IsDir:   f.IsDir,
			Deleted: f.Deleted,
			Sum:     proto.EncodeSum(f.Sum, !f.Deleted && !f.IsDir),
			Version: version,
		})
	}
	return e.idx.Seq, out
}

// Plan builds work items for a remote index.
func (e *Engine) Plan(remote []proto.Entry) []Task {
	if e.Mode == config.SendOnly {
		return nil
	}
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
	if e.Mode == config.ReceiveOnly {
		local := make(map[string]*File, len(e.idx.Files))
		for k, v := range e.idx.Files {
			if !ig.Match(k, v.IsDir) {
				local[k] = v
			}
		}
		return planReceive(local, filtered)
	}
	return Plan(e.Self, e.idx.Files, filtered)
}

// Meta returns the block list of a non-deleted file.
func (e *Engine) Meta(rel string) (sum [32]byte, blocks [][32]byte, size int64, err error) {
	if e.Mode == config.ReceiveOnly {
		return sum, nil, 0, errors.New("receive-only folder does not serve files")
	}
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
	if task.Backup {
		if err := e.stageConflictLocked(task); err != nil {
			return err
		}
		if err := e.checkLocalLocked(task); err != nil {
			return err
		}
	}
	if err := e.root.Remove(task.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	e.idx.Files[task.Path] = &File{Deleted: true, IsDir: task.Remote.IsDir, Version: e.clocks.Intern(task.Version)}
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
	cur.Version = e.clocks.Intern(task.Version)
	cur.Local = false
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
		if err := e.checkLocalLocked(task); err != nil {
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
	e.idx.Files[task.Path] = &File{IsDir: true, Mode: uint32(info.Mode().Perm()), Version: e.clocks.Intern(task.Version)}
	e.idx.Seq++
	return e.persistLocked()
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
	if task.Op == opConflict || task.Backup {
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
	if task.Op == opConflict || task.Backup {
		if err := e.checkLocalLocked(task); err != nil {
			return err
		}
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
		Sum: sum, Blocks: blocks, Version: e.clocks.Intern(task.Version),
	}
	e.idx.Seq++
	return e.persistLocked()
}

// Stats counts live files and directories.
func (e *Engine) Stats() (files int, seq uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.statsValid || e.statsSeq != e.idx.Seq {
		e.statsFiles = 0
		for _, f := range e.idx.Files {
			if !f.Deleted {
				e.statsFiles++
			}
		}
		e.statsSeq, e.statsValid = e.idx.Seq, true
	}
	return e.statsFiles, e.idx.Seq
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
