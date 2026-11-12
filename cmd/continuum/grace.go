package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/rules"
)

const graceUsage = `usage: continuum grace [AGENT [MINUTES|off|default]]

How long Continuum waits past a reset for the agent to resume by itself
before it continues the session (only while the agent's process is running).
  continuum grace                 show the current waits
  continuum grace claude 10       wait 10 minutes for Claude
  continuum grace claude off      don't wait: continue right after the reset
  continuum grace claude default  back to the built-in wait`

// cmdGrace shows or sets native_grace_seconds per agent in config.json,
// keeping every other setting in the file as it is.
func cmdGrace(args []string) error {
	p := core.GetPaths()
	r, _ := rules.Cached(p.Rules)
	if len(args) == 0 {
		cfg, _ := core.LoadConfig(p.Config)
		for _, name := range r.HarnessNames() {
			h := r.Harness(name)
			if len(h.NativeCommands) == 0 && h.NativeGraceSeconds == nil {
				continue
			}
			sec, src := h.Grace(r.Defaults), "built-in"
			if v, ok := cfg.NativeGrace[name]; ok {
				sec, src = v, "your setting"
			}
			fmt.Printf("%-8s %s (%s)\n", name+":", describeGrace(sec), src)
		}
		fmt.Println("\nChange with `continuum grace AGENT MINUTES|off|default`.")
		return nil
	}
	if len(args) != 2 {
		return errors.New(graceUsage)
	}
	agent, val := args[0], strings.ToLower(args[1])
	if _, ok := r.Harnesses[agent]; !ok {
		return fmt.Errorf("unknown agent %q (one of: %s)", agent, strings.Join(r.HarnessNames(), ", "))
	}
	seconds := -1 // -1: remove the setting
	switch val {
	case "off", "0", "none", "no":
		seconds = 0
	case "default", "reset":
	default:
		m, err := strconv.ParseFloat(strings.TrimSuffix(val, "m"), 64)
		if err != nil || m < 0 || m > 24*60 {
			return errors.New(graceUsage)
		}
		seconds = int(m * 60)
	}

	raw := map[string]any{}
	if data, err := os.ReadFile(p.Config); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("%s is not valid JSON, not changing it: %w", p.Config, err)
		}
	}
	grace, _ := raw["native_grace_seconds"].(map[string]any)
	if grace == nil {
		grace = map[string]any{}
	}
	if seconds < 0 {
		delete(grace, agent)
	} else {
		grace[agent] = seconds
	}
	if len(grace) == 0 {
		delete(raw, "native_grace_seconds")
	} else {
		raw["native_grace_seconds"] = grace
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	if err := p.Ensure(); err != nil {
		return err
	}
	if err := core.WriteFileAtomic(p.Config, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if seconds < 0 {
		seconds = r.Harness(agent).Grace(r.Defaults)
	}
	fmt.Printf("%s: %s. Sessions already waiting are re-planned within seconds.\n", agent, describeGrace(seconds))
	return nil
}

func describeGrace(sec int) string {
	if sec <= 0 {
		return "no wait: continues right after the reset"
	}
	if sec%60 == 0 {
		return fmt.Sprintf("waits %d min past the reset for the agent's own resume", sec/60)
	}
	return fmt.Sprintf("waits %ds past the reset for the agent's own resume", sec)
}

// setConfigKey sets (or, with value nil, removes) one key in config.json,
// keeping every other setting in the file.
func setConfigKey(p core.Paths, key string, value any) error {
	raw := map[string]any{}
	if data, err := os.ReadFile(p.Config); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("%s is not valid JSON, not changing it: %w", p.Config, err)
		}
	}
	if value == nil {
		if _, ok := raw[key]; !ok {
			return nil
		}
		delete(raw, key)
	} else {
		raw[key] = value
	}
	if err := p.Ensure(); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	return core.WriteFileAtomic(p.Config, append(data, '\n'), 0o600)
}
