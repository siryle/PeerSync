package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSharedPathsRejectAliasesAndNestedRoots(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip(err)
	}
	for _, second := range []string{root, alias, filepath.Join(root, "nested"), filepath.Join(alias, "nested")} {
		cfg := Default("test")
		cfg.Shares = []Share{{Path: root}, {Path: second}}
		filename := filepath.Join(t.TempDir(), FileName)
		if err := Save(filename, cfg); err == nil {
			t.Fatalf("saved overlapping paths %s and %s", root, second)
		}
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(filename); err == nil {
			t.Fatalf("loaded overlapping paths %s and %s", root, second)
		}
	}
	if err := ValidatePaths([]Share{{Path: root}, {Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
}
