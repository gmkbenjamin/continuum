//go:build windows

package deliver

import "errors"

type ptyChild struct {
	rw interface {
		Read([]byte) (int, error)
		Write([]byte) (int, error)
		Close() error
	}
}

func startPTY(argv []string, dir string, env []string) (*ptyChild, error) {
	return nil, errors.New("typing through an attached terminal is not supported on Windows yet")
}

func (c *ptyChild) stop() {}
