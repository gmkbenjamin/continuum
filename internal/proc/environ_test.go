//go:build darwin || linux

package proc

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// The test binary runs itself as a helper process (system binaries such as
// /bin/sleep are protected on macOS; agents are not).
func TestMain(m *testing.M) {
	if os.Getenv("CONTINUUM_ENV_HELPER") == "1" {
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestEnvironOfAnotherProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "CONTINUUM_ENV_HELPER=1", "CONTINUUM_ENV_TEST=pane 42")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	time.Sleep(200 * time.Millisecond)
	env := Environ(cmd.Process.Pid)
	if env["CONTINUUM_ENV_TEST"] != "pane 42" {
		t.Fatalf("got %q (%d vars)", env["CONTINUUM_ENV_TEST"], len(env))
	}
	if Environ(999999) != nil {
		t.Fatal("environment of a missing process")
	}
}
