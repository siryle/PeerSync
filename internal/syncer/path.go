package syncer

import (
	"path"
	"strings"
)

const tmpDirName = ".peersync-tmp"

var reservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// ValidRel reports whether p is a safe relative path using forward slashes.
func ValidRel(p string) bool {
	if p == "" || len(p) > 1024 {
		return false
	}
	if path.IsAbs(p) || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	if p == tmpDirName || strings.HasPrefix(p, tmpDirName+"/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		base := part
		if i := strings.IndexByte(part, '.'); i >= 0 {
			base = part[:i]
		}
		if _, bad := reservedNames[strings.ToUpper(base)]; bad {
			return false
		}
	}
	return true
}
