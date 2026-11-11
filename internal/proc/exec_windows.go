//go:build windows

package proc

import (
	"errors"
	"os"
	"os/exec"
)

// Exec runs argv attached to this console and exits with its exit code.
func Exec(argv []string) error {
	path, err := LookPath(argv[0])
	if err != nil {
		return err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
