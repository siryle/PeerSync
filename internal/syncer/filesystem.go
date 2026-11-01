package syncer

import (
	"errors"
	"os"
	"time"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

var errIgnored = errors.New("path is ignored")

func (e *Engine) setMetadata(rel string, r proto.Entry) (os.FileInfo, error) {
	if err := e.root.Chmod(rel, fileMode(r.Mode, r.IsDir)); err != nil {
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
	if e.ignores().Match(task.Path, dir) {
		return errIgnored
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
		e.idx.Files[task.Path] = &File{Deleted: true, IsDir: cur.IsDir, Version: vclock.Bump(cur.Version, e.Self), Local: e.Mode == config.ReceiveOnly}
		changed = true
	case err != nil:
		return err
	case info.IsDir():
		changed = e.upsertDir(task.Path, uint32(info.Mode().Perm()))
	case !info.Mode().IsRegular():
		return errStale
	default:
		sum, blocks, size, err := e.hashFile(task.Path)
		if err != nil {
			return err
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
