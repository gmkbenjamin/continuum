//go:build windows

package main

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// windowsDocumentsDirs returns candidate Documents directories on Windows,
// checking FOLDERID_Documents first (covers OneDrive Known Folder Move) and
// falling back to <home>\Documents.
func windowsDocumentsDirs(home string) []string {
	var dirs []string
	if kf, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0); err == nil && kf != "" {
		dirs = append(dirs, kf)
	}
	fallback := filepath.Join(home, "Documents")
	if len(dirs) == 0 || !strings.EqualFold(filepath.Clean(dirs[0]), filepath.Clean(fallback)) {
		dirs = append(dirs, fallback)
	}
	return dirs
}
