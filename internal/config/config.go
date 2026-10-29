// Package config is the on-disk client configuration. Paths follow the
// operating system convention unless PEERSYNC_HOME is set.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// FileName is the config file inside the home directory.
const FileName = "config.json"

// IdentityFile is the device key file inside the home directory.
const IdentityFile = "identity.json"

// Config is the client configuration.
type Config struct {
	DeviceName string  `json:"device_name"`
	ListenPort int     `json:"listen_port"`
	Server     string  `json:"server"`
	Shares     []Share `json:"shares"`
}

// Share is one synchronized directory.
type Share struct {
	Label    string `json:"label"`
	Path     string `json:"path"`
	ShareID  string `json:"share_id"`
	ShareKey string `json:"share_key"`
}

// Home returns the configuration directory.
func Home() (string, error) {
	if h := os.Getenv("PEERSYNC_HOME"); h != "" {
		return h, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "peersync"), nil
}

// Default returns a config with the local discovery server and port 42420.
func Default(name string) Config {
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		name = "peersync"
	}
	return Config{
		DeviceName: name,
		ListenPort: 42420,
		Server:     "ws://127.0.0.1:8080",
		Shares:     []Share{},
	}
}

// Load reads a config file. A missing file returns a default config and exists=false.
func Load(path string) (Config, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(""), false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, false, err
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = 42420
	}
	if cfg.Server == "" {
		cfg.Server = "ws://127.0.0.1:8080"
	}
	if cfg.Shares == nil {
		cfg.Shares = []Share{}
	}
	return cfg, true, nil
}

// Save writes the config atomically.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IndexPath is where a folder's index is stored, outside the synced tree.
func IndexPath(home, shareID string) string {
	return filepath.Join(home, "indexes", shareID+".json")
}

// StatusPath is the file `peersync status` reads.
func StatusPath(home string) string {
	return filepath.Join(home, "status.json")
}

// Platform reports the OS name stored in status output.
func Platform() string {
	return runtime.GOOS
}
