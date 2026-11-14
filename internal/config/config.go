// Package config is the on-disk client configuration. Paths follow the
// operating system convention unless PEERSYNC_HOME is set.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FileName is the config file inside the home directory.
const FileName = "config.json"

// IdentityFile is the device key file inside the home directory.
const IdentityFile = "identity.json"

// ScanIntervals returns polling and content-verification intervals.
func ScanIntervals() (scan, verify time.Duration, err error) {
	scan, verify = 20*time.Second, 5*time.Minute
	for _, setting := range []struct {
		name  string
		value *time.Duration
	}{
		{"PEERSYNC_SCAN_INTERVAL", &scan},
		{"PEERSYNC_VERIFY_INTERVAL", &verify},
	} {
		text := strings.TrimSpace(os.Getenv(setting.name))
		if text == "" {
			continue
		}
		value, parseErr := time.ParseDuration(text)
		if parseErr != nil || value <= 0 {
			return 0, 0, fmt.Errorf("%s must be a positive duration, for example 5m or 1h", setting.name)
		}
		*setting.value = value
	}
	return scan, verify, nil
}

// Config is the client configuration.
type Config struct {
	DeviceName string  `json:"device_name"`
	ListenPort int     `json:"listen_port"`
	Server     string  `json:"server"`
	Shares     []Share `json:"shares"`
}

// Share is one synchronized directory.
type Share struct {
	Label    string   `json:"label"`
	Path     string   `json:"path"`
	ShareID  string   `json:"share_id"`
	ShareKey string   `json:"share_key"`
	Mode     SyncMode `json:"mode,omitempty"`
	SourceID string   `json:"source_device_id,omitempty"`
}

type SyncMode string

const (
	SendReceive SyncMode = "send-receive"
	SendOnly    SyncMode = "send-only"
	ReceiveOnly SyncMode = "receive-only"
)

func (m SyncMode) Effective() SyncMode {
	if m == "" {
		return SendReceive
	}
	return m
}

func ValidateSync(mode SyncMode, sourceID string) error {
	switch mode.Effective() {
	case SendReceive, SendOnly:
	case ReceiveOnly:
		if strings.TrimSpace(sourceID) == "" {
			return errors.New("receive-only mode requires a source device ID")
		}
	default:
		return errors.New("invalid sync mode")
	}
	return nil
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

// ErrServerInUse means an invite names a different discovery server while
// this device already syncs other folders through the current one.
var ErrServerInUse = errors.New("invite discovery server differs from the one already in use")

// MergeDiscoveryServer keeps the current server when an invite would retarget
// folders that are already syncing. The invite server is used only before the
// first folder is added.
func MergeDiscoveryServer(current, invite string, hasShares bool) (string, error) {
	invite = strings.TrimSpace(invite)
	if invite == "" || sameServer(current, invite) {
		return current, nil
	}
	if hasShares {
		return "", ErrServerInUse
	}
	return invite, nil
}

func sameServer(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
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
