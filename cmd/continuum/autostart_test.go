package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/service"
	"github.com/continuum-dev/continuum/internal/store"
)

// fakeService stands in for launchd/systemd and the daemon: tests never
// touch the real ones.
type fakeService struct{ loginItem, started, stopped int }

func stubService(t *testing.T) *fakeService {
	f := &fakeService{}
	oi, ou, on, os_, ost := serviceInstall, serviceUninstall, serviceInstalled, startDaemon, stopRunning
	t.Cleanup(func() {
		serviceInstall, serviceUninstall, serviceInstalled, startDaemon, stopRunning = oi, ou, on, os_, ost
	})
	serviceInstall = func(bin string, p core.Paths) (service.Plan, error) {
		f.loginItem = 1
		return service.Plan{File: "/fake/LaunchAgents/dev.continuum.daemon.plist"}, nil
	}
	serviceUninstall = func(p core.Paths) error { f.loginItem = 0; return nil }
	serviceInstalled = func(p core.Paths) bool { return f.loginItem == 1 }
	startDaemon = func() error { f.started++; return nil }
	stopRunning = func(p core.Paths) { f.stopped++ }
	return f
}

func writeInstall(t *testing.T, rec daemon.Install) core.Paths {
	p := core.GetPaths()
	p.Ensure()
	if err := saveInstall(p, &rec); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDisableThenEnable(t *testing.T) {
	sandboxHome(t)
	f := stubService(t)
	f.loginItem = 1
	p := writeInstall(t, daemon.Install{Bin: "/x/continuum", Service: true, Version: core.Version})

	out := capture(t, func() {
		if err := cmdDisable(nil); err != nil {
			t.Fatal(err)
		}
	})
	cfg, _ := core.LoadConfig(p.Config)
	rec, _ := daemon.LoadInstall(p)
	if cfg.AutostartEnabled() || f.loginItem != 0 || !rec.NoAutostart || !strings.Contains(out, "Auto-start is off") {
		t.Fatalf("disable: autostart=%v login=%d rec=%+v\n%s", cfg.AutostartEnabled(), f.loginItem, rec, out)
	}
	if !strings.Contains(autostartState(p, cfg), "off") {
		t.Fatalf("state: %s", autostartState(p, cfg))
	}
	// update keeps it off: no login item planned.
	upd := installOptionsFromRecord(rec)
	if upd.Service || !upd.NoAutostart {
		t.Fatalf("update would turn auto-start back on: %+v", upd)
	}

	os.WriteFile(stoppedPath(p), []byte("x"), 0o600)
	out = capture(t, func() {
		if err := cmdEnable(nil); err != nil {
			t.Fatal(err)
		}
	})
	cfg, _ = core.LoadConfig(p.Config)
	rec, _ = daemon.LoadInstall(p)
	if !cfg.AutostartEnabled() || f.loginItem != 1 || rec.NoAutostart || !rec.Service || f.started != 1 || stoppedByUser(p) {
		t.Fatalf("enable: autostart=%v login=%d rec=%+v started=%d\n%s", cfg.AutostartEnabled(), f.loginItem, rec, f.started, out)
	}
	if !strings.Contains(autostartState(p, cfg), "at login") {
		t.Fatalf("state: %s", autostartState(p, cfg))
	}
}

// Standalone mode has nothing to disable; enable switches it to background.
func TestAutostartFromStandalone(t *testing.T) {
	sandboxHome(t)
	f := stubService(t)
	p := writeInstall(t, daemon.Install{Bin: "/x/continuum", Standalone: true, Version: core.Version})
	setConfigKey(p, "autostart_daemon", false)
	out := capture(t, func() { cmdDisable(nil) })
	if !strings.Contains(out, "standalone") {
		t.Fatalf("disable in standalone:\n%s", out)
	}
	capture(t, func() { cmdEnable(nil) })
	rec, _ := daemon.LoadInstall(p)
	cfg, _ := core.LoadConfig(p.Config)
	if rec.Standalone || !cfg.AutostartEnabled() || f.loginItem != 1 {
		t.Fatalf("enable from standalone: %+v autostart=%v", rec, cfg.AutostartEnabled())
	}
}

func TestEnableNeedsAnInstall(t *testing.T) {
	sandboxHome(t)
	stubService(t)
	if err := cmdEnable(nil); err == nil {
		t.Fatal("enabled without an install")
	}
}

func TestInteractiveAutostart(t *testing.T) {
	sandboxHome(t)
	f := stubService(t)
	f.loginItem = 1
	p := writeInstall(t, daemon.Install{Bin: "/x/continuum", Service: true, Version: core.Version})
	out := runScript(t, "7\n2\n\nq\n") // daemon menu → turn auto-start off
	if cfg, _ := core.LoadConfig(p.Config); cfg.AutostartEnabled() || f.loginItem != 0 {
		t.Fatalf("not disabled:\n%s", out)
	}
	if !strings.Contains(out, "auto-start off") {
		t.Fatalf("menu doesn't show it:\n%s", out)
	}
	runScript(t, "7\n2\n\nq\n") // and back on
	if cfg, _ := core.LoadConfig(p.Config); !cfg.AutostartEnabled() || f.loginItem != 1 {
		t.Fatal("not enabled again")
	}
}

// Removing the login item stops the daemon it started (launchd, systemd and
// Task Scheduler stop their job); --keep-running starts it again.
func TestDisableKeepRunningSurvivesTheLoginItem(t *testing.T) {
	sandboxHome(t)
	f := stubService(t)
	f.loginItem = 1
	p := writeInstall(t, daemon.Install{Bin: "/x/continuum", Service: true, Version: core.Version})
	lock, err := store.Lock(p.Lock) // the running daemon
	if err != nil {
		t.Fatal(err)
	}
	serviceUninstall = func(core.Paths) error { f.loginItem = 0; lock.Close(); return nil }
	out := capture(t, func() {
		if err := cmdDisable([]string{"--keep-running"}); err != nil {
			t.Fatal(err)
		}
	})
	if f.started != 1 || f.stopped != 0 || !strings.Contains(out, "still running") {
		t.Fatalf("started=%d stopped=%d\n%s", f.started, f.stopped, out)
	}
}

// The systemd user unit's daemon is started by the user's service manager,
// not pid 1: that start (at login) must count, so `continuum stop` lasts only
// until the next login there too. A shell in a terminal systemd started is
// not the service.
func TestLaunchedBySystemdUserUnit(t *testing.T) {
	if os.Getppid() == 1 {
		t.Skip("the test's parent is pid 1")
	}
	t.Setenv("XPC_SERVICE_NAME", "")
	t.Setenv("INVOCATION_ID", "0123456789abcdef")
	t.Setenv("MANAGERPID", strconv.Itoa(os.Getppid()))
	if !launchedByService() {
		t.Error("started by the systemd user manager: not recognised")
	}
	t.Setenv("MANAGERPID", strconv.Itoa(os.Getppid()+100000))
	if launchedByService() {
		t.Error("a process whose parent isn't the service manager counted as the service")
	}
}

// `daemonRunning` takes the lock for an instant; a daemon starting right
// then must not give up as if another one ran.
func TestDaemonLockOutlastsAProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	probe, err := store.Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); probe.Close() }()
	f, err := takeLock(path)
	if err != nil {
		t.Fatalf("gave up while a probe held the lock: %v", err)
	}
	f.Close()
}

// Without a login item, nothing starts the daemon at the next login: status
// must not say it does.
func TestStatusAfterStopWithoutALoginItem(t *testing.T) {
	sandboxHome(t)
	stubService(t) // no login item
	p := writeInstall(t, daemon.Install{Bin: "/x/continuum", Version: core.Version})
	os.WriteFile(stoppedPath(p), []byte("x"), 0o600)
	out := capture(t, func() { cmdStatus(nil) })
	if !strings.Contains(out, "stopped by you") || strings.Contains(out, "next login") {
		t.Fatalf("status:\n%s", out)
	}
}

// With auto-start off, update starts no daemon that isn't running, and its
// plan must not say it will.
func TestUpdatePlanWithAutostartOff(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Load("")
	o := installOptionsFromRecord(&daemon.Install{Bin: "/x/continuum", Version: core.Version, Copy: true, Shell: true, NoAutostart: true})
	for _, it := range installPlanItems(o, p, cfg, r) {
		if it.What == "background daemon" && it.Action == "start" {
			t.Fatalf("the plan says it will start the daemon: %+v", it)
		}
		if it.What == "login item" && it.Action != "remove" {
			t.Fatalf("the plan adds a login item: %+v", it)
		}
	}
}
