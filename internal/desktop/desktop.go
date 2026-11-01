// Package desktop opens the PeerSync window on macOS and Windows.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/ui"
)

// Run serves the desktop UI and syncs until the native application exits.
func Run() error {
	if !Available() {
		return errors.New("图形界面只在 macOS 和 Windows 上提供")
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(home, "peersync.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctrl, err := ui.NewController(home, logger)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	page, err := ui.Serve(ctx, ctrl)
	if err != nil {
		return err
	}
	logger.Info("window", "url", page)
	ctrl.Start()
	defer ctrl.Stop()
	return openWindow(page, ctrl)
}

// ReportError prints an error and, when there is no terminal, shows it in a dialog.
func ReportError(err error) {
	EnsureConsole()
	fmt.Fprintln(os.Stderr, "peersync:", err)
	if !stderrIsTTY() {
		alert(err.Error())
	}
}

func stderrIsTTY() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
