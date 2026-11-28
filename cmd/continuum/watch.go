package main

// The daemon, run either in the background (`continuum daemon`, started at
// login or by hooks) or in the foreground with readable output
// (`continuum watch`, the standalone mode).

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/logx"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/store"
	"golang.org/x/term"
)

type daemonOptions struct {
	Once, Debug, Watch bool
}

func runDaemon(o daemonOptions) error {
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	cfg, _ := core.LoadConfig(p.Config)
	lock, err := takeLock(p.Lock)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			if o.Watch {
				return fmt.Errorf("Continuum is already running in the background. Follow it with `continuum logs -f`, or stop it and watch here with `continuum watch --takeover`")
			}
			fmt.Println("continuum daemon is already running")
			return nil
		}
		return err
	}
	defer lock.Close()
	if launchedByService() {
		// Started at login: `continuum stop` lasted until now.
		_ = os.Remove(stoppedPath(p))
	}
	_ = os.WriteFile(p.PID, []byte(strconv.Itoa(os.Getpid())), 0o600)
	defer os.Remove(p.PID)
	exportAgentHomes(cfg)
	_ = os.WriteFile(p.BinRecord, []byte(self()), 0o600)

	level := logx.LevelFromEnv(logx.ParseLevel(cfg.LogLevel))
	if o.Debug || cfg.Debug() {
		level = logx.Debug
	}
	var sinks []logx.Sink
	if lf, err := logx.OpenFile(p.Log, 5<<20); err == nil {
		defer lf.Close()
		sinks = append(sinks, logx.Sink{W: lf, Level: level})
	}
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	if o.Watch || tty {
		sinks = append(sinks, logx.Sink{W: os.Stdout, Level: level, Human: true, Color: tty})
	}

	d, err := daemon.New(p)
	if err != nil {
		return err
	}
	d.Log = logx.New(sinks...)
	d.Compat = daemon.LoadCompat(p)
	d.PublishSnapshot = true
	if o.Once {
		d.Tick()
		return nil
	}
	if o.Watch {
		printWatchHeader(p, cfg, level)
	}
	warnCapabilities(d)
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	// Give Ctrl+C back afterwards (the interactive menu runs watch, then
	// carries on).
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			close(stop)
		}
	}()
	d.Run(stop)
	if o.Watch {
		fmt.Printf("\nStopped. Limits that reset while Continuum isn't running are caught up the next\ntime it runs (up to %d hours back).\n", int(cfg.CatchUp().Hours()))
	}
	return nil
}

// exportAgentHomes passes the agents' folders set in config.json to the
// agents' own commands the daemon runs (session lists, resume commands): a
// daemon started at login doesn't have the variables your shell sets, and
// those commands would otherwise look in the default folders. A default
// folder is left implicit (Claude Code reads some files elsewhere when
// CLAUDE_CONFIG_DIR is set, even to ~/.claude).
func exportAgentHomes(cfg core.Config) {
	home, _ := os.UserHomeDir()
	for _, a := range []struct{ env, dir, def string }{
		{"CLAUDE_CONFIG_DIR", cfg.ClaudeHome, ".claude"},
		{"CODEX_HOME", cfg.CodexHome, ".codex"},
		{"GROK_HOME", cfg.GrokHome, ".grok"},
	} {
		if a.dir != "" && filepath.Clean(a.dir) != filepath.Join(home, a.def) {
			_ = os.Setenv(a.env, a.dir)
		}
	}
}

// takeLock takes the daemon lock. `status`, hooks and `start` test whether a
// daemon runs by taking the lock for an instant; a daemon starting in that
// instant would wrongly see another one and exit, so it tries again for a
// moment first.
func takeLock(path string) (*os.File, error) {
	f, err := store.Lock(path)
	for i := 0; i < 10 && errors.Is(err, store.ErrLocked); i++ {
		time.Sleep(50 * time.Millisecond)
		f, err = store.Lock(path)
	}
	return f, err
}

