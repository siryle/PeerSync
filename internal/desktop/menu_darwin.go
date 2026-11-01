package desktop

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/netcraft/peersync/internal/ui"
)

const (
	menuToggle = iota + 1
	menuReconnect
	menuQuit
)

type menuFolder struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

type menuState struct {
	Title    string       `json:"title"`
	Summary  string       `json:"summary"`
	Error    string       `json:"error"`
	Version  string       `json:"version"`
	Running  bool         `json:"running"`
	Busy     bool         `json:"busy"`
	Quitting bool         `json:"quitting"`
	Folders  []menuFolder `json:"folders"`
}

func makeMenuState(st ui.State, busy, quitting bool) menuState {
	out := menuState{
		Title: "同步运行中", Error: st.Error, Version: st.Version,
		Running: st.Running, Busy: busy, Quitting: quitting,
		Folders: []menuFolder{},
	}
	peers := map[string]bool{}
	discovery := false
	for _, sh := range st.Shares {
		out.Folders = append(out.Folders, menuFolder{Label: sh.Label, Path: sh.Path})
		discovery = discovery || sh.Discovery
		for _, p := range sh.Peers {
			if st.Running && p.Connected {
				peers[p.ID] = true
			}
		}
	}
	sort.Slice(out.Folders, func(i, j int) bool {
		a, b := out.Folders[i], out.Folders[j]
		if a.Label == b.Label {
			return a.Path < b.Path
		}
		return a.Label < b.Label
	})
	out.Summary = fmt.Sprintf("%d 个文件夹 · %d 台设备已连接", len(out.Folders), len(peers))
	switch {
	case quitting:
		out.Title = "正在退出…"
	case busy:
		out.Title = "正在切换同步…"
	case st.Error != "":
		out.Title = "同步异常"
	case !st.Running:
		out.Title = "同步已暂停"
	case len(out.Folders) == 0:
		out.Title = "尚未添加同步文件夹"
	case !discovery && len(peers) == 0:
		out.Title = "正在连接发现服务"
	}
	return out
}

type menuControls struct {
	ctrl     *ui.Controller
	mu       sync.Mutex
	busy     atomic.Bool
	quitting atomic.Bool
}

func (m *menuControls) snapshot() menuState {
	return makeMenuState(m.ctrl.State(), m.busy.Load(), m.quitting.Load())
}

func (m *menuControls) act(action int, quit func()) {
	// Keep stop/start work off AppKit's main thread and serialize menu actions.
	if action == menuQuit {
		if !m.quitting.CompareAndSwap(false, true) {
			return
		}
		go func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.ctrl.Stop()
			quit()
		}()
		return
	}
	if (action != menuToggle && action != menuReconnect) || m.quitting.Load() || !m.busy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.busy.Store(false)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.quitting.Load() {
			return
		}
		running := m.ctrl.State().Running
		switch action {
		case menuToggle:
			if running {
				m.ctrl.Stop()
			} else {
				m.ctrl.Start()
			}
		case menuReconnect:
			if running {
				m.ctrl.Stop()
				if !m.quitting.Load() {
					m.ctrl.Start()
				}
			}
		}
	}()
}
