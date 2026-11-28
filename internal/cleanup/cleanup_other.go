//go:build !darwin && !linux && !windows

package cleanup

import (
	"errors"
	"os"
	"time"
)

var errUnsupported = errors.New("process cleanup is supported on macOS, Linux, and Windows")

func checkPrivilege() error {
	if os.Getuid() == 0 || os.Getuid() != os.Geteuid() {
		return errors.New("run cleanup as your regular user")
	}
	return nil
}

func bootID() (string, error) { return "", errUnsupported }
func discover(time.Time, *Finding) ([]Finding, map[int]bool, error) {
	return nil, nil, errUnsupported
}
func snapshot() (inventory, error)                             { return nil, errUnsupported }
func terminate(time.Time, Finding, map[int]bool) (bool, error) { return false, errUnsupported }
