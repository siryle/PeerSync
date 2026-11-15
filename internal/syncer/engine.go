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
	Mode0   bool          `json:"mode0,omitempty"` // Mode 0 is real; a missing mode is still the default.
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

	mu        sync.Mutex
	backupMu  sync.Mutex
	persistMu sync.Mutex
	idx       indexFile
	clocks    vclock.Pool
	inflight  map[string]int
	subs      []chan struct{}
	kick      chan struct{}
	// batch coalesces index writes. A reconcile used to rewrite the whole
	// index once per file, which dominated syncing a large tree.
	batch        int
	dirty        bool
	statsSeq     uint64
	statsFiles   int
	statsValid   bool
	caseFold     bool
	scanComplete bool

	ready     chan struct{}
	readyOnce sync.Once
}

type Options struct {
	Mode        config.SyncMode
	SourceID    string
	VerifyEvery time.Duration
}

// Open loads an index. An existing unreadable index stops the share so old
// remote versions cannot outrank local edits after resetting its clocks.
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
		_ = dir.Close()
		return nil, fmt.Errorf("index unreadable; restore the index before syncing %s: %w", indexPath, err)
	}
	e.caseFold = foldsCase(abs)
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
	ig, _ := e.ignores()

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
		ig, _ = e.ignores()
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
			// Root events and ignore-rule changes still need a complete walk.
			if err != nil || rel == ".peersyncignore" || (e.caseFold && event.Op&fsnotify.Rename != 0) {
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

// Scan refreshes the index and verifies all file contents.
func (e *Engine) Scan() error {
	return e.scan(true)
}

func (e *Engine) scan(full bool) error {
	return e.scanContext(context.Background(), full, nil)
}

func (e *Engine) scanContext(ctx context.Context, full bool, watcher *fsnotify.Watcher, paths ...string) (scanErr error) {
	defer func() {
		if scanErr != nil {
			e.mu.Lock()
			e.scanComplete = false
			e.mu.Unlock()
		}
	}()
	started := time.Now()
	scopes := make(map[string]bool, len(paths))
	for _, rel := range paths {
		if !ValidRel(rel) {
			return errors.New("bad scan path")
		}
		scopes[rel] = true
	}
	e.mu.Lock()
	complete := e.scanComplete
	e.mu.Unlock()
	if !complete {
		full = true
		clear(scopes)
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
	ig, err := e.ignores()
	if err != nil {
		return err
	}
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
		if watcher == nil {
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
	var links []string
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
			// A link hides whatever it replaced. That is not evidence the files are gone.
			if ValidRel(rel) {
				links = append(links, rel)
			}
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
		if err := e.checkParents(root); err != nil {
			return err
		}
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
			unread = append(unread, it.rel)
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
			info, err := e.root.Lstat(it.rel)
			if err != nil || !info.IsDir() {
				unread = append(unread, it.rel)
				continue
			}
			if e.upsertDir(it.rel, uint32(info.Mode().Perm())) {
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
		if cur != nil && !cur.Deleted && (cur.Mode != it.mode || cur.Mode0 != (it.mode == 0)) {
			cur.Mode = it.mode
			cur.Mode0 = it.mode == 0
			cur.Version = e.clocks.Intern(vclock.Bump(cur.Version, e.Self))
			cur.Local = e.Mode == config.ReceiveOnly
			changed = true
		}
	}
	for rel, f := range e.idx.Files {
		if !inScope(rel) || seen[rel] || f.Deleted || e.inflight[rel] > 0 || unreadCovers(unread, rel) || unreadCovers(links, rel) {
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
	becameComplete := !e.scanComplete && len(unread) == 0
	e.scanComplete = len(unread) == 0
	if changed {
		e.idx.Seq++
	}
	if changed || e.dirty {
		if err := e.persistLocked(); err != nil {
			return err
		}
	} else if becameComplete {
		e.notify()
	}
	if len(unread) > 0 {
		return fmt.Errorf("scan incomplete, left %s unchanged", unread[0])
	}
	e.markReady()
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

func (e *Engine) ignores() (ignore.Set, error) {
	s := ignore.Defaults()
	f, err := e.openFile(".peersyncignore")
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read ignore rules: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return s, fmt.Errorf("read ignore rules: %w", err)
	}
	return s.Merge(ignore.Parse(string(b))), nil
}

func (e *Engine) upsertDir(rel string, mode uint32) bool {
	prev := e.idx.Files[rel]
	zero := mode == 0
	if prev != nil && !prev.Deleted && prev.IsDir && prev.Mode == mode {
		if prev.Mode0 == zero {
			return false
		}
		prev.Mode0 = zero
		prev.Version = e.clocks.Intern(vclock.Bump(prev.Version, e.Self))
		prev.Local = e.Mode == config.ReceiveOnly
		return true
	}
	ver := vclock.Vector{}
	if prev != nil {
		ver = prev.Version
	}
	e.idx.Files[rel] = &File{IsDir: true, Mode: mode, Mode0: zero, Version: e.clocks.Intern(vclock.Bump(ver, e.Self)), Local: e.Mode == config.ReceiveOnly}
	return true
}

func (e *Engine) upsertFile(rel string, size, mtime int64, mode uint32, sum [32]byte, blocks [][32]byte) bool {
	prev := e.idx.Files[rel]
	zero := mode == 0
	if prev != nil && !prev.Deleted && !prev.IsDir && prev.Sum == sum && prev.ModTime == mtime && prev.Mode == mode {
		prev.Size = size
		prev.ModTime = mtime
		prev.Mode = mode
		prev.Blocks = blocks
		if prev.Mode0 == zero {
			return false
		}
		prev.Mode0 = zero
		prev.Version = e.clocks.Intern(vclock.Bump(prev.Version, e.Self))
		prev.Local = e.Mode == config.ReceiveOnly
		return true
	}
	ver := vclock.Vector{}
	if prev != nil {
		ver = prev.Version
	}
	e.idx.Files[rel] = &File{
		Size: size, ModTime: mtime, Mode: mode, Mode0: zero,
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
	return e.entriesLocked(cloneVersions)
}

func (e *Engine) entriesLocked(cloneVersions bool) (uint64, []proto.Entry) {
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
			Mode0:   f.Mode0,
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
	ig, err := e.ignores()
	if err != nil {
		e.Log.Warn("sync plan blocked", "err", err)
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	owners := e.foldOwners()
	filtered := make([]proto.Entry, 0, len(remote))
	var holds []Task
	for _, entry := range remote {
		if !ValidRel(entry.Path) || ig.Match(entry.Path, entry.IsDir) {
			continue
		}
		if owner, ok := owners[foldKey(entry.Path)]; ok && owner != entry.Path {
			if !entry.IsDir && !entry.Deleted {
				holds = append(holds, Task{Op: opPull, Path: entry.Path, Remote: entry, Version: vclock.Clone(entry.Version), Hold: true})
			}
			continue
		}
		cur := e.idx.Files[entry.Path]
		if !ig.Match(entry.Path, entry.IsDir || (cur != nil && cur.IsDir)) {
			filtered = append(filtered, entry)
		}
	}
	var tasks []Task
	if e.Mode == config.ReceiveOnly {
		local := make(map[string]*File, len(e.idx.Files))
		for k, v := range e.idx.Files {
			if !ig.Match(k, v.IsDir) {
				local[k] = v
			}
		}
		tasks = planReceive(local, filtered, e.caseFold)
	} else {
		tasks = Plan(e.Self, e.idx.Files, filtered)
	}
	return append(holds, tasks...)
}

// Meta returns the block list of a non-deleted file for a peer that asked.
func (e *Engine) Meta(rel string) (sum [32]byte, blocks [][32]byte, size int64, err error) {
	if e.Mode == config.ReceiveOnly {
		return sum, nil, 0, errors.New("receive-only folder does not serve files")
	}
	return e.fileBlocks(rel)
}

// fileBlocks is the local block list used to plan a download. Receive-only
// folders use it too; Meta is what refuses to serve those bytes to a peer.
func (e *Engine) fileBlocks(rel string) (sum [32]byte, blocks [][32]byte, size int64, err error) {
	if !ValidRel(rel) {
		return sum, nil, 0, errors.New("bad path")
	}
	ig, err := e.ignores()
	if err != nil {
		return sum, nil, 0, err
	}
	if ig.Match(rel, false) {
		return sum, nil, 0, os.ErrNotExist
	}
	if err := e.checkParents(rel); err != nil {
		return sum, nil, 0, err
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
	if err := e.withWritableParents(task.Path, func() error {
		err := e.root.Remove(task.Path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}); err != nil {
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
		cur.Mode0 = cur.Mode == 0
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
	}
	var info os.FileInfo
	err := e.withWritableParents(task.Path, func() error {
		if cur := e.idx.Files[task.Path]; cur != nil && !cur.Deleted && !cur.IsDir {
			if err := e.root.Remove(task.Path); err != nil {
				return err
			}
		}
		if err := e.root.MkdirAll(task.Path, fileMode(task.Remote.Mode, task.Remote.Mode0, true)); err != nil {
			return err
		}
		var err error
		info, err = e.setMetadata(task.Path, task.Remote)
		return err
	})
	if err != nil {
		return err
	}
	perm := uint32(info.Mode().Perm())
	e.idx.Files[task.Path] = &File{IsDir: true, Mode: perm, Mode0: perm == 0, Version: e.clocks.Intern(task.Version)}
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
	if task.Hold {
		if !ValidRel(task.Path) {
			return errors.New("bad path")
		}
		ig, err := e.ignores()
		if err != nil {
			return err
		}
		if ig.Match(task.Path, false) {
			return errIgnored
		}
		if err := e.checkParents(task.Path); err != nil {
			return err
		}
		return e.backupIncomingLocked(task, tmp)
	}
	if err := e.checkLocalLocked(task); err != nil {
		return err
	}
	if task.Op == opConflict || task.Backup {
		if err := e.stageConflictLocked(task); err != nil {
			return err
		}
	}
	if task.Op == opConflict || task.Backup {
		if err := e.checkLocalLocked(task); err != nil {
			return err
		}
	}
	var info os.FileInfo
	err := e.withWritableParents(task.Path, func() error {
		if err := e.root.MkdirAll(path.Dir(task.Path), 0o755); err != nil {
			return err
		}
		var err error
		info, err = e.setMetadata(tmp, task.Remote)
		if err != nil {
			return err
		}
		cur := e.idx.Files[task.Path]
		replacingDir := cur != nil && !cur.Deleted && cur.IsDir
		if replacingDir {
			if err := e.root.Remove(task.Path); err != nil {
				return err
			}
		}
		if err := e.root.Rename(tmp, task.Path); err != nil {
			if replacingDir {
				return errors.Join(err, e.root.Mkdir(task.Path, fileMode(cur.Mode, cur.Mode0, true)))
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	perm := uint32(info.Mode().Perm())
	e.idx.Files[task.Path] = &File{
		Size: task.Remote.Size, ModTime: info.ModTime().UnixNano(), Mode: perm, Mode0: perm == 0,
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
	if err := e.persistLocked(); err != nil {
		e.Log.Warn("save index", "err", err)
	}
}

// persistLocked writes the index unless a batch is open.
func (e *Engine) persistLocked() error {
	e.dirty = true
	if e.batch > 0 {
		return nil
	}
	snap := cloneIndex(e.idx)
	e.mu.Unlock()
	err := e.saveSnapshot(snap)
	e.mu.Lock()
	if err != nil {
		return err
	}
	if e.idx.Seq == snap.Seq {
		e.dirty = false
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

func fileMode(m uint32, keepZero, dir bool) os.FileMode {
	perm := os.FileMode(m) & 0o777
	if perm == 0 && !keepZero {
		if dir {
			return 0o755
		}
		return 0o644
	}
	return perm
}

func foldKey(p string) string { return strings.ToLower(p) }

// foldsCase reports whether the filesystem treats names that differ only by
// case as the same file.
func foldsCase(root string) bool {
	probe, err := os.MkdirTemp(root, ".peersync-probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(probe)
	name := filepath.Join(probe, "CaseProbe")
	if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(probe, "caseprobe"))
	return err == nil
}

// foldOwners maps each folded path to the one live index entry that owns it.
// It is called with e.mu held.
func (e *Engine) foldOwners() map[string]string {
	if !e.caseFold {
		return nil
	}
	owners := make(map[string]string, len(e.idx.Files))
	for path, file := range e.idx.Files {
		if file.Deleted {
			continue
		}
		key := foldKey(path)
		if _, ok := owners[key]; !ok {
			owners[key] = path
		}
	}
	return owners
}