func printWatchHeader(p core.Paths, cfg core.Config, level logx.Level) {
	sc, _ := scope.Load(p)
	var agents []string
	for _, a := range shellHookAgents() {
		if r := rulesOrNil(p); r != nil {
			agents = append(agents, r.Harness(a.Harness).DisplayName)
		}
	}
	scopeText := "every session"
	if sc.Mode == scope.ModeChosen {
		scopeText = fmt.Sprintf("chosen sessions only (%d)", len(sc.Chosen))
	}
	awake := "off"
	if h := cfg.KeepAwake(); h > 0 {
		awake = fmt.Sprintf("on (when a resume is within %dh)", int(h.Hours()))
	}
	fmt.Printf("Continuum %s · watching in this terminal. Press Ctrl+C to stop.\n", core.Version)
	if len(agents) > 0 {
		fmt.Printf("  agents:      %s\n", strings.Join(agents, ", "))
	}
	fmt.Printf("  scope:       %s\n", scopeText)
	fmt.Printf("  keep-awake:  %s\n", awake)
	fmt.Printf("  log:         %s (level: %s)\n", tildePath(p.Log), level)
	if cfg.DryRun {
		fmt.Println("  dry run:     on (nothing is typed or run)")
	}
	fmt.Println()
}

func cmdWatch(args []string) error {
	fs := newFlagSet("watch")
	debug := fs.Bool("debug", false, "Also show why: every event, check and delivery route.")
	takeover := fs.Bool("takeover", false, "If Continuum is already running in the background, stop it and watch here instead.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	if daemonRunning(p) && *takeover {
		stopDaemon(p)
		fmt.Println("Stopped the background daemon; watching here instead.")
		fmt.Println()
	}
	return runDaemon(daemonOptions{Watch: true, Debug: *debug})
}

func cmdLogs(args []string) error {
	fs := newFlagSet("logs")
	follow := fs.Bool("f", false, "Keep printing new lines as they are written (Ctrl+C to stop).")
	n := fs.Int("n", 40, "How many of the last `LINES` to show.")
	hooksLog := fs.Bool("hooks", false, "Show hooks.log (one line per agent hook call, written when debug logging is on) instead of daemon.log.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	path := p.Log
	if *hooksLog {
		path = p.Home + "/hooks.log"
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no log yet at %s", tildePath(path))
	}
	defer f.Close()
	lines := tailLines(f, *n)
	for _, l := range lines {
		fmt.Println(l)
	}
	if !*follow {
		return nil
	}
	off, _ := f.Seek(0, io.SeekEnd)
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if err == nil {
			fmt.Print(line)
			off += int64(len(line))
			continue
		}
		time.Sleep(300 * time.Millisecond)
		// The log rotated: start again at the top of the new file.
		if st, serr := os.Stat(path); serr == nil && st.Size() < off {
			f.Close()
			if f, err = os.Open(path); err != nil {
				return err
			}
			r, off = bufio.NewReader(f), 0
		}
	}
}

func tailLines(r io.Reader, n int) []string {
	sc := bufio.NewScanner(r)
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

// ensureDaemon starts the background daemon from hooks and `continuum run`,
// unless the user chose standalone mode (autostart_daemon: false).
func ensureDaemon() {
	defer func() { _ = recover() }()
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	if !cfg.AutostartEnabled() || stoppedByUser(p) || daemonRunning(p) {
		return
	}
	_, _ = proc.StartDetached([]string{self(), "daemon"}, p.Home, deliver.CleanEnv(os.Environ()), p.Home+"/daemon.stderr.log")
}

func rulesOrNil(p core.Paths) *rules.Rules {
	r, _ := rules.Cached(p.Rules)
	return r
}

// `continuum stop` leaves a note so that hooks don't start the daemon again
// straight away. `continuum start`, and the start at the next login, remove it.
func stoppedPath(p core.Paths) string { return filepath.Join(p.Home, "stopped") }

func stoppedByUser(p core.Paths) bool {
	_, err := os.Stat(stoppedPath(p))
	return err == nil
}

// launchedByService: started by launchd or systemd (at login), not by hand.
// A systemd user unit's parent is the user's service manager ($MANAGERPID),
// not pid 1. A terminal systemd started passes these variables on too, but
// there the parent is a shell.
func launchedByService() bool {
	if strings.Contains(os.Getenv("XPC_SERVICE_NAME"), "continuum") {
		return true
	}
	if os.Getenv("INVOCATION_ID") == "" {
		return false
	}
	ppid := os.Getppid()
	return ppid == 1 || os.Getenv("MANAGERPID") == strconv.Itoa(ppid)
}
