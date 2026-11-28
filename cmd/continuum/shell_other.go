//go:build !windows

package main

import "path/filepath"

func windowsDocumentsDirs(home string) []string {
	return []string{filepath.Join(home, "Documents")}
}
