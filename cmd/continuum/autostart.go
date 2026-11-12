package main

// `continuum enable` and `continuum disable`: whether the background daemon
// starts by itself (at login, and from hooks when an agent needs it).
// `start` and `stop` act on the daemon now; these decide what happens next.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/service"
)

// What these commands use to act on the login item and the daemon. Tests
// replace them, so they never touch the real launchd, systemd or daemon.
var (
	serviceInstall   = service.Install
	serviceUninstall = service.Uninstall
	serviceInstalled = service.Installed
	startDaemon      = cmdStart
	stopRunning      = stopDaemon
)

func saveInstall(p core.Paths, rec *daemon.Install) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return core.WriteFileAtomic(daemon.InstallPath(p), append(data, '\n'), 0o600)
}

func cmdDisable(args []string) error {
	fs := newFlagSet("disable")
	keep := fs.Bool("keep-running", false, "Leave the daemon running now; only stop it from starting by itself.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	rec, installed := daemon.LoadInstall(p)
	if installed && rec.Standalone {
		fmt.Println("Continuum runs standalone: nothing starts by itself already.")
		fmt.Println("It runs while `continuum watch` is open.")
		return nil
	}
	if err := setConfigKey(p, "autostart_daemon", false); err != nil {
		return err
	}
	fmt.Println("hooks:     no longer start the daemon")
	if serviceInstalled(p) {
		if err := serviceUninstall(p); err != nil {
			fmt.Println("login:     couldn't remove the login item:", err)
		} else {
			fmt.Println("login:     login item removed; it won't start at login")
		}
	}
	if installed {
		rec.NoAutostart = true
		if err := saveInstall(p, rec); err != nil {
			return err
		}
	}
	running := daemonRunning(p)
	switch {
	case running && *keep:
		fmt.Println("daemon:    still running (until `continuum stop` or you log out)")
	case running:
		stopRunning(p)
		fmt.Println("daemon:    stopped")
	}
	fmt.Println()
	fmt.Println("✅ Auto-start is off. Nothing starts Continuum by itself now.")
	fmt.Println("   `continuum start` runs it until you stop it or log out, `continuum watch` runs it")
	fmt.Println("   in a terminal, and `continuum enable` turns auto-start back on. Limits that reset")
	fmt.Println("   meanwhile are caught up when it next runs.")
	return nil
}

func cmdEnable(args []string) error {
	fs := newFlagSet("enable")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	rec, installed := daemon.LoadInstall(p)
	if !installed {
		return errors.New("Continuum isn't installed here; run `continuum install`")
	}
	if rec.Standalone {
		fmt.Println("mode:      background (was standalone)")
	}
	if err := setConfigKey(p, "autostart_daemon", nil); err != nil {
		return err
	}
	_ = os.Remove(stoppedPath(p))
	fmt.Println("hooks:     start the daemon when an agent needs it")
	bin := rec.Bin
	if bin == "" {
		bin = self()
	}
	if plan, err := serviceInstall(bin, p); err != nil {
		fmt.Println("login:     couldn't add the login item:", err)
	} else {
		where := plan.File
		if where == "" {
			where = "scheduled task Continuum"
		}
		fmt.Println("login:     starts at login via", tildePath(where))
		rec.Service = true
	}
	rec.Standalone, rec.NoAutostart = false, false
	if err := saveInstall(p, rec); err != nil {
		return err
	}
	// The login item starts it straight away on most systems; make sure.
	for i := 0; i < 20 && !daemonRunning(p); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !daemonRunning(p) {
		if err := startDaemon(); err != nil {
			fmt.Println("daemon:    couldn't start:", err)
		}
	} else {
		fmt.Println("daemon:    running")
	}
	fmt.Println()
	fmt.Println("✅ Auto-start is on: Continuum starts at login and whenever an agent needs it.")
	return nil
}

// autostartState describes, in words, what starts the daemon.
func autostartState(p core.Paths, cfg core.Config) string {
	rec, installed := daemon.LoadInstall(p)
	switch {
	case installed && rec.Standalone:
		return "off (standalone mode)"
	case !cfg.AutostartEnabled():
		return "off (`continuum enable` turns it on)"
	case serviceInstalled(p):
		return "on: at login, and whenever an agent needs it"
	default:
		return "on whenever an agent needs it (not at login)"
	}
}
