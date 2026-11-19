package main

// The Continuum skill: a short guide that agents with skills (Claude Code,
// Codex, Antigravity, and Grok Build, Muse Code, the Cursor CLI and opencode
// through Claude's folder) load when the
// user asks about resuming, so the agent can check and control Continuum
// for its own session. `continuum agent-guide` prints the same for any agent.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/rules"
)

// skillMarker identifies Continuum's skill file, so uninstall removes only it.
const skillMarker = "continuum-skill: 1"

const skillHead = `---
name: continuum
description: Check or control Continuum, which continues this coding session by itself after a usage limit resets or a temporary API error passes. Use when the user asks whether or when this session will be resumed, wants to stop or allow auto-resume, wants to change the message typed when it resumes, or wants to start, stop or turn off Continuum.
metadata:
  ` + skillMarker + `
---

`

const guideBody = `# Continuum

Continuum types a short "continue" message into this session when a usage limit
resets or an API error passes, so the work carries on by itself. Run these with
your shell tool. ` + "`this`" + ` means the session you are running in.

| To | Run |
|---|---|
| See this session's state, and whether it will be resumed | ` + "`continuum this`" + ` (` + "`--json`" + ` for scripts) |
| Never auto-resume this session | ` + "`continuum exclude this`" + ` |
| Allow it again | ` + "`continuum include this`" + ` |
| Set the message typed when it resumes | ` + "`continuum message set --session this \"…\"`" + ` |
| Go back to the default message | ` + "`continuum message reset --session this`" + ` |
| Cancel a scheduled resume | ` + "`continuum cancel this`" + ` |
| Resume now instead of waiting | ` + "`continuum resume-now this`" + ` |
| Every session and its state | ` + "`continuum status`" + ` (` + "`--json`" + `) |
| Stop or start Continuum | ` + "`continuum stop`" + ` / ` + "`continuum start`" + ` |
| Turn auto-start off or on | ` + "`continuum disable`" + ` / ` + "`continuum enable`" + ` |

- Change only what the user asked for, and tell them what you changed.
- The resume message is typed into this session as a new prompt, on one line.
  Write it as an instruction to yourself, for example "Limit reset: re-run the
  failing tests, then continue the migration."
- Every command explains itself with ` + "`-h`" + `.
`

// guideText is the guide with the program's full path, for when ` + "`continuum`" + `
// isn't on the agent's PATH.
func guideText(bin string) string {
	return guideBody + "- If `continuum` isn't found, run it as `" + bin + "`.\n"
}

func cmdAgentGuide(args []string) error {
	fs := newFlagSet("agent-guide")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	fmt.Print(guideText(installedBin()))
	return nil
}

// installedBin is the installed program, else this one.
func installedBin() string {
	p := core.GetPaths()
	if data, err := os.ReadFile(p.BinRecord); err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data))
	}
	if rec, ok := daemon.LoadInstall(p); ok && strings.TrimSpace(rec.Bin) != "" {
		return strings.TrimSpace(rec.Bin)
	}
	return self()
}

// skillTarget is where an agent reads its skills.
type skillTarget struct {
	Agent string // display name
	Dir   string // .../skills/continuum
}

// skillTargets lists the installed agents that read skills.
func skillTargets(cfg core.Config, r *rules.Rules) []skillTarget {
	home, _ := os.UserHomeDir()
	cands := []struct{ harness, dir string }{
		{"claude", filepath.Join(cfg.ClaudeDir(), "skills")},
		{"codex", filepath.Join(cfg.CodexDir(), "skills")},
		{"antigravity", filepath.Join(home, ".gemini", "config", "skills")},
	}
	var out []skillTarget
	for _, c := range cands {
		h := r.Harness(c.harness)
		if h == nil || !cfg.HarnessEnabled(c.harness) {
			continue
		}
		_, binErr := deliver.Binary(cfg, h)
		_, dirErr := os.Stat(filepath.Dir(c.dir))
		if binErr != nil && dirErr != nil {
			continue // the agent isn't installed
		}
		name := h.DisplayName
		if c.harness == "claude" {
			name += " (and Grok Build, Muse Code, the Cursor CLI and opencode, which read its skills)"
		}
		out = append(out, skillTarget{Agent: name, Dir: filepath.Join(c.dir, "continuum")})
	}
	return out
}

// allSkillDirs: every place Continuum may have put its skill, installed
// agents or not (for uninstall).
func allSkillDirs(cfg core.Config) []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(cfg.ClaudeDir(), "skills", "continuum"),
		filepath.Join(cfg.CodexDir(), "skills", "continuum"),
		filepath.Join(home, ".gemini", "config", "skills", "continuum"),
	}
}

func skillFile(dir string) string { return filepath.Join(dir, "SKILL.md") }

// ourSkill: the folder holds Continuum's skill (and nothing of the user's).
func ourSkill(dir string) bool {
	data, err := os.ReadFile(skillFile(dir))
	return err == nil && strings.Contains(string(data), skillMarker)
}

// installSkills writes or refreshes the skill for each agent. A folder of
// that name the user made is left alone.
func installSkills(cfg core.Config, r *rules.Rules, bin string, dry bool) []string {
	content := skillHead + guideText(bin)
	var out []string
	for _, t := range skillTargets(cfg, r) {
		f := skillFile(t.Dir)
		old, err := os.ReadFile(f)
		switch {
		case err == nil && string(old) == content:
			out = append(out, fmt.Sprintf("%-10s skill up to date in %s", "skill:", tildePath(t.Dir)))
			continue
		case err == nil && !strings.Contains(string(old), skillMarker):
			out = append(out, fmt.Sprintf("%-10s %s already has a skill named continuum that isn't Continuum's; left alone", "skill:", tildePath(t.Dir)))
			continue
		}
		if dry {
			out = append(out, fmt.Sprintf("%-10s would add the Continuum skill to %s", "skill:", tildePath(t.Dir)))
			continue
		}
		err = os.MkdirAll(t.Dir, 0o755)
		if err == nil {
			err = core.WriteFileAtomic(f, []byte(content), 0o644)
		}
		if err != nil {
			out = append(out, fmt.Sprintf("%-10s %s: %v", "skill:", tildePath(t.Dir), err))
			continue
		}
		out = append(out, fmt.Sprintf("%-10s %s can now check and control Continuum (%s)", "skill:", t.Agent, tildePath(t.Dir)))
	}
	return out
}

// uninstallSkills removes Continuum's skill wherever it is.
func uninstallSkills(cfg core.Config, dry bool) []string {
	var out []string
	for _, dir := range allSkillDirs(cfg) {
		if !ourSkill(dir) {
			continue
		}
		if !dry {
			_ = os.Remove(skillFile(dir))
			_ = os.Remove(dir)               // only if nothing else is in it
			_ = os.Remove(filepath.Dir(dir)) // the skills folder, if install made it and it's empty
		}
		out = append(out, fmt.Sprintf("%-10s %s %s", "skill:", map[bool]string{true: "would remove", false: "removed"}[dry], tildePath(dir)))
	}
	return out
}
