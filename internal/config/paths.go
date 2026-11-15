package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ValidatePaths also catches aliases through symlinks and filesystem case folding.
func ValidatePaths(shares []Share) error {
	var roots []string
	for _, sh := range shares {
		root, err := resolvedPath(sh.Path)
		if err != nil {
			return err
		}
		for _, other := range roots {
			if containsPath(root, other) || containsPath(other, root) {
				return fmt.Errorf("shared folders overlap: %s and %s", other, root)
			}
		}
		roots = append(roots, root)
	}
	return nil
}

func resolvedPath(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(abs)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(abs) == abs {
			return "", err
		}
		tail = append(tail, filepath.Base(abs))
		abs = filepath.Dir(abs)
	}
}

func containsPath(parent, child string) bool {
	info, _ := os.Stat(parent)
	for {
		if parent == child {
			return true
		}
		if other, err := os.Stat(child); err == nil && info != nil && os.SameFile(info, other) {
			return true
		}
		if filepath.Dir(child) == child {
			return false
		}
		child = filepath.Dir(child)
	}
}
