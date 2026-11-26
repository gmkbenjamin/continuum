package daemon

// Staying compatible while vendors ship updates.
//
// Agents and their desktop apps update themselves, often weekly, and an update
// can change what Continuum relies on: a hook removed from settings, a CLI flag
// renamed, a session list that no longer includes desktop sessions. The daemon
// notices an update (the agent's executable changes), re-checks everything it
// relies on, repairs what it can (re-adds its own hooks), falls back to the
// safe behaviour for the rest, and tells the user once, in plain words.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/store"
)

// Compat is ~/.continuum/compat.json: what Continuum learned about the agents
// installed here, and what it last checked.
type Compat struct {
	// Agents: last seen executable fingerprint and check result per agent.
	Agents map[string]*AgentCompat `json:"agents"`
	// Desktop: per desktop app, the bundled agent version for which the
	// agent's own session list was seen to include the app's sessions.
	Desktop map[string]*DesktopCompat `json:"desktop"`
}

type AgentCompat struct {
	Fingerprint string    `json:"fingerprint"`
	Version     string    `json:"version,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
	Problems    []string  `json:"problems,omitempty"`
	Repaired    []string  `json:"repaired,omitempty"`
	// ExactPID: the executable fingerprint for which a process its hooks
	// reported was seen holding that session in the agent's own session list,
	// so its hooks name the right process (see ExactPIDs).
	ExactPID string `json:"exact_pid,omitempty"`
	// ExactPIDFirst: the first agreeing process, as "fingerprint|pid": trust
	// takes a second, different one.
	ExactPIDFirst string `json:"exact_pid_first,omitempty"`
}

// ExactPIDs: whether harness's hooks are known to name the exact process
// holding a session: Claude Code's always do (CLAUDE_PID); another agent's
// once that was seen to hold here, for its current build. Only then can a
// second process holding a conversation be told to be an out-of-date copy.
func (c *Compat) ExactPIDs(harness string) bool {
	if harness == "claude" {
		return true
	}
	if c == nil {
		return false
	}
	ac := c.Agents[harness]
	return ac != nil && ac.ExactPID != "" && ac.ExactPID == ac.Fingerprint
}

type DesktopCompat struct {
	CoveredVersion string    `json:"covered_version,omitempty"`
	SeenAt         time.Time `json:"seen_at,omitempty"`
}

// Install is ~/.continuum/install.json, written by `continuum install`: what
// the user set up, so the daemon can restore it after an agent update.
type Install struct {
	Bin     string    `json:"bin"`
	Hooks   []string  `json:"hooks"`
	At      time.Time `json:"at"`
	Version string    `json:"version,omitempty"`
	// The user's choices, re-applied by `continuum update`.
	Shell   bool   `json:"shell"`
	Service bool   `json:"service"`
	Copy    bool   `json:"copy"`
	Only    string `json:"only,omitempty"`
	// Standalone: no background daemon; Continuum runs only while
	// `continuum watch` is open.
	Standalone bool `json:"standalone,omitempty"`
	// NoAutostart: `continuum disable`. Background mode, but nothing starts
	// the daemon by itself (no login item, hooks don't start it); you start
	// it with `continuum start`. `continuum enable` turns this off.
	NoAutostart bool `json:"no_autostart,omitempty"`
	// NoSkill: install --no-skill; agents don't get the Continuum skill.
	NoSkill bool `json:"no_skill,omitempty"`
}

func compatPath(p core.Paths) string  { return filepath.Join(p.Home, "compat.json") }
func InstallPath(p core.Paths) string { return filepath.Join(p.Home, "install.json") }

func LoadCompat(p core.Paths) *Compat {
	c := &Compat{}
	_ = store.LoadJSON(compatPath(p), c)
	if c.Agents == nil {
		c.Agents = map[string]*AgentCompat{}
	}
	if c.Desktop == nil {
		c.Desktop = map[string]*DesktopCompat{}
	}
	return c
}

func LoadInstall(p core.Paths) (*Install, bool) {
	data, err := os.ReadFile(InstallPath(p))
	if err != nil {
		return nil, false
	}
	var in Install
	if json.Unmarshal(data, &in) != nil {
		return nil, false
	}
	return &in, true
}

// appSupport is where desktop apps keep their data on this OS.
func appSupport() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support")
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			return v
		}
	}
	return filepath.Join(home, ".config")
}

func (d *Daemon) homes() map[string]string {
	m := d.Config.Homes()
	m["app_support"] = appSupport()
	return m
}

// bundledVersion returns the newest version folder of a desktop app's
// bundled agent ("" if unknown).
func bundledVersion(app *rules.DesktopApp, homes map[string]string) string {
	if app.VersionDir == "" {
		return ""
	}
	dir := app.VersionDir
	for k, v := range homes {
		dir = strings.ReplaceAll(dir, "{"+k+"}", v)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool { return versionLess(names[i], names[j]) })
	if len(names) == 0 {
		return ""
	}
	return names[len(names)-1]
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		var x, y int
		fmt.Sscan(pa[i], &x)
		fmt.Sscan(pb[i], &y)
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

// trustRegistry reports, per desktop app of h, whether the agent's own
// session list is known to include that app's sessions for the app's current
// version. Learned by observeDesktop; reset when the app updates.
func (d *Daemon) trustRegistry(h *rules.Harness) map[string]bool {
	out := map[string]bool{}
	if d.Compat == nil {
		return out
	}
	for _, a := range h.DesktopApps {
		dc := d.Compat.Desktop[a.Name]
		if dc == nil || dc.CoveredVersion == "" {
			continue
		}
		cur := bundledVersion(a, d.homes())
		switch {
		case cur != "" && cur == dc.CoveredVersion:
			out[a.Name] = true
		case cur == "" && d.Now().Sub(dc.SeenAt) < 14*24*time.Hour:
			out[a.Name] = true
		}
	}
	return out
}

// observeExactPID learns whether ev's agent's hooks name the process its own
// session list does for the session: once they agree for two different
// processes, for this build of the agent, the out-of-date copy checks apply
// to it.
func (d *Daemon) observeExactPID(ev core.Event) {
	if d.Compat == nil || ev.PID <= 0 || ev.SessionID == "" || !strings.HasPrefix(ev.Source, "hook:") ||
		ev.Kind == core.KindSessionEnd || d.Compat.ExactPIDs(ev.Harness) || d.LiveFn == nil {
		return
	}
	ac := d.Compat.Agents[ev.Harness]
	if ac == nil || ac.Fingerprint == "" {
		return // not checked yet (checkCompat records the build first)
	}
	// Asking can be slow (Muse Code runs a command): back off while it
	// doesn't agree, from a minute up to an hour, afresh for each build.
	if d.pidProbe == nil {
		d.pidProbe = map[string]*probeState{}
	}
	pr := d.pidProbe[ev.Harness]
	if pr == nil || pr.fingerprint != ac.Fingerprint {
		pr = &probeState{fingerprint: ac.Fingerprint, gap: time.Minute}
		d.pidProbe[ev.Harness] = pr
	}
	first := ""
	if f, ok := strings.CutPrefix(ac.ExactPIDFirst, ac.Fingerprint+"|"); ok {
		first = f
	}
	if first == fmt.Sprint(ev.PID) {
		return // agreed already: a different process has to agree too
	}
	if d.Now().Before(pr.next) {
		return
	}
	st := d.LiveFn(&core.Job{Harness: ev.Harness, SessionID: ev.SessionID, PID: ev.PID, Cwd: ev.Cwd})
	// Only an entry of the agent's own list naming this session counts, not a
	// process-scan guess or the "process is alive" stand-in.
	if st == nil || !st.Authoritative || !st.Listed || st.Session == nil || st.Session.PID != ev.PID {
		pr.next = d.Now().Add(pr.gap)
		if pr.gap *= 2; pr.gap > time.Hour {
			pr.gap = time.Hour
		}
		return
	}
	d.compatDirty = true
	if first == "" {
		ac.ExactPIDFirst = fmt.Sprintf("%s|%d", ac.Fingerprint, ev.PID)
		return
	}
	ac.ExactPID = ac.Fingerprint
	d.logf("%s: its hooks name the process its own session list does; out-of-date copies of a conversation are now recognised for it", d.name(ev.Harness))
}

// probeState paces observeExactPID for one agent.
type probeState struct {
	fingerprint string
	next        time.Time
	gap         time.Duration
}

// observeDesktop learns whether the agent's own session list includes desktop
// sessions: when a running desktop session shows up in it, the list is
// trusted for that app version (so a session missing from it is closed, and
// can be continued without waiting for the app to quit).
func (d *Daemon) observeDesktop() {
	if d.Compat == nil {
		return
	}
	f := live.Default()
	for _, name := range d.Rules.HarnessNames() {
		h := d.Rules.Harness(name)
		for _, a := range h.DesktopApps {
			entries := f.DesktopEntries(h, live.Query{Homes: d.homes()}, a)
			if len(entries) == 0 {
				continue
			}
			ver := entries[0].Version
			if b := bundledVersion(a, d.homes()); b != "" {
				ver = b
			}
			dc := d.Compat.Desktop[a.Name]
			if dc == nil {
				dc = &DesktopCompat{}
				d.Compat.Desktop[a.Name] = dc
			}
			if dc.CoveredVersion != ver {
				d.logf("%s %s: its sessions appear in %s's own session list; trusting that list for it", a.Name, ver, h.DisplayName)
			}
			dc.CoveredVersion, dc.SeenAt = ver, d.Now()
			d.compatDirty = true
		}
	}
}

// repairHooks rewrites Continuum's hooks in t.Path when they are missing or
// outdated, for agents the user installed hooks for (not when Continuum is
// switched off: then it leaves settings files alone).
func (d *Daemon) repairHooks(t hooks.Target, inst *Install, installed bool) []string {
	if !installed || !d.Config.IsEnabled() || !containsStr(inst.Hooks, t.Harness) {
		return nil
	}
	st, _ := hooks.Status(t, inst.Bin)
	if st == hooks.StateCurrent {
		return nil
	}
	verb := "re-added its hooks to "
	if st == hooks.StateOutdated {
		verb = "updated its hooks in "
	}
	changed, err := hooks.Install(t, inst.Bin, false)
	if err != nil {
		d.logf("hooks in %s need updating but could not be written: %v", t.Path, err)
		return nil
	}
	if !changed {
		return nil
	}
	return []string{verb + t.Path}
}

// fingerprint identifies an installed agent build: its resolved executable
// path, size and modification time. Updating the agent changes it.
func fingerprint(bin string) string {
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		real = bin
	}
	st, err := os.Stat(real)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s|%d|%d", real, st.Size(), st.ModTime().Unix())
}

// checkCompat runs when an agent was installed or updated (and once a day):
// it re-checks hooks, resume commands and session listing, restores hooks an
// update removed, and tells the user what changed.
func (d *Daemon) checkCompat(force bool) {
	if d.Compat == nil {
		return
	}
	inst, installed := LoadInstall(d.Paths)
	for _, t := range hooks.Targets(d.Config) {
		h := d.Rules.Harness(t.Harness)
		bin, err := deliver.Binary(d.Config, h)
		if err != nil {
			continue
		}
		fp := fingerprint(bin)
		ac := d.Compat.Agents[t.Harness]
		if ac == nil {
			ac = &AgentCompat{}
			d.Compat.Agents[t.Harness] = ac
		}
		first := ac.Fingerprint == ""
		// Hooks are cheap to check (one small file): always. Something other
		// than an agent update may rewrite the settings file too.
		repaired := d.repairHooks(t, inst, installed)
		if !force && ac.Fingerprint == fp && d.Now().Sub(ac.CheckedAt) < 24*time.Hour {
			if len(repaired) > 0 {
				d.logf("%s: %s", h.DisplayName, strings.Join(repaired, "; "))
				d.notifyText(h.DisplayName, "Continuum "+strings.Join(repaired, "; ")+". All set.")
			}
			continue
		}
		updated := !first && ac.Fingerprint != fp
		ac.Fingerprint, ac.CheckedAt = fp, d.Now()
		ac.Version = versionFromPath(fp)
		before := strings.Join(ac.Problems, "; ")
		ac.Problems, ac.Repaired = nil, repaired

		// Resume commands: probe the installed agent's --help again.
		usable := 0
		for _, nc := range h.NativeCommands {
			if ok, _ := deliver.Supported(bin, nc); ok {
				usable++
			}
		}
		if len(h.NativeCommands) > 0 && usable == 0 {
			ac.Problems = append(ac.Problems, "none of its resume commands are available any more; only sessions in a terminal or pane can be continued")
		}
		// Session listing: the source of the never-a-copy guarantee.
		if ls := h.LiveSessions; ls != nil && len(ls.Command) > 0 {
			if _, ok := live.Default().List(h, live.Query{Bin: bin, Homes: d.homes()}); !ok {
				ac.Problems = append(ac.Problems, "it no longer reports its open sessions; Continuum now assumes sessions are open and only types into terminals")
			}
		}
		d.compatDirty = true
		now := strings.Join(ac.Problems, "; ")
		name := h.DisplayName
		switch {
		case len(ac.Repaired) > 0:
			d.logf("%s updated: %s", name, strings.Join(ac.Repaired, "; "))
			d.notifyText(name+" updated", "Continuum "+strings.Join(ac.Repaired, "; ")+". All set.")
		}
		if now != "" && now != before {
			d.logf("%s compatibility: %s", name, now)
			d.notifyText(name+" changed", "Continuum adjusted: "+now+". Run `continuum doctor` for details.")
		} else if now == "" && before != "" {
			d.logf("%s compatibility restored", name)
		}
		if updated && now == "" && len(ac.Repaired) == 0 {
			d.logf("%s updated (%s): everything Continuum relies on still works", name, ac.Version)
		}
	}
}

func versionFromPath(fp string) string {
	path := strings.SplitN(fp, "|", 2)[0]
	base := filepath.Base(path)
	if len(base) > 0 && base[0] >= '0' && base[0] <= '9' {
		return base
	}
	return ""
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (d *Daemon) notifyText(title, body string) {
	if !d.Config.NotifyEnabled() || d.NotifyFn == nil {
		return
	}
	d.NotifyFn("Continuum · "+title, body)
}

func (d *Daemon) saveCompat() {
	if d.Compat == nil || !d.compatDirty {
		return
	}
	if err := store.SaveJSON(compatPath(d.Paths), d.Compat); err != nil {
		d.logf("save compat: %v", err)
	}
	d.compatDirty = false
}

// AppSupport and BundledVersion are exported for `continuum doctor`.
func AppSupport() string { return appSupport() }

func BundledVersion(a *rules.DesktopApp, homes map[string]string) string {
	return bundledVersion(a, homes)
}

// probeTerminalApps asks each running scriptable terminal app a harmless
// question once, when the daemon starts (normally right after install, with
// the user at the computer). macOS asks the user once whether Continuum may
// control the app; better now than at 3am with the screen locked.
func (d *Daemon) probeTerminalApps(apps map[string]*rules.TerminalApp) {
	if runtime.GOOS != "darwin" || len(apps) == 0 {
		return
	}
	procs, _ := proc.List()
	for _, a := range apps {
		running := false
		for _, p := range procs {
			if a.MatchesProcess(p.Line) {
				running = true
			}
		}
		if !running || len(a.Find) == 0 {
			continue
		}
		argv := make([]string, len(a.Find))
		for i, x := range a.Find {
			argv[i] = strings.ReplaceAll(x, "{tty}", "/dev/none")
		}
		if _, err := proc.Run(60*time.Second, "", argv...); err != nil {
			d.logf("%s: Continuum may not control it yet (%v); sessions there are resumed once allowed in System Settings › Privacy & Security › Automation", a.Name, firstLine(err.Error()))
		} else {
			d.logf("%s: Continuum can type into its terminals", a.Name)
		}
	}
}
