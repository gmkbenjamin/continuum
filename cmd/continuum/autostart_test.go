package main

import (
	"os"
	"strings"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/service"
)

// fakeService stands in for launchd/systemd and the daemon: tests never
// touch the real ones.
type fakeService struct{ loginItem, started, stopped int }

func stubService(t *testing.T) *fakeService {
	f := &fakeService{}
	oi, ou, on, os_, ost := serviceInstall, serviceUninstall, serviceInstalled, startDaemon, stopRunning
	t.Cleanup(func() { serviceInstall, serviceUninstall, serviceInstalled, startDaemon, stopRunning = oi, ou, on, os_, ost })
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
