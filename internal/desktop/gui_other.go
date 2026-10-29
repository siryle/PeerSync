//go:build !darwin && !windows

package desktop

import (
	"errors"

	"github.com/netcraft/peersync/internal/ui"
)

func Available() bool { return false }

func alert(string) {}

func pickFolder() (string, error) {
	return "", errors.New("图形界面只在 macOS 和 Windows 上提供")
}

func openWindow(string, *ui.Controller) error {
	return errors.New("图形界面只在 macOS 和 Windows 上提供")
}
