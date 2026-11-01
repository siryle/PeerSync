//go:build darwin && cgo

package desktop

/*
#cgo darwin CFLAGS: -fobjc-arc
#cgo darwin LDFLAGS: -framework Cocoa -framework WebKit
#include <stdlib.h>
#include "gui_darwin.h"
*/
import "C"

import (
	"encoding/json"
	"runtime"
	"unsafe"

	"github.com/netcraft/peersync/internal/ui"
)

func init() { runtime.LockOSThread() }

var nativeControls *menuControls

func Available() bool { return true }

func alert(msg string) {
	raw := C.CString(msg)
	defer C.free(unsafe.Pointer(raw))
	C.psShowAlert(raw)
}

func pickFolder() (string, error) {
	p := C.psPickFolder()
	if p == nil {
		return "", ui.ErrCancelled
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoString(p), nil
}

//export peerSyncMenuState
func peerSyncMenuState() *C.char {
	body, _ := json.Marshal(nativeControls.snapshot())
	return C.CString(string(body))
}

//export peerSyncMenuAction
func peerSyncMenuAction(action C.int) {
	nativeControls.act(int(action), func() { C.psFinishQuit() })
}

func openWindow(page string, ctrl *ui.Controller) error {
	ctrl.SetPicker(pickFolder)
	nativeControls = &menuControls{ctrl: ctrl}
	defer func() { nativeControls = nil }()
	title := C.CString("PeerSync")
	raw := C.CString(page)
	defer C.free(unsafe.Pointer(title))
	defer C.free(unsafe.Pointer(raw))
	logo, err := ui.LogoPNG()
	if err != nil {
		return err
	}
	C.psRunApp(title, raw, unsafe.Pointer(&logo[0]), C.int(len(logo)))
	return nil
}
