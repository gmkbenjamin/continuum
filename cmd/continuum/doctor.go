package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/service"
)

// selfTest holds real messages agents have printed. If a rules change stops
// recognising one, doctor says so.
var selfTest = []struct {
	harness, text, kind string
	hasReset            bool
}{
	{"claude", "You've hit your session limit · resets 10am (Australia/Sydney)", core.KindUsageLimit, true},
	{"claude", "You've hit your weekly limit · resets Nov 14, 6pm (Australia/Sydney)", core.KindUsageLimit, true},
	{"claude", "Claude usage limit reached. Your limit will reset at 5pm (America/New_York).", core.KindUsageLimit, true},
	{"claude", "API Error: 529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}", core.KindTransient, false},
	{"codex", "You’ve hit your usage limit. Visit https://chatgpt.com/settings/usage to purchase more credits or try again at 5:53 PM.", core.KindUsageLimit, true},
	{"codex", "You've hit your usage limit. Upgrade to Pro or try again in 2 hours 13 minutes.", core.KindUsageLimit, true},
	{"antigravity", "⚠ Third-party quota reached. Please switch to Gemini models or upgrade your subscription to increase your limits. Resets in 2h30m50s.", core.KindUsageLimit, true},
	{"generic", "Rate limit exceeded. Your quota resets in 45 minutes.", core.KindUsageLimit, true},
}

