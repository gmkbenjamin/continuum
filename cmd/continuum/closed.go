package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/continuum-dev/continuum/internal/core"
)

const closedUsage = `usage: continuum closed [on|off|default]

Whether Continuum keeps tracking and resuming sessions after they close.
  continuum closed          show the current setting
  continuum closed off      only track open sessions; drop resumes once a session closes
  continuum closed on       track and resume closed sessions (the default)
  continuum closed default  back to the built-in default (on)`

// cmdClosed shows or sets track_closed_sessions in config.json, keeping every
// other setting in the file as it is.
func cmdClosed(args []string) error {
	p := core.GetPaths()
	if len(args) == 0 {
		cfg, _ := core.LoadConfig(p.Config)
		if cfg.ClosedSessionsTracked() {
			src := "built-in default"
			if cfg.TrackClosedSessions != nil {
				src = "your setting"
			}
			fmt.Printf("Closed sessions: tracked (%s). Closed stalled sessions stay queued to resume.\n", src)
		} else {
			fmt.Println("Closed sessions: not tracked (your setting). Continuum drops resumes once a session closes.")
		}
		fmt.Println("Change with `continuum closed on|off|default`.")
		return nil
	}
	if len(args) != 1 {
		return errors.New(closedUsage)
	}
	switch strings.ToLower(args[0]) {
	case "off", "false", "no", "0":
		if err := setConfigKey(p, "track_closed_sessions", false); err != nil {
			return err
		}
		fmt.Println("Closed sessions: not tracked. Only open sessions are tracked and resumed; already-closed pending resumes drop within seconds.")
		return nil
	case "on", "true", "yes", "1":
		if err := setConfigKey(p, "track_closed_sessions", true); err != nil {
			return err
		}
		fmt.Println("Closed sessions: tracked. Closed stalled sessions stay queued to resume.")
		return nil
	case "default", "reset":
		if err := setConfigKey(p, "track_closed_sessions", nil); err != nil {
			return err
		}
		fmt.Println("Closed sessions: tracked (built-in default). Closed stalled sessions stay queued to resume.")
		return nil
	default:
		return errors.New(closedUsage)
	}
}
