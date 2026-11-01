package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

// Conflict records a local version kept outside the shared tree.
type Conflict struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
	Size      int64     `json:"size"`
	ModTime   int64     `json:"mod_time"`
	Mode      uint32    `json:"mode"`
	Sum       string    `json:"sum"`
}

type ConflictView struct {
	Conflict
	CurrentExists bool   `json:"current_exists"`
	CurrentIsDir  bool   `json:"current_is_dir"`
	CurrentSum    string `json:"current_sum"`
	CurrentSize   int64  `json:"current_size"`
	BackupText    string `json:"backup_text"`
	CurrentText   string `json:"current_text"`
	TextAvailable bool   `json:"text_available"`
}

func ConflictDir(indexPath string) string {
	return strings.TrimSuffix(indexPath, filepath.Ext(indexPath)) + ".conflicts"
}

func CheckConflictLocation(root, indexPath string) error {
	abs, err := filepath.Abs(ConflictDir(indexPath))
	if err != nil {
		return err
	}
	// Resolve existing ancestors as the backup directory may not exist yet.
	resolved := abs
	var tail []string
	for {
		p, err := filepath.EvalSymlinks(resolved)
		if err == nil {
			resolved = p
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(resolved) == resolved {
			return err
		}
		tail = append(tail, filepath.Base(resolved))
		resolved = filepath.Dir(resolved)
	}
	for i := len(tail) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, tail[i])
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return err
	}
	for ancestor := resolved; ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Stat(ancestor)
		if err == nil && os.SameFile(rootInfo, info) {
			return errors.New("conflict backups must be outside the shared folder; move PEERSYNC_HOME outside it")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(ancestor) == ancestor {
			return nil
		}
	}
}

func validConflictID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == sha256.Size && id == strings.ToLower(id)
}

func readConflict(dir *os.Root, id string) (Conflict, error) {
	var c Conflict
	if !validConflictID(id) {
		return c, errors.New("invalid backup ID")
	}
	f, err := dir.Open(id + ".json")
	if err != nil {
		return c, err
	}
	defer f.Close()
	if err := json.NewDecoder(io.LimitReader(f, 8192)).Decode(&c); err != nil {
		return c, err
	}
	if c.ID != id || !ValidRel(c.Path) || c.Size < 0 {
		return c, errors.New("invalid backup metadata")
	}
	if _, ok := proto.DecodeSum(c.Sum); !ok {
		return c, errors.New("invalid backup checksum")
	}
	return c, nil
}

