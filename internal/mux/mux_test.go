package mux

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/rules"
)

func TestSendCommands(t *testing.T) {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	var stdins []string
	orig := Runner
	defer func() { Runner = orig }()
	Runner = func(_ time.Duration, stdin string, argv ...string) (string, error) {
		calls = append(calls, argv)
		stdins = append(stdins, stdin)
		return "", nil
	}

	text := `continue ^C \n "quoted"`
	cases := []struct {
		loc       core.Location
		wantText  []string
		wantEnter []string
	}{
		{core.Location{Kind: "tmux", Target: "%3", Socket: "/tmp/tmux-501/default"},
			[]string{"tmux", "-S", "/tmp/tmux-501/default", "send-keys", "-t", "%3", "-l", "--", text},
			[]string{"tmux", "-S", "/tmp/tmux-501/default", "send-keys", "-t", "%3", "Enter"}},
		{core.Location{Kind: "tmux", Target: "%3"}, // no socket: the optional group is dropped
			[]string{"tmux", "send-keys", "-t", "%3", "-l", "--", text},
			[]string{"tmux", "send-keys", "-t", "%3", "Enter"}},
		{core.Location{Kind: "screen", Target: "0", Socket: "1234.dev"},
			[]string{"screen", "-S", "1234.dev", "-p", "0", "-X", "stuff", `continue \^C \\n "quoted"`},
			[]string{"screen", "-S", "1234.dev", "-p", "0", "-X", "stuff", "\r"}},
		{core.Location{Kind: "zellij", Target: "terminal_2", Socket: "dev"},
			[]string{"zellij", "--session", "dev", "action", "write-chars", "--pane-id", "terminal_2", text},
			[]string{"zellij", "--session", "dev", "action", "write", "--pane-id", "terminal_2", "13"}},
		{core.Location{Kind: "herdr", Target: "wP:p1"},
			[]string{"herdr", "pane", "send-text", "wP:p1", text},
			[]string{"herdr", "pane", "send-keys", "wP:p1", "enter"}},
		{core.Location{Kind: "herdr", Target: "w1:p1", Socket: "continuum-it"},
			[]string{"herdr", "--session", "continuum-it", "pane", "send-text", "w1:p1", text},
			[]string{"herdr", "--session", "continuum-it", "pane", "send-keys", "w1:p1", "enter"}},
		{core.Location{Kind: "rex", Target: "block:1", Socket: "unix:///x.sock"},
			[]string{"rex", "-S", "unix:///x.sock", "--autostart=false", "send", "-b", "block:1", text},
			[]string{"rex", "-S", "unix:///x.sock", "--autostart=false", "send-key", "-b", "block:1", "enter"}},
		{core.Location{Kind: "wezterm", Target: "4"},
			[]string{"wezterm", "cli", "send-text", "--pane-id", "4", "--no-paste", text},
			[]string{"wezterm", "cli", "send-text", "--pane-id", "4", "--no-paste", "\r"}},
		{core.Location{Kind: "kitty", Target: "5", Socket: "unix:/tmp/kitty"},
			[]string{"kitty", "@", "--to", "unix:/tmp/kitty", "send-text", "--match", "id:5", "--stdin"},
			[]string{"kitty", "@", "--to", "unix:/tmp/kitty", "send-text", "--match", "id:5", "--stdin"}},
	}
	for _, c := range cases {
		calls, stdins = nil, nil
		loc := c.loc
		if err := Send(r, &loc, text, 0); err != nil {
			t.Fatalf("%s: %v", c.loc.Kind, err)
		}
		if len(calls) != 2 || !reflect.DeepEqual(calls[0], c.wantText) || !reflect.DeepEqual(calls[1], c.wantEnter) {
			t.Errorf("%s:\n got %q\nwant %q\n     %q", c.loc.Kind, calls, c.wantText, c.wantEnter)
		}
		if c.loc.Kind == "kitty" && (stdins[0] != text || stdins[1] != "\r") {
			t.Errorf("kitty stdin %q", stdins)
		}
	}
}

func TestTmuxFormatIsNotAPlaceholder(t *testing.T) {
	got := Expand([]rules.Arg{{Text: "#{pane_current_command}"}, {Group: []string{"-S", "{socket}"}}}, map[string]string{"socket": ""})
	if !reflect.DeepEqual(got, []string{"#{pane_current_command}"}) {
		t.Fatalf("got %q", got)
	}
}

func TestUnknownMultiplexer(t *testing.T) {
	r, _ := rules.Load("")
	err := Send(r, &core.Location{Kind: "nope", Target: "1"}, "x", 0)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("got %v", err)
	}
}

// Pane ids repeat across servers and sessions (tmux %3 on every server,
// screen window 0 in every session): a process is in the pane only if it is
// on the same one.
func TestInPaneNeedsTheSameServer(t *testing.T) {
	r, _ := rules.Load("")
	cases := []struct {
		env  map[string]string
		loc  core.Location
		want bool
	}{
		{map[string]string{"TMUX_PANE": "%3", "TMUX": "/tmp/tmux-501/default,1,0"}, core.Location{Kind: "tmux", Target: "%3", Socket: "/tmp/tmux-501/default"}, true},
		{map[string]string{"TMUX_PANE": "%3", "TMUX": "/tmp/tmux-501/default,1,0"}, core.Location{Kind: "tmux", Target: "%3", Socket: "/tmp/tmux-501/work"}, false},
		{map[string]string{"TMUX_PANE": "%3", "TMUX": "/tmp/tmux-501/default,1,0"}, core.Location{Kind: "tmux", Target: "%3"}, true}, // server not recorded
		{map[string]string{"WINDOW": "0", "STY": "222.play"}, core.Location{Kind: "screen", Target: "0", Socket: "111.work"}, false},
		{map[string]string{"WINDOW": "0", "STY": "111.work"}, core.Location{Kind: "screen", Target: "0", Socket: "111.work"}, true},
		{map[string]string{"ZELLIJ_PANE_ID": "0", "ZELLIJ_SESSION_NAME": "play"}, core.Location{Kind: "zellij", Target: "terminal_0", Socket: "work"}, false},
	}
	for _, c := range cases {
		if got := InPane(r, c.env, &c.loc); got != c.want {
			t.Errorf("env %v in %s: %v, want %v", c.env, c.loc.String(), got, c.want)
		}
	}
}

// The pane list's third field is the server's socket path, as the panes'
// own TMUX says it, so a scanned pane matches the one a hook reported.
func TestListReadsTheServerSocket(t *testing.T) {
	r, _ := rules.Load("")
	orig := Runner
	defer func() { Runner = orig }()
	Runner = func(_ time.Duration, _ string, argv ...string) (string, error) {
		return "%1\tzsh\t/private/tmp/tmux-501/default\n%2\tnode\n", nil
	}
	panes, err := List(r, "tmux", "/tmp/tmux-501/default")
	if err != nil || len(panes) != 2 {
		t.Fatalf("%v %+v", err, panes)
	}
	if got := panes[0].Location.String(); got != "tmux:%1@/private/tmp/tmux-501/default" || panes[0].Command != "zsh" {
		t.Errorf("pane 1: %s %q", got, panes[0].Command)
	}
	if got := panes[1].Location.String(); got != "tmux:%2@/tmp/tmux-501/default" || panes[1].Command != "node" {
		t.Errorf("pane 2 (no socket printed): %s %q", got, panes[1].Command)
	}
}
