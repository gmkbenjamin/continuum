//go:build windows

package main

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// windowsDocumentsDirs returns candidate Documents directories on Windows,
// checking FOLDERID_Documents first (covers OneDrive Known Folder Move when
// under home) and falling back to <home>\Documents.
func windowsDocumentsDirs(home string) []string {
	var dirs []string
	cleanHome := filepath.Clean(home)
	if kf, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0); err == nil && kf != "" {
		cleanKF := filepath.Clean(kf)
		prefix := cleanHome + string(filepath.Separator)
		if strings.EqualFold(cleanKF, cleanHome) || (len(cleanKF) >= len(prefix) && strings.EqualFold(cleanKF[:len(prefix)], prefix)) {
			dirs = append(dirs, cleanKF)
		}
	}
	fallback := filepath.Join(cleanHome, "Documents")
	if len(dirs) == 0 || !strings.EqualFold(dirs[0], fallback) {
		dirs = append(dirs, fallback)
	}
	return dirs
}
