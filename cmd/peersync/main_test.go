package main

import (
	"path/filepath"
	"testing"

	"github.com/netcraft/peersync/internal/config"
)

func TestEnvDeviceName(t *testing.T) {
	for _, tc := range []struct{ name, existing, force, want string }{
		{"first-start", "", "", "client-a"},
		{"preserve-saved", "saved", "", "saved"},
		{"force", "saved", "1", "client-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PEERSYNC_NAME", "client-a")
			t.Setenv("PEERSYNC_NAME_FORCE", tc.force)
			t.Setenv("PEERSYNC_CREATE", "")
			t.Setenv("PEERSYNC_INVITE", "")
			home := t.TempDir()
			path := filepath.Join(home, config.FileName)
			if tc.existing != "" {
				if err := config.Save(path, config.Default(tc.existing)); err != nil {
					t.Fatal(err)
				}
			}
			if err := applyEnv(home, ""); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DeviceName != tc.want {
				t.Fatalf("name=%q want %q", cfg.DeviceName, tc.want)
			}
		})
	}
}
