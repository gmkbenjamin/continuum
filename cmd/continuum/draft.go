package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/continuum-dev/continuum/internal/core"
)

const draftUsage = `usage: continuum draft [restore|submit|default]

What Continuum does when a chat input box already has unsubmitted text when a
usage limit resets (for sessions running in Continuum's wrapper):
  continuum draft          show the current setting
  continuum draft restore  clear your draft, resume, then put your draft back (default)
  continuum draft submit   press Enter on your existing text first, then resume
  continuum draft default  back to the built-in default (restore)`

// cmdDraft shows or sets draft_handling in config.json, keeping every other
// setting in the file as it is.
func cmdDraft(args []string) error {
	p := core.GetPaths()
	if len(args) == 0 {
		cfg, _ := core.LoadConfig(p.Config)
		src := "built-in default"
		if strings.TrimSpace(cfg.DraftHandling) != "" {
			src = "your setting"
		}
		if cfg.DraftMode() == core.DraftSubmit {
			fmt.Printf("Existing input text: submit (%s). Continuum presses Enter on your existing text first, then sends the resume message.\n", src)
		} else {
			fmt.Printf("Existing input text: restore (%s). Continuum clears your draft, resumes, then puts your draft back.\n", src)
		}
		fmt.Println("Change with `continuum draft restore|submit|default`.")
		return nil
	}
	if len(args) != 1 {
		return errors.New(draftUsage)
	}
	switch strings.ToLower(args[0]) {
	case "restore", "preserve", "keep":
		if err := setConfigKey(p, "draft_handling", core.DraftRestore); err != nil {
			return err
		}
		fmt.Println("Existing input text: restore. Continuum clears your draft, resumes, then puts your draft back.")
		return nil
	case "submit", "send", "enter":
		if err := setConfigKey(p, "draft_handling", core.DraftSubmit); err != nil {
			return err
		}
		fmt.Println("Existing input text: submit. Continuum presses Enter on your existing text first, then sends the resume message.")
		return nil
	case "default", "reset":
		if err := setConfigKey(p, "draft_handling", nil); err != nil {
			return err
		}
		fmt.Println("Existing input text: restore (built-in default). Continuum clears your draft, resumes, then puts your draft back.")
		return nil
	default:
		return errors.New(draftUsage)
	}
}
