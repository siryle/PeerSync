//go:build windows

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/ui"
)

func Available() bool { return true }

func alert(msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	box := user32.NewProc("MessageBoxW")
	title, _ := syscall.UTF16PtrFromString("PeerSync")
	text, _ := syscall.UTF16PtrFromString(msg)
	const mbOK = 0x00000000
	const mbIcon = 0x00000030
	box.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbOK|mbIcon)
}

func openWindow(page string, ctrl *ui.Controller) error {
	home, err := config.Home()
	if err != nil {
		return err
	}
	debug := os.Getenv("PEERSYNC_DEBUG") == "1"
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     debug,
		AutoFocus: true,
		DataPath:  filepath.Join(home, "webview"),
		WindowOptions: webview2.WindowOptions{
			Title:  "PeerSync",
			Width:  1080,
			Height: 720,
			IconId: 1,
			Center: true,
		},
	})
	if w == nil {
		return errors.New("需要安装 Microsoft Edge WebView2 运行时。Windows 11 通常已经自带，也可以从微软网站安装 Evergreen Runtime。")
	}
	defer w.Destroy()
	ctrl.SetPicker(func() (string, error) {
		return pickFolder(w)
	})
	w.SetSize(880, 560, webview2.HintMin)
	w.SetSize(1080, 720, webview2.HintNone)
	w.Navigate(page)
	w.Run()
	return nil
}

func pickFolder(w webview2.WebView) (string, error) {
	type result struct {
		path string
		err  error
	}
	ch := make(chan result, 1)
	w.Dispatch(func() {
		path, err := browseFolder(uintptr(w.Window()))
		ch <- result{path, err}
	})
	res := <-ch
	return res.path, res.err
}

type browseInfoW struct {
	owner       uintptr
	root        uintptr
	displayName *uint16
	title       *uint16
	flags       uint32
	callback    uintptr
	lParam      uintptr
	image       int32
}

func browseFolder(owner uintptr) (string, error) {
	ole32 := syscall.NewLazyDLL("ole32.dll")
	shell32 := syscall.NewLazyDLL("shell32.dll")
	ole32.NewProc("OleInitialize").Call(0)

	title, err := syscall.UTF16PtrFromString("选择要同步的文件夹")
	if err != nil {
		return "", err
	}
	var display [260]uint16
	info := browseInfoW{
		owner:       owner,
		displayName: &display[0],
		title:       title,
		flags:       0x0001 | 0x0010 | 0x0040, // directories, edit box, newer dialog
	}
	pidl, _, _ := shell32.NewProc("SHBrowseForFolderW").Call(uintptr(unsafe.Pointer(&info)))
	if pidl == 0 {
		return "", ui.ErrCancelled
	}
	defer ole32.NewProc("CoTaskMemFree").Call(pidl)

	var path [260]uint16
	ok, _, _ := shell32.NewProc("SHGetPathFromIDListW").Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	if ok == 0 {
		return "", errors.New("没有拿到文件夹路径")
	}
	return syscall.UTF16ToString(path[:]), nil
}
