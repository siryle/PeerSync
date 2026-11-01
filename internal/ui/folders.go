package ui

import (
	"fmt"
	"os"
	"path/filepath"
)

type folderEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type folderList struct {
	Path    string        `json:"path"`
	Parent  string        `json:"parent"`
	Folders []folderEntry `json:"folders"`
}

// listFolders browses the machine running PeerSync, including container mounts.
func listFolders(path string) (folderList, error) {
	if path == "" {
		path = os.Getenv("PEERSYNC_PATH")
		if path == "" {
			path = string(filepath.Separator)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return folderList{}, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return folderList{}, fmt.Errorf("无法读取文件夹，请检查路径和访问权限：%w", err)
	}
	result := folderList{Path: abs, Parent: filepath.Dir(abs), Folders: []folderEntry{}}
	for _, entry := range entries {
		child := filepath.Join(abs, entry.Name())
		isDir := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(child)
			isDir = err == nil && info.IsDir()
		}
		if isDir {
			result.Folders = append(result.Folders, folderEntry{Name: entry.Name(), Path: child})
		}
	}
	return result, nil
}
