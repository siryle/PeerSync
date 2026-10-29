// Package ignore matches a small subset of gitignore syntax used by
// .peersyncignore files.
package ignore

import (
	"path"
	"strings"
)

// Set is an ordered list of ignore patterns. Later patterns do not override
// earlier ones; the first match wins. There is no negation syntax.
type Set struct {
	pats []pattern
}

type pattern struct {
	raw      string
	dirOnly  bool
	anchored bool
	anyDir   bool
}

// Defaults ignores operating-system junk and the temporary download directory.
func Defaults() Set {
	return Parse(`
.DS_Store
Thumbs.db
desktop.ini
*.tmp
*~
~$*
.peersync-tmp/
`)
}

// Parse reads patterns separated by newlines. Blank lines and lines starting
// with # are comments.
func Parse(text string) Set {
	var s Set
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		p := pattern{raw: trim}
		if strings.HasSuffix(p.raw, "/") {
			p.dirOnly = true
			p.raw = strings.TrimSuffix(p.raw, "/")
		}
		if strings.HasPrefix(p.raw, "/") {
			p.anchored = true
			p.raw = strings.TrimPrefix(p.raw, "/")
		}
		if strings.Contains(p.raw, "/") {
			p.anchored = true
		}
		// A pattern with no slash matches in every directory, unless it was
		// anchored to the share root with a leading slash.
		p.anyDir = !p.anchored
		if p.raw != "" {
			s.pats = append(s.pats, p)
		}
	}
	return s
}

// Merge appends extra patterns after the receiver's patterns.
func (s Set) Merge(extra Set) Set {
	s.pats = append(append([]pattern{}, s.pats...), extra.pats...)
	return s
}

// Match reports whether rel should be left out of the sync.
// rel uses forward slashes and is relative to the shared root.
func (s Set) Match(rel string, isDir bool) bool {
	rel = path.Clean(rel)
	if rel == "." {
		return false
	}
	base := path.Base(rel)
	for _, p := range s.pats {
		if p.dirOnly && !isDir {
			// A directory pattern also ignores everything inside it.
			if p.anyDir {
				if hasDirComponent(rel, p.raw) {
					return true
				}
			} else if insideDir(rel, p) {
				return true
			}
			continue
		}
		if p.anyDir {
			if matchGlob(p.raw, base) || (isDir && matchGlob(p.raw, rel)) {
				return true
			}
			continue
		}
		if matchGlob(p.raw, rel) || (isDir && matchGlob(p.raw, rel)) {
			return true
		}
		if insideDir(rel, p) {
			return true
		}
	}
	return false
}

func insideDir(rel string, p pattern) bool {
	prefix := p.raw
	return rel == prefix || strings.HasPrefix(rel, prefix+"/")
}

func hasDirComponent(rel, name string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return false
	}
	for _, c := range parts[:len(parts)-1] {
		if matchGlob(name, c) {
			return true
		}
	}
	return false
}

func matchGlob(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}