func cmdDoctor() int {
	p := core.GetPaths()
	cfg, cfgErr := core.LoadConfig(p.Config)
	problems := 0
	ok := func(format string, a ...any) { fmt.Printf("  ✓ "+format+"\n", a...) }
	warn := func(format string, a ...any) { fmt.Printf("  ! "+format+"\n", a...) }
	bad := func(format string, a ...any) { problems++; fmt.Printf("  ✗ "+format+"\n", a...) }

	fmt.Printf("Continuum %s · home %s\n\n", core.Version, p.Home)

	fmt.Println("Core")
	if cfgErr != nil {
		bad("config.json: %v", cfgErr)
	} else {
		ok("config loaded")
	}
	r, rErr := rules.Load(p.Rules)
	if rErr != nil {
		bad("rules: %v (built-in rules in use)", rErr)
	} else {
		ok("rules loaded (version %d)", r.Version)
	}
	if rec, inst := daemon.LoadInstall(p); inst && rec.Standalone {
		ok("standalone mode: nothing runs in the background")
		if daemonRunning(p) {
			ok("running now (`continuum watch`)")
		} else {
			fmt.Println("  · not running now: run `continuum watch` to resume stalled sessions")
		}
	} else {
		if daemonRunning(p) {
			ok("daemon running (`continuum logs -f` follows it)")
		} else {
			warn("daemon not running: `continuum start` (hooks and `continuum run` also start it)")
		}
		if service.Installed(p) {
			ok("starts at login")
		} else {
			warn("does not start at login: `continuum install`")
		}
	}
	if cfg.Debug() {
		ok("debug logging on: %s, %s", tildePath(p.Log), tildePath(filepath.Join(p.Home, "hooks.log")))
	}

	fmt.Println("\nRecognition self-test")
	now := time.Now()
	failed := 0
	for _, s := range selfTest {
		h := r.Harness(s.harness)
		m := h.Classify(s.text, 10)
		_, resetOK := resettime.Parse(s.text, now, time.Local)
		if m == nil || m.Kind != s.kind || (s.hasReset && !resetOK) {
			failed++
			bad("%s: no longer recognises %q", s.harness, s.text)
		}
	}
	if failed == 0 {
		ok("%d known limit messages recognised, reset times parsed", len(selfTest))
	}

	fmt.Println("\nAgents")
	for _, t := range hooks.Targets(cfg) {
		h := r.Harness(t.Harness)
		bin, err := deliver.Binary(cfg, h)
		if err != nil {
			warn("%s: not installed", h.DisplayName)
			continue
		}
		ver, _ := proc.Run(15*time.Second, "", bin, "--version")
		ok("%s %s (%s)", h.DisplayName, strings.TrimSpace(firstLineOf(ver)), bin)
		if inst, detail := hooks.Installed(t); inst {
			ok("  hooks installed in %s", t.Path)
		} else {
			warn("  hooks not installed (%s): `continuum install`", detail)
		}
		usable := 0
		for _, nc := range h.NativeCommands {
			if sup, why := deliver.Supported(bin, nc); sup {
				usable++
				ok("  can resume with %s", nc.Name)
			} else {
				warn("  %s unavailable: %s", nc.Name, why)
			}
		}
		if usable == 0 && len(h.NativeCommands) > 0 {
			warn("  no CLI resume route: only sessions in a pane or `continuum run` can be resumed")
		}
		if h.LiveSessions != nil {
			q := live.Query{Bin: bin, Homes: cfg.Homes()}
			if list, good := live.Default().List(h, q); good {
				ok("  can tell which sessions are open (%d open now), so it never starts a copy", len(list))
			} else if h.LiveSessions.ScanProcesses {
				ok("  can tell which sessions are open (process scan), so it never starts a copy")
			} else {
				warn("  cannot list open sessions: the CLI route is only used when the hook-recorded process has exited")
			}
		}
		var dir string
		switch t.Harness {
		case "claude":
			dir = filepath.Join(cfg.ClaudeDir(), "projects")
		case "codex":
			dir = filepath.Join(cfg.CodexDir(), "sessions")
		case "antigravity":
			dir = filepath.Join(cfg.AntigravityDataDir(), "brain")
		}
		if dir != "" {
			if _, err := os.Stat(dir); err == nil {
				ok("  watching %s", dir)
			} else {
				warn("  %s does not exist yet", dir)
			}
		}
	}

	fmt.Println("\nDesktop apps")
	compat := daemon.LoadCompat(p)
	procs, _ := proc.List()
	homes := cfg.Homes()
	homes["app_support"] = daemon.AppSupport()
	for _, name := range r.HarnessNames() {
		h := r.Harness(name)
		for _, a := range h.DesktopApps {
			running := false
			for _, pr := range procs {
				if a.MatchesProcess(pr.Line) {
					running = true
				}
			}
			state := "not running"
			if running {
				state = "running"
			}
			ver := daemon.BundledVersion(a, homes)
			if ver != "" {
				state += ", bundled " + h.DisplayName + " " + ver
			}
			ok("%s (%s): its sessions are detected through %s's own files", a.Name, state, h.DisplayName)
			hasReach := false
			for _, nc := range h.NativeCommands {
				if nc.ReachesDesktop {
					hasReach = true
				}
			}
			switch {
			case hasReach:
				ok("  continued inside the app, through %s's local server", h.DisplayName)
			default:
				dc := compat.Desktop[a.Name]
				if dc != nil && dc.CoveredVersion != "" && (ver == "" || ver == dc.CoveredVersion) {
					ok("  continued automatically as soon as the app lets go of a session (%s's session list covers it; confirmed for %s)", h.DisplayName, dc.CoveredVersion)
				} else {
					ok("  continued automatically once the app is closed; never copied while it may hold the session")
					fmt.Printf("    · Continuum learns when %s lists Desktop sessions itself (after you use one), and then continues without waiting for the app to close\n", h.DisplayName)
				}
			}
		}
	}
	if len(compat.Agents) > 0 {
		fmt.Println("\nAgent updates")
		for name, ac := range compat.Agents {
			if len(ac.Problems) > 0 {
				warn("%s %s: %s", name, ac.Version, strings.Join(ac.Problems, "; "))
			} else {
				ok("%s %s: checked %s ago, all Continuum relies on still works", name, ac.Version, time.Since(ac.CheckedAt).Round(time.Minute))
			}
		}
	}

	fmt.Println("\nOther agents")
	hookCmds := map[string]bool{}
	for _, a := range shellHookAgents() {
		hookCmds[a.Command] = true
	}
	others := 0
	withHooks := map[string]bool{} // listed above, under Agents
	for _, t := range hooks.Targets(cfg) {
		withHooks[t.Harness] = true
	}
	for _, name := range r.HarnessNames() {
		h := r.Harness(name)
		if withHooks[name] || name == "generic" || h.Binary == "" {
			continue
		}
		bin, err := proc.LookPath(h.Binary)
		if err != nil {
			continue
		}
		others++
		how := "resumed in tmux panes, and under `continuum run`"
		if hookCmds[h.Binary] {
			how = "wrapped by the shell hook in any terminal, and resumed in tmux panes"
		} else if h.Wrap != nil && !h.InShellHook() {
			how += fmt.Sprintf(" (add %q to shell_hook_agents to wrap it in every terminal)", h.Binary)
		}
		ok("%s (%s): %s", h.DisplayName, bin, how)
	}
	if others == 0 {
		fmt.Println("  · none of the other supported agents is installed")
	}
	fmt.Printf("  · %d agents supported in total: `continuum rules` lists them\n", len(r.HarnessNames())-1)

	fmt.Println("\nTerminals and multiplexers")
	here := mux.LocateFromEnv(r, mux.EnvMap(os.Environ()))
	for _, m := range r.MultiplexersByPriority() {
		name := m.Name
		if len(m.SendText) == 0 {
			continue
		}
		bin := ""
		for _, a := range m.SendText {
			if a.Text != "" {
				bin = a.Text
				break
			}
		}
		if p, err := proc.LookPath(bin); err == nil {
			mark := ""
			if here != nil && here.Kind == name {
				mark = " ← this shell runs here (" + here.Target + ")"
			}
			ok("%s (%s)%s", name, p, mark)
		} else {
			fmt.Printf("  · %s not installed\n", name)
		}
	}
	hooked := false
	for _, rc := range shellRCs() {
		if data, err := os.ReadFile(rc.Path); err == nil && strings.Contains(string(data), blockStart) {
			hooked = true
			ok("any terminal: shell hook in %s wraps installed agents", rc.Path)
		}
	}
	if !hooked && runtime.GOOS != "windows" {
		warn("any other terminal: no shell hook yet (`continuum install`), or start agents with `continuum run -- <agent>`")
	}

	fmt.Println("\nScope and power")
	if sc, err := scope.Load(p); err != nil {
		bad("scope file unreadable: %v", err)
	} else if sc.Mode == scope.ModeChosen {
		ok("resuming chosen sessions only (%d chosen, %d excluded): `continuum scope`", len(sc.Chosen), len(sc.Excluded))
	} else {
		ok("resuming every session (%d excluded): `continuum scope chosen` to pick", len(sc.Excluded))
	}
	if h := cfg.KeepAwake(); h > 0 {
		ok("keeps the computer awake (screen may lock) while a resume is due within %d hours or a resumed session is working", int(h.Hours()))
	} else {
		warn("keep_awake_hours is 0: a sleeping computer will not resume anything until it wakes")
	}
	if cfg.CopiesAllowed() {
		warn("allow_copies is on: the agent CLI may start a second copy of a session that is still open")
	} else {
		ok("never starts a second copy of an open session")
	}

	fmt.Println()
	if problems == 0 {
		fmt.Println("✅ Continuum is ready.")
		return 0
	}
	fmt.Printf("%d problem(s) found.\n", problems)
	return 1
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
