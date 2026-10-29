//go:build windows

package desktop

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func EnsureConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsole := kernel32.NewProc("GetConsoleWindow")
	hwnd, _, _ := getConsole.Call()
	if hwnd != 0 {
		return
	}
	attach := kernel32.NewProc("AttachConsole")
	const attachParent = ^uintptr(0)
	r, _, _ := attach.Call(attachParent)
	if r == 0 {
		return
	}
	in, err := windows.Open("CONIN$", windows.O_RDWR, 0)
	if err == nil {
		os.Stdin = os.NewFile(uintptr(in), "stdin")
	}
	out, err := windows.Open("CONOUT$", windows.O_RDWR, 0)
	if err == nil {
		os.Stdout = os.NewFile(uintptr(out), "stdout")
	}
	errOut, err := windows.Open("CONOUT$", windows.O_RDWR, 0)
	if err == nil {
		os.Stderr = os.NewFile(uintptr(errOut), "stderr")
	}
}
