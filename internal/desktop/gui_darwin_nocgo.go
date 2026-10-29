//go:build darwin && !cgo

package desktop

import (
	"errors"

	"github.com/netcraft/peersync/internal/ui"
)

func Available() bool { return false }

func alert(string) {}

func pickFolder() (string, error) {
	return "", errors.New("这个二进制没有窗口")
}

func openWindow(string, *ui.Controller) error {
	return errors.New("这个 Mac 二进制没有窗口。请打开 PeerSync.app")
}
