//go:build !windows

package proc

import (
	"os"
	"syscall"
)

// Exec replaces the current process with argv (resolved with LookPath).
func Exec(argv []string) error {
	path, err := LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, os.Environ())
}
