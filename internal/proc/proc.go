// Package proc wraps the few process operations Continuum needs on every OS.
package proc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Run executes argv with a timeout and returns trimmed stdout.
func Run(timeout time.Duration, stdin string, argv ...string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("empty command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	hideWindow(cmd)
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), errors.New("timed out: " + strings.Join(argv, " "))
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return out.String(), errors.New(err.Error() + ": " + msg)
	}
	return out.String(), nil
}

// StartDetached starts argv in its own process group/session with output to
// logPath and does not wait for it.
func StartDetached(argv []string, dir string, env []string, logPath string) (int, error) {
	if len(argv) == 0 {
		return 0, errors.New("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	devnull, err := os.Open(os.DevNull)
	if err == nil {
		cmd.Stdin = devnull
	}
	if logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			cmd.Stdout = f
			cmd.Stderr = f
		}
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

// LookPath finds a binary, also checking common install locations that may be
// missing from a daemon's PATH (launchd and systemd start with a bare PATH).
func LookPath(name string) (string, error) {
	if name == "" {
		return "", errors.New("no binary")
	}
	if strings.ContainsAny(name, `/\`) {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, dir := range extraDirs(home) {
		for _, cand := range candidates(dir, name) {
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand, nil
			}
		}
	}
	return "", errors.New(name + " not found")
}

// AugmentedPATH returns PATH plus the usual user install locations.
func AugmentedPATH() string {
	home, _ := os.UserHomeDir()
	parts := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))
	seen := map[string]bool{}
	var out []string
	for _, p := range append(parts, extraDirs(home)...) {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return strings.Join(out, string(os.PathListSeparator))
}
