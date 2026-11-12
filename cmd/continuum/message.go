package main

// `continuum message`: view and change what Continuum types to continue a
// session: a default, per agent, per folder and per session.

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
)

func cmdMessage(args []string) error {
	sub := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "show", "list", "ls":
		return showMessages()
	case "set":
		return setMessage(args)
	case "reset", "clear", "unset", "rm":
		return resetMessage(args)
	case "preview", "test":
		return previewMessage(args)
	}
	return fmt.Errorf("unknown `continuum message %s` (show, set, reset, preview; see `continuum message -h`)", sub)
}

type msgTarget struct{ agent, session, folder *string }

// resolveTarget turns --agent/--session/--folder into what to change.
// It returns the agent name for an agent target, or a scope entry.
func (t msgTarget) resolve(r *rules.Rules) (string, *scope.Entry, error) {
	n := 0
	for _, v := range []*string{t.agent, t.session, t.folder} {
		if *v != "" {
			n++
		}
	}
	if n > 1 {
		return "", nil, errors.New("use only one of --agent, --session and --folder")
	}
	switch {
	case *t.agent != "":
		if _, ok := r.Harnesses[*t.agent]; !ok {
			return "", nil, fmt.Errorf("unknown agent %q (one of: %s)", *t.agent, strings.Join(r.HarnessNames(), ", "))
		}
		return *t.agent, nil, nil
	case *t.session != "":
		e, err := resolveEntry(*t.session, knownSessions())
		if err != nil {
			return "", nil, err
		}
		return "", &e, nil
	case *t.folder != "":
		f := *t.folder
		if !strings.HasPrefix(f, "/") && !strings.HasPrefix(f, "~") && !strings.HasPrefix(f, ".") {
			f = "./" + f
		}
		e, err := resolveEntry(f, nil)
		if err != nil {
			return "", nil, err
		}
		return "", &e, nil
	}
	return "", nil, nil // the default
}

