//go:build darwin || linux || windows

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

// Arguments come back exactly, spaces inside one argument included: a prompt
// like "help me fix it" must not read as the subcommand `help`.
func TestArgsOfAnotherProcess(t *testing.T) {
	if !EnvironSupported() {
		t.Skip("not on this OS")
	}
	cmd := exec.Command(os.Args[0], "help me fix the tests", "--flag")
	cmd.Env = append(os.Environ(), "CONTINUUM_ENV_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	var got []string
	for i := 0; i < 50 && len(got) < 3; i++ {
		got = Args(cmd.Process.Pid)
		time.Sleep(20 * time.Millisecond)
	}
	if len(got) != 3 || got[1] != "help me fix the tests" || got[2] != "--flag" {
		t.Fatalf("args %q", got)
	}
}
