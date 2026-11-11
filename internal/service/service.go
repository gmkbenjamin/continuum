// Package service registers the Continuum daemon to start at login:
// a launchd agent on macOS, a systemd user unit on Linux and a scheduled task
// on Windows.
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/proc"
)

const (
	label    = "dev.continuum.daemon"
	unitName = "continuum.service"
	taskName = "Continuum"
)

// Plan describes what Install will do, for --dry-run and for tests.
type Plan struct {
	File     string
	Content  string
	Commands [][]string
}

func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// MakePlan builds the service definition for this OS.
func MakePlan(bin string, p core.Paths) (Plan, error) {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		file := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
		content := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + label + `</string>
  <key>ProgramArguments</key>
  <array><string>` + xmlEsc(bin) + `</string><string>daemon</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Standard</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>` + xmlEsc(proc.AugmentedPATH()) + `</string>
    <key>CONTINUUM_HOME</key><string>` + xmlEsc(p.Home) + `</string>
  </dict>
  <key>StandardOutPath</key><string>` + xmlEsc(p.Log) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEsc(p.Log) + `</string>
</dict>
</plist>
`
		uid := strconv.Itoa(os.Getuid())
		return Plan{File: file, Content: content, Commands: [][]string{
			{"launchctl", "bootout", "gui/" + uid + "/" + label},
			{"launchctl", "bootstrap", "gui/" + uid, file},
		}}, nil
	case "linux", "freebsd":
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".config")
		}
		file := filepath.Join(dir, "systemd", "user", unitName)
		content := fmt.Sprintf(`[Unit]
Description=Continuum - keeps AI coding agents working through usage limits

[Service]
ExecStart=%s daemon
Restart=on-failure
RestartSec=10
Environment=PATH=%s
Environment=CONTINUUM_HOME=%s

[Install]
WantedBy=default.target
`, systemdQuote(bin), proc.AugmentedPATH(), p.Home)
		return Plan{File: file, Content: content, Commands: [][]string{
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", "--now", unitName},
		}}, nil
	case "windows":
		tr := `"` + bin + `" daemon`
		return Plan{Commands: [][]string{
			{"schtasks", "/Create", "/F", "/TN", taskName, "/SC", "ONLOGON", "/RL", "LIMITED", "/TR", tr},
			{"schtasks", "/Run", "/TN", taskName},
		}}, nil
	}
	return Plan{}, fmt.Errorf("automatic start is not supported on %s; run `continuum daemon` from your login scripts", runtime.GOOS)
}

func systemdQuote(s string) string {
	if strings.ContainsAny(s, " \t\"'\\") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}

// Install writes and loads the service.
func Install(bin string, p core.Paths) (Plan, error) {
	plan, err := MakePlan(bin, p)
	if err != nil {
		return plan, err
	}
	if plan.File != "" {
		if err := os.MkdirAll(filepath.Dir(plan.File), 0o755); err != nil {
			return plan, err
		}
		if err := core.WriteFileAtomic(plan.File, []byte(plan.Content), 0o644); err != nil {
			return plan, err
		}
	}
	for i, c := range plan.Commands {
		_, err := proc.Run(30*time.Second, "", c...)
		// The first launchctl command unloads a previous copy and may fail.
		if err != nil && !(runtime.GOOS == "darwin" && i == 0) {
			return plan, fmt.Errorf("%s: %w", strings.Join(c, " "), err)
		}
	}
	return plan, nil
}

// Uninstall stops and removes the service.
func Uninstall(p core.Paths) error {
	plan, err := MakePlan("continuum", p)
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		_, _ = proc.Run(30*time.Second, "", "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+label)
	case "windows":
		_, _ = proc.Run(30*time.Second, "", "schtasks", "/End", "/TN", taskName)
		_, _ = proc.Run(30*time.Second, "", "schtasks", "/Delete", "/F", "/TN", taskName)
	default:
		_, _ = proc.Run(30*time.Second, "", "systemctl", "--user", "disable", "--now", unitName)
	}
	if plan.File != "" {
		if err := os.Remove(plan.File); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Installed reports whether the service definition exists.
func Installed(p core.Paths) bool {
	plan, err := MakePlan("continuum", p)
	if err != nil {
		return false
	}
	if plan.File != "" {
		_, err := os.Stat(plan.File)
		return err == nil
	}
	_, err = proc.Run(10*time.Second, "", "schtasks", "/Query", "/TN", taskName)
	return err == nil
}
