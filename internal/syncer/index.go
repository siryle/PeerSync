package syncer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/netcraft/peersync/internal/vclock"
)

// Decode one file at a time: a Decoder over the whole map still buffers the
// complete JSON value, just like ReadFile followed by Unmarshal.
func (e *Engine) load() error {
	f, err := os.Open(e.IndexPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	if err := indexDelimiter(d, '{'); err != nil {
		return err
	}
	idx := indexFile{Files: make(map[string]*File)}
	var clocks vclock.Pool
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		switch key {
		case "seq":
			if err := d.Decode(&idx.Seq); err != nil {
				return err
			}
		case "files":
			token, err := d.Token()
			if err != nil {
				return err
			}
			idx.Files = make(map[string]*File)
			if token == nil {
				continue
			}
			if token != json.Delim('{') {
				return errors.New("index files must be an object")
			}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				var file *File
				if err := d.Decode(&file); err != nil {
					return err
				}
				if file == nil {
					return errors.New("null file in index")
				}
				file.Version = clocks.Intern(file.Version)
				idx.Files[key.(string)] = file
			}
			if err := indexDelimiter(d, '}'); err != nil {
				return err
			}
		default:
			var skip json.RawMessage
			if err := d.Decode(&skip); err != nil {
				return err
			}
		}
	}
	if err := indexDelimiter(d, '}'); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("trailing data in index")
		}
		return err
	}
	// Publish only after the entire index has been validated.
	e.idx, e.clocks = idx, clocks
	return nil
}

func indexDelimiter(d *json.Decoder, want json.Delim) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token != want {
		return fmt.Errorf("expected index delimiter %q", want)
	}
	return nil
}

func cloneIndex(idx indexFile) indexFile {
	out := indexFile{Seq: idx.Seq, Files: make(map[string]*File, len(idx.Files))}
	for name, file := range idx.Files {
		copied := *file
		out.Files[name] = &copied
	}
	return out
}

// saveSnapshot writes one consistent index snapshot outside e.mu, so a large
// tree does not hold up scan and transfer steps while the file is written.
func (e *Engine) saveSnapshot(snap indexFile) error {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	e.mu.Lock()
	current := e.idx.Seq
	e.mu.Unlock()
	if current != snap.Seq {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.IndexPath), 0o700); err != nil {
		return err
	}
	tmp := e.IndexPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	err = writeIndex(f, snap)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, e.IndexPath)
}

func writeIndex(dst io.Writer, idx indexFile) error {
	w := bufio.NewWriterSize(dst, 64<<10)
	if _, err := fmt.Fprintf(w, `{"seq":%d,"files":{`, idx.Seq); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	first := true
	for name, file := range idx.Files {
		if !first {
			if err := w.WriteByte(','); err != nil {
				return err
			}
		}
		first = false
		if err := enc.Encode(name); err != nil {
			return err
		}
		if err := w.WriteByte(':'); err != nil {
			return err
		}
		if err := enc.Encode(file); err != nil {
			return err
		}
	}
	if _, err := w.WriteString("}}\n"); err != nil {
		return err
	}
	return w.Flush()
}
