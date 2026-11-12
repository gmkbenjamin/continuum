package main

// `continuum diagnose`: one report with everything needed to understand a
// problem, safe to attach to a bug report.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/service"
	"github.com/continuum-dev/continuum/internal/store"
)

var longID = regexp.MustCompile(`\b([0-9a-fA-F]{8})(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}\b`)

// redactor shortens personal details: home folder, user name, session ids.
func redactor() func(string) string {
	home, _ := os.UserHomeDir()
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return func(s string) string {
		if home != "" {
			s = strings.ReplaceAll(s, home, "~")
		}
		if name != "" && len(name) > 2 {
			s = regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(name)+`\b`).ReplaceAllString(s, "<user>")
		}
		return longID.ReplaceAllString(s, "$1…")
	}
}

func cmdDiagnose(args []string) error {
	fs := newFlagSet("diagnose")
	out := fs.String("o", "", "Write the report to `FILE` instead of printing it.")
	lines := fs.Int("lines", 150, "How many of the last log `LINES` to include.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	red := redactor()
	var b strings.Builder
	w := func(format string, a ...any) { b.WriteString(red(fmt.Sprintf(format, a...)) + "\n") }

	p := core.GetPaths()
	cfg, cfgErr := core.LoadConfig(p.Config)
	r, rulesErr := rules.Cached(p.Rules)

	w("# Continuum diagnostic report")
	w("")
	w("- generated: %s", time.Now().Format(time.RFC3339))
	w("- version: %s (%s/%s, %s)", core.Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	w("- home: %s", p.Home)

	w("\n## Install")
	if rec, ok := daemon.LoadInstall(p); ok {
		mode := "background"
		if rec.Standalone {
			mode = "standalone"
		}
		w("- installed %s, version %s, mode %s", rec.At.Format("2006-01-02"), rec.Version, mode)
		w("- hooks for: %s; shell hook: %v; login item chosen: %v", strings.Join(rec.Hooks, ", "), rec.Shell, rec.Service)
	} else {
		w("- not installed (no install record)")
	}
	w("- daemon running: %v; login item present: %v", daemonRunning(p), service.Installed(p))
	for _, rc := range allRCs() {
		if data, err := os.ReadFile(rc.Path); err == nil && strings.Contains(string(data), blockStart) {
			w("- shell hook in %s", rc.Path)
		}
	}

	w("\n## Settings")
	if cfgErr != nil {
		w("- config.json unreadable: %v", cfgErr)
	}
	if data, err := os.ReadFile(p.Config); err == nil {
		var raw map[string]any
		if json.Unmarshal(data, &raw) == nil {
			keys := make([]string, 0, len(raw))
			for k := range raw {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v := raw[k]
				switch k {
				case "resume_prompts", "reset_hints_command":
					v = "(set)"
				}
				js, _ := json.Marshal(v)
				w("- %s: %s", k, js)
			}
		}
	} else {
		w("- config.json: none (defaults)")
	}
	w("- log level: %s", map[bool]string{true: "debug", false: "info"}[cfg.Debug()])
	if sc, err := scope.Load(p); err == nil {
		w("- scope: %s (%d chosen, %d excluded)", sc.Mode, len(sc.Chosen), len(sc.Excluded))
	}
	if _, err := os.Stat(p.Rules); err == nil {
		w("- rules.json override: present%s", map[bool]string{true: " but INVALID: " + fmt.Sprint(rulesErr), false: ""}[rulesErr != nil])
	}

	w("\n## Agents")
	if r != nil {
		for _, t := range hooks.Targets(cfg) {
			h := r.Harness(t.Harness)
			bin, err := deliver.Binary(cfg, h)
			if err != nil {
				w("- %s: not installed", h.DisplayName)
				continue
			}
			ver, _ := proc.Run(15*time.Second, "", bin, "--version")
			inst := "none"
			if rec, ok := daemon.LoadInstall(p); ok {
				inst, _ = hooks.Status(t, rec.Bin)
			}
			w("- %s %s (%s): hooks %s", h.DisplayName, strings.TrimSpace(firstLineOf(ver)), bin, inst)
			var routes []string
			for _, nc := range h.NativeCommands {
				ok, _ := deliver.Supported(bin, nc)
				routes = append(routes, fmt.Sprintf("%s=%v", nc.Name, ok))
			}
			w("  - resume commands: %s", strings.Join(routes, ", "))
			if ls := h.LiveSessions; ls != nil {
				if len(ls.Command) > 0 || ls.RegistryDir != "" {
					list, ok := live.Default().List(h, live.Query{Bin: bin, Homes: map[string]string{"claude_home": cfg.ClaudeDir(), "codex_home": cfg.CodexDir()}})
					w("  - open sessions: %d (from its session list, readable: %v)", len(list), ok)
				} else if ls.ScanProcesses {
					w("  - open sessions: found by process scan")
				}
			}
		}
		var others []string
		for _, name := range r.HarnessNames() {
			h := r.Harness(name)
			if name == "claude" || name == "codex" || h.Binary == "" {
				continue
			}
			if _, err := proc.LookPath(h.Binary); err == nil {
				others = append(others, h.DisplayName)
			}
		}
		w("- other agents installed: %s", orNone(others))
		procs, _ := proc.List()
		var apps []string
		for _, name := range r.HarnessNames() {
			for _, a := range r.Harness(name).DesktopApps {
				state := "not running"
				for _, pr := range procs {
					if a.MatchesProcess(pr.Line) {
						state = "running"
					}
				}
				apps = append(apps, a.Name+" "+state)
			}
		}
		for _, a := range r.TerminalApps {
			for _, pr := range procs {
				if a.MatchesProcess(pr.Line) {
					apps = append(apps, a.Name+" running")
					break
				}
			}
		}
		w("- apps: %s", orNone(apps))
		var muxes []string
		for _, m := range r.MultiplexersByPriority() {
			for _, a := range m.SendText {
				if a.Text != "" {
					if _, err := proc.LookPath(a.Text); err == nil {
						muxes = append(muxes, m.Name)
					}
					break
				}
			}
		}
		w("- multiplexers installed: %s", orNone(muxes))
	}

	w("\n## Update checks")
	c := daemon.LoadCompat(p)
	for name, ac := range c.Agents {
		w("- %s %s: checked %s; problems: %s; repaired: %s", name, ac.Version, ac.CheckedAt.Format(time.RFC3339), orNone(ac.Problems), orNone(ac.Repaired))
	}
	for name, dc := range c.Desktop {
		w("- %s: session list covers it for version %q (seen %s)", name, dc.CoveredVersion, dc.SeenAt.Format(time.RFC3339))
	}

	w("\n## Resumes")
	if jobs, err := store.LoadJobs(p.Jobs); err == nil {
		counts := map[string]int{}
		for _, j := range jobs {
			counts[j.State]++
		}
		w("- by state: %v", counts)
	}
	w("- recent history:")
	for _, l := range lastLines(p.History, 25) {
		var e map[string]any
		if json.Unmarshal([]byte(l), &e) != nil {
			continue
		}
		delete(e, "message")
		js, _ := json.Marshal(e)
		w("  %s", js)
	}

	w("\n## daemon.log (last %d lines)", *lines)
	w("```")
	for _, l := range lastLines(p.Log, *lines) {
		w("%s", l)
	}
	w("```")
	if hl := lastLines(filepath.Join(p.Home, "hooks.log"), 50); len(hl) > 0 {
		w("\n## hooks.log (last 50 lines)")
		w("```")
		for _, l := range hl {
			w("%s", l)
		}
		w("```")
	}
	w("\nPersonal paths are shortened to ~, your user name to <user>, and session ids to")
	w("their first 8 characters; prompts and limit messages are left out. Please read it")
	w("before sharing.")

	if *out != "" {
		if err := os.WriteFile(*out, []byte(b.String()), 0o600); err != nil {
			return err
		}
		fmt.Printf("Report written to %s. Please read it before sharing.\n", *out)
		return nil
	}
	fmt.Print(b.String())
	return nil
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}

func lastLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var ring []string
	for sc.Scan() {
		ring = append(ring, sc.Text())
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	return ring
}