func ListConflicts(indexPath string) ([]Conflict, error) {
	out := []Conflict{}
	dir, err := os.OpenRoot(ConflictDir(indexPath))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := os.ReadDir(ConflictDir(indexPath))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !validConflictID(id) {
			continue
		}
		c, err := readConflict(dir, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// OpenConflict provides the immutable backup for a download.
func OpenConflict(indexPath, id string) (Conflict, *os.File, error) {
	dir, err := os.OpenRoot(ConflictDir(indexPath))
	if err != nil {
		return Conflict{}, nil, err
	}
	defer dir.Close()
	c, err := readConflict(dir, id)
	if err != nil {
		return c, nil, err
	}
	info, err := dir.Lstat(id + ".data")
	if err != nil || !info.Mode().IsRegular() {
		return c, nil, errors.New("backup is not a regular file")
	}
	f, err := dir.OpenFile(id+".data", readFileFlags, 0)
	if err != nil {
		return c, nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return c, nil, errStale
	}
	return c, f, nil
}

func (e *Engine) stageConflictLocked(task Task) error {
	if err := CheckConflictLocation(e.Root, e.IndexPath); err != nil {
		return err
	}
	cur := e.idx.Files[task.Path]
	if cur == nil || cur.Deleted || cur.IsDir {
		return errStale
	}
	version, _ := json.Marshal(cur.Version)
	h := sha256.New()
	h.Write([]byte(task.Path + "\x00"))
	h.Write(cur.Sum[:])
	h.Write(version)
	id := hex.EncodeToString(h.Sum(nil))
	if c, f, err := OpenConflict(e.IndexPath, id); err == nil {
		sum, _, size, hashErr := HashReader(f)
		f.Close()
		if hashErr == nil && c.Path == task.Path && sum == cur.Sum && size == cur.Size {
			return nil
		}
		return errors.New("existing backup failed verification")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(ConflictDir(e.IndexPath), 0o700); err != nil {
		return err
	}
	dir, err := os.OpenRoot(ConflictDir(e.IndexPath))
	if err != nil {
		return err
	}
	defer dir.Close()
	in, err := e.openFile(task.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	// A crash before publishing metadata may leave data; verify before reusing it.
	out, err := dir.OpenFile(id+".data", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		out, err = dir.OpenFile(id+".data", readFileFlags, 0)
		if err != nil {
			return err
		}
	} else if err == nil {
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Sync()
		}
	}
	if err != nil {
		if out != nil {
			out.Close()
			dir.Remove(id + ".data")
		}
		return err
	}
	_, err = out.Seek(0, io.SeekStart)
	var sum [32]byte
	var size int64
	if err == nil {
		sum, _, size, err = HashReader(out)
	}
	err = errors.Join(err, out.Close())
	if err == nil && (sum != cur.Sum || size != cur.Size) {
		err = errStale
	}
	if err != nil {
		dir.Remove(id + ".data")
		return err
	}
	c := Conflict{ID: id, Path: task.Path, CreatedAt: time.Now().UTC(),
		Size: size, ModTime: cur.ModTime, Mode: cur.Mode, Sum: proto.EncodeSum(sum, true)}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	meta, err := dir.OpenFile(id+".json.tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = meta.Write(b)
	if err == nil {
		err = meta.Sync()
	}
	err = errors.Join(err, meta.Close())
	if err != nil {
		return err
	}
	return dir.Rename(id+".json.tmp", id+".json")
}

func preview(f *os.File) (string, bool, error) {
	const limit = 512 << 10
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > limit || !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
		return "", false, nil
	}
	return string(b), true, nil
}

func (e *Engine) ViewConflict(id string) (ConflictView, error) {
	c, backup, err := OpenConflict(e.IndexPath, id)
	if err != nil {
		return ConflictView{}, err
	}
	defer backup.Close()
	view := ConflictView{Conflict: c, CurrentSum: "missing"}
	view.BackupText, view.TextAvailable, err = preview(backup)
	if err != nil {
		return view, err
	}
	info, err := e.root.Lstat(c.Path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	if info.IsDir() {
		view.CurrentExists, view.CurrentIsDir, view.CurrentSum = true, true, "directory"
		return view, nil
	}
	current, err := e.openFile(c.Path)
	if err != nil {
		return view, err
	}
	defer current.Close()
	sum, _, size, err := e.hashFile(c.Path)
	if err != nil {
		return view, err
	}
	view.CurrentExists, view.CurrentSum, view.CurrentSize = true, proto.EncodeSum(sum, true), size
	text, ok, err := preview(current)
	view.CurrentText, view.TextAvailable = text, view.TextAvailable && ok
	return view, err
}

func InspectConflict(root, indexPath, id string) (ConflictView, error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return ConflictView{}, err
	}
	defer dir.Close()
	e := &Engine{root: dir, IndexPath: indexPath}
	return e.ViewConflict(id)
}

// RestoreConflict is a local operation; the caller must stop sync workers first.
func (e *Engine) RestoreConflict(id, expected string) error {
	c, backup, err := OpenConflict(e.IndexPath, id)
	if err != nil {
		return err
	}
	defer backup.Close()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ignores().Match(c.Path, false) {
		return errIgnored
	}
	cur := e.idx.Files[c.Path]
	actual := "missing"
	if cur != nil && !cur.Deleted {
		if cur.IsDir {
			return errors.New("restore target is a directory")
		}
		actual = proto.EncodeSum(cur.Sum, true)
	}
	if expected == "" || expected != actual {
		return errors.New("current file changed; compare the backup again before restoring")
	}
	if cur != nil && !cur.Deleted {
		if err := e.stageConflictLocked(Task{Path: c.Path}); err != nil {
			return err
		}
	}
	if err := e.root.MkdirAll(tmpDirName, 0o700); err != nil {
		return err
	}
	tmp := path.Join(tmpDirName, "restore-"+id)
	out, err := e.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer e.root.Remove(tmp)
	_, err = io.Copy(out, backup)
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
	err = errors.Join(err, out.Close())
	if err != nil {
		return err
	}
	if proto.EncodeSum(sum, true) != c.Sum || size != c.Size {
		return errors.New("backup failed checksum verification")
	}
	info, err := e.setMetadata(tmp, proto.Entry{Mode: c.Mode, ModTime: c.ModTime})
	if err != nil {
		return err
	}
	task := Task{Path: c.Path, Had: cur != nil, Remote: proto.Entry{Path: c.Path}}
	if cur != nil {
		task.Seen = vclock.Clone(cur.Version)
	}
	if err := e.checkLocalLocked(task); err != nil {
		return err
	}
	if err := e.root.MkdirAll(path.Dir(c.Path), 0o755); err != nil {
		return err
	}
	if err := e.root.Rename(tmp, c.Path); err != nil {
		return err
	}
	e.upsertFile(c.Path, size, info.ModTime().UnixNano(), uint32(info.Mode().Perm()), sum, blocks)
	e.idx.Seq++
	return e.persistLocked()
}