func setMessage(args []string) error {
	fs := newFlagSet("message")
	agent := fs.String("agent", "", "Set it for one `AGENT` (claude, codex, gemini, ...).")
	session := fs.String("session", "", "Set it for one session: its `ID` or name (see `continuum sessions`).")
	folder := fs.String("folder", "", "Set it for every session in `FOLDER` and below.")
	outage := fs.Bool("outage", false, "Set the message used after a temporary API error, instead of the usage-limit one.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	text, err := messages.Clean(strings.Join(fs.Args(), " "))
	if err != nil {
		return fmt.Errorf("%v: usage: continuum message set [--agent A | --session ID | --folder F] [--outage] \"TEXT\"", err)
	}
	p := core.GetPaths()
	r, _ := rules.Cached(p.Rules)
	m, err := messages.Load(p)
	if err != nil {
		return fmt.Errorf("%s is unreadable: %v", messages.Path(p), err)
	}
	agentName, entry, err := msgTarget{agent, session, folder}.resolve(r)
	if err != nil {
		return err
	}
	pair := messages.Pair{Text: text}
	if *outage {
		pair = messages.Pair{Outage: text}
	}
	what := "usage-limit message"
	if *outage {
		what = "outage message"
	}
	switch {
	case agentName != "":
		old := m.Agents[agentName]
		if *outage {
			old.Outage = text
		} else {
			old.Text = text
		}
		m.Agents[agentName] = old
		fmt.Printf("%s %s set.\n", r.Harness(agentName).DisplayName, what)
	case entry != nil:
		m.Set(*entry, pair)
		fmt.Printf("%s set for %s.\n", strings.ToUpper(what[:1])+what[1:], entry.String())
	default:
		if *outage {
			m.Default.Outage = text
		} else {
			m.Default.Text = text
		}
		fmt.Printf("Default %s set.\n", what)
	}
	return m.Save(p)
}

func resetMessage(args []string) error {
	fs := newFlagSet("message")
	agent := fs.String("agent", "", "Reset the message for one `AGENT`.")
	session := fs.String("session", "", "Reset the message for one session (`ID` or name).")
	folder := fs.String("folder", "", "Reset the message for `FOLDER`.")
	outage := fs.Bool("outage", false, "Reset only the outage message.")
	all := fs.Bool("all", false, "Reset every message you set, back to the built-in ones.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	r, _ := rules.Cached(p.Rules)
	m, _ := messages.Load(p)
	if *all {
		m = &messages.Messages{Agents: map[string]messages.Pair{}}
		fmt.Println("All messages reset to the built-in ones.")
		return m.Save(p)
	}
	agentName, entry, err := msgTarget{agent, session, folder}.resolve(r)
	if err != nil {
		if *session == "" {
			return err
		}
		e := scope.Entry{Session: *session} // a session that is gone
		entry = &e
	}
	switch {
	case agentName != "":
		old := m.Agents[agentName]
		if *outage {
			old.Outage = ""
		} else {
			old = messages.Pair{}
		}
		if old.Text == "" && old.Outage == "" {
			delete(m.Agents, agentName)
		} else {
			m.Agents[agentName] = old
		}
		fmt.Printf("%s message reset.\n", r.Harness(agentName).DisplayName)
	case entry != nil:
		if !m.Clear(*entry, *outage) {
			return fmt.Errorf("no message is set for %s", entry.String())
		}
		fmt.Printf("Message reset for %s.\n", entry.String())
	default:
		if *outage {
			m.Default.Outage = ""
		} else {
			m.Default = messages.Pair{}
		}
		fmt.Println("Default message reset to the built-in one.")
	}
	return m.Save(p)
}

func previewMessage(args []string) error {
	fs := newFlagSet("message")
	outage := fs.Bool("outage", false, "Show the message used after a temporary API error.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: continuum message preview [--outage] ID|NAME")
	}
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	m, _ := messages.Load(p)
	sessions := knownSessions()
	e, err := resolveEntry(fs.Arg(0), sessions)
	if err != nil {
		return err
	}
	j := &core.Job{Harness: e.Harness, SessionID: e.Session, Cwd: e.Dir}
	for _, s := range sessions {
		if s.ID == e.Session {
			j.Cwd = s.Cwd
		}
	}
	c := m.Resolve(j, *outage, cfg, r)
	fmt.Printf("%s would get (from %s):\n\n  %s\n", e.String(), c.Source, c.Text)
	return nil
}

func clip(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func showMessages() error {
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	m, err := messages.Load(p)
	if err != nil {
		fmt.Printf("warning: %s is unreadable (%v); the built-in messages are used\n\n", tildePath(messages.Path(p)), err)
	}
	q := func(s string) string {
		if s == "" {
			return "(not set)"
		}
		return `"` + clip(s, 72) + `"`
	}
	fmt.Println("Resume messages. The most specific one is used:")
	fmt.Println("session › folder › agent › default › built-in.")
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "  built-in\t%s\n", q(r.Defaults.ResumePrompt))
	fmt.Fprintf(tw, "  default\t%s\n", q(m.Default.Text))
	if m.Default.Outage != "" {
		fmt.Fprintf(tw, "  default (outage)\t%s\n", q(m.Default.Outage))
	}
	agents := make([]string, 0, len(m.Agents))
	for a := range m.Agents {
		agents = append(agents, a)
	}
	for a := range cfg.ResumePrompts {
		if !strings.HasSuffix(a, "_transient") {
			if _, ok := m.Agents[a]; !ok {
				agents = append(agents, a)
			}
		}
	}
	sort.Strings(agents)
	for _, a := range agents {
		pr := m.Agents[a]
		if pr.Text == "" && cfg.ResumePrompts[a] != "" {
			pr.Text = cfg.ResumePrompts[a]
		}
		if pr.Text != "" {
			fmt.Fprintf(tw, "  agent %s\t%s\n", a, q(pr.Text))
		}
		if pr.Outage != "" {
			fmt.Fprintf(tw, "  agent %s (outage)\t%s\n", a, q(pr.Outage))
		}
	}
	for _, rule := range m.Rules {
		if rule.Text != "" {
			fmt.Fprintf(tw, "  %s\t%s\n", rule.Entry.String(), q(rule.Text))
		}
		if rule.Outage != "" {
			fmt.Fprintf(tw, "  %s (outage)\t%s\n", rule.Entry.String(), q(rule.Outage))
		}
	}
	tw.Flush()

	if sessions := knownSessions(); len(sessions) > 0 {
		fmt.Println("\nOpen and recent sessions would get:")
		tw = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		for _, s := range sessions {
			c := m.Resolve(&core.Job{Harness: s.Harness, SessionID: s.ID, Cwd: s.Cwd}, false, cfg, r)
			label := s.Name
			if label == "" {
				label = tildePath(s.Cwd)
			}
			fmt.Fprintf(tw, "  %s %s\t%s\t%s\n", s.Harness, shortID(s.ID), label, c.Source)
		}
		tw.Flush()
	}
	fmt.Println("\nChange with `continuum message set [--agent|--session|--folder] \"…\"`.")
	fmt.Println("Placeholders: {agent}, {folder}. Messages are typed as one line.")
	return nil
}
