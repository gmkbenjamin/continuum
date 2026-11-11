package deliver

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

func runIn(dir string, env []string, timeout time.Duration, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), errors.New("timed out")
	}
	if err != nil {
		msg := strings.TrimSpace(out.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return out.String(), errors.New(err.Error() + ": " + msg)
	}
	return out.String(), nil
}
