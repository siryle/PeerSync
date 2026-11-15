package syncer

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

var errIgnored = errors.New("path is ignored")

// Root confines links to the share, but parents must not redirect sync paths.
func (e *Engine) checkParents(rel string) error {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return errors.New("bad path")
	}
	for parent := path.Dir(rel); parent != "."; parent = path.Dir(parent) {
		info, err := e.root.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink parent is not synchronized")
		}
	}
	return nil
}

// Called with e.mu held; restore directory permissions before releasing it.
func (e *Engine) withWritableParents(rel string, fn func() error) (err error) {
	if err := e.checkParents(rel); err != nil {
		return err
	}
	var parents []string
	for p := path.Dir(rel); p != "."; p = path.Dir(p) {
		parents = append(parents, p)
	}
	type permission struct {
		path string
		mode os.FileMode
	}
	var restore []permission
	defer func() {
		for i := len(restore) - 1; i >= 0; i-- {
			p := restore[i]
			err = errors.Join(err, e.root.Chmod(p.path, p.mode))
		}
	}()
	for i := len(parents) - 1; i >= 0; i-- {
		p := parents[i]
		info, statErr := e.root.Lstat(p)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() {
			return errors.New("parent is not a directory")
		}
		mode := info.Mode().Perm()
		if mode&0o300 != 0o300 {
			if err := e.root.Chmod(p, mode|0o300); err != nil {
				return err
			}
			restore = append(restore, permission{p, mode})
		}
	}
	return fn()
}

func (e *Engine) setMetadata(rel string, r proto.Entry) (os.FileInfo, error) {
	if err := e.checkParents(rel); err != nil {
		return nil, err
	}
	if err := e.root.Chmod(rel, fileMode(r.Mode, r.Mode0, r.IsDir)); err != nil {
		return nil, err
	}
	if !r.IsDir {
		mtime := time.Unix(0, r.ModTime)
		if r.ModTime == 0 {
			mtime = time.Now()
		}
		if err := e.root.Chtimes(rel, mtime, mtime); err != nil {
			return nil, err
		}
	}
	return e.root.Lstat(rel)
}

// openFile confines reads to the share and never reads a pipe or device.
func (e *Engine) openFile(rel string) (*os.File, error) {
	if err := e.checkParents(rel); err != nil {
		return nil, err
	}
	info, err := e.root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := e.root.OpenFile(rel, readFileFlags, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, errStale
	}
	return f, nil
}

func (e *Engine) hashFile(rel string) (sum [32]byte, blocks [][32]byte, size int64, err error) {
	f, err := e.openFile(rel)
	if err != nil {
		return sum, nil, 0, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return sum, nil, 0, err
	}
	sum, blocks, size, err = HashReader(f)
	if err != nil {
		return sum, nil, 0, err
	}
	after, err := f.Stat()
	if err != nil {
		return sum, nil, 0, err
	}
	current, pathErr := e.root.Lstat(rel)
	if pathErr != nil || !os.SameFile(after, current) || size != after.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return sum, nil, 0, errStale
	}
	return sum, blocks, size, nil
}

// checkLocalLocked also records edits hidden from scans by an active download.
func (e *Engine) checkLocalLocked(task Task) error {
	if !ValidRel(task.Path) {
		return errors.New("bad path")
	}
	cur := e.idx.Files[task.Path]
	dir := task.Remote.IsDir || (cur != nil && cur.IsDir)
	ig, err := e.ignores()
	if err != nil {
		return err
	}
	if ig.Match(task.Path, dir) {
		return errIgnored
	}
	if err := e.checkParents(task.Path); err != nil {
		return err
	}
	if !unchanged(cur, task) {
		return errStale
	}
	info, err := e.root.Lstat(task.Path)
	changed := false
	switch {
	case errors.Is(err, os.ErrNotExist):
		if cur == nil || cur.Deleted {
			return nil
		}
		e.idx.Files[task.Path] = &File{Deleted: true, IsDir: cur.IsDir, Version: e.clocks.Intern(vclock.Bump(cur.Version, e.Self)), Local: e.Mode == config.ReceiveOnly}
		changed = true
	case err != nil:
		return err
	case info.IsDir():
		changed = e.upsertDir(task.Path, uint32(info.Mode().Perm()))
	case !info.Mode().IsRegular():
		return errStale
	default:
		// Hashing a large file must not stall every other update in this folder.
		e.mu.Unlock()
		sum, blocks, size, err := e.hashFile(task.Path)
		e.mu.Lock()
		if err != nil {
			return err
		}
		if !unchanged(e.idx.Files[task.Path], task) {
			return errStale
		}
		info, err = e.root.Lstat(task.Path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != size {
			return errStale
		}
		changed = e.upsertFile(task.Path, size, info.ModTime().UnixNano(), uint32(info.Mode().Perm()), sum, blocks)
	}
	if !changed {
		return nil
	}
	e.idx.Seq++
	if err := e.persistLocked(); err != nil {
		return err
	}
	e.Kick()
	return errStale
}
