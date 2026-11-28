package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

// Shell integration: `claude` and `codex` typed in any terminal run under
// `continuum run --auto`, so Continuum can type into the original session
// (never a copy), whatever the terminal emulator, focused or not.

const (
	blockStart = "# >>> continuum >>>"
	blockEnd   = "# <<< continuum <<<"
)

// hookAgent is one command the shell hook wraps.
type hookAgent struct{ Harness, Command string }

// shellHookAgents lists the commands to wrap: config "shell_hook_agents" if
// set, otherwise every installed agent whose rules allow the shell hook.
func shellHookAgents() []hookAgent {
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	if r == nil || !cfg.IsEnabled() {
		return nil
	}
	var out []hookAgent
	seen := map[string]bool{}
	add := func(h *rules.Harness) {
		if h == nil || h.Binary == "" || seen[h.Binary] || !validName.MatchString(h.Binary) || !cfg.HarnessEnabled(h.Name) {
			return
		}
		seen[h.Binary] = true
		out = append(out, hookAgent{h.Name, h.Binary})
	}
	if len(cfg.ShellHookAgents) > 0 {
		for _, want := range cfg.ShellHookAgents {
			for _, name := range r.HarnessNames() {
				h := r.Harness(name)
				if name == want || h.Binary == want {
					add(h)
				}
				for _, a := range h.CommandAliases {
					if a.Command == want && !seen[a.Command] && validName.MatchString(a.Command) && cfg.HarnessEnabled(h.Name) {
						seen[a.Command] = true
						out = append(out, hookAgent{h.Name, a.Command})
					}
				}
			}
		}
		return out
	}
	for _, name := range r.HarnessNames() {
		h := r.Harness(name)
		if !h.InShellHook() {
			continue
		}
		if _, err := proc.LookPath(h.Binary); err == nil {
			add(h)
		}
		for _, a := range h.CommandAliases {
			if seen[a.Command] || !validName.MatchString(a.Command) || !cfg.HarnessEnabled(h.Name) {
				continue
			}
			if aliasResolves(a) {
				seen[a.Command] = true
				out = append(out, hookAgent{h.Name, a.Command})
			}
		}
	}
	return out
}

// aliasResolves reports whether a's command is installed and is the agent's
// own (Cursor's `agent` links into its cursor-agent folder; another tool
// named `agent` is left alone).
func aliasResolves(a rules.CommandAlias) bool {
	p, err := proc.LookPath(a.Command)
	if err != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return a.Matches(p)
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// psQuote wraps s in PowerShell single quotes, doubling ASCII ' and all four
// Unicode single-quote delimiters PowerShell's tokenizer also recognizes.
var psQuoteReplacer = strings.NewReplacer(
	"'", "''",
	"\u2018", "\u2018\u2018",
	"\u2019", "\u2019\u2019",
	"\u201a", "\u201a\u201a",
	"\u201b", "\u201b\u201b",
)

func psQuote(s string) string { return "'" + psQuoteReplacer.Replace(s) + "'" }

func isUTF16BOM(data []byte) bool {
	return len(data) >= 2 && ((data[0] == 0xFF && data[1] == 0xFE) || (data[0] == 0xFE && data[1] == 0xFF))
}

// shellBlock is what install writes into the shell's startup file. It asks
// Continuum for the current wrapper functions at each shell start, so agents
// installed later are picked up without reinstalling.
func shellBlock(shell, bin string) (string, error) {
	head := blockStart + "\n" +
		"# Runs AI coding agents (claude, codex, gemini, ...) under Continuum so usage\n" +
		"# limits resume in the same session, in any terminal. Remove this block or run\n" +
		"# `continuum uninstall` to stop. Skip once: CONTINUUM_NO_WRAP=1 <agent>.\n"
	switch shell {
	case "zsh", "bash", "sh":
		return head + "__continuum_bin=" + shQuote(bin) + "\n" +
			"[ -x \"$__continuum_bin\" ] && eval \"$(\"$__continuum_bin\" shell-init " + shell + " 2>/dev/null)\"\n" +
			blockEnd + "\n", nil
	case "fish":
		return head + "set -g __continuum_bin " + shQuote(bin) + "\n" +
			"test -x $__continuum_bin; and $__continuum_bin shell-init fish 2>/dev/null | source\n" +
			blockEnd + "\n", nil
	case "pwsh", "powershell":
		psHead := blockStart + "\n" +
			"# Runs AI coding agents (claude, codex, gemini, ...) under Continuum so usage\n" +
			"# limits resume in the same session, in any terminal. Remove this block or run\n" +
			"# `continuum uninstall` to stop. Skip once: $env:CONTINUUM_NO_WRAP=1; <agent>.\n"
		return psHead + "$__continuum_bin = " + psQuote(bin) + "\n" +
			"if (Test-Path -LiteralPath $__continuum_bin) {\n" +
			"    $__continuum_init = & $__continuum_bin shell-init " + shell + " 2>$null | Out-String\n" +
			"    if ($__continuum_init) { Invoke-Expression $__continuum_init }\n" +
			"}\n" +
			blockEnd + "\n", nil
	}
	return "", fmt.Errorf("unsupported shell %q (zsh, bash, fish, pwsh, powershell)", shell)
}

// shellFunctions is what `continuum shell-init` prints: one wrapper function
// per agent command.
func shellFunctions(shell, bin string, agents []hookAgent) (string, error) {
	var b strings.Builder
	switch shell {
	case "zsh", "bash", "sh":
		b.WriteString("# continuum shell-init " + shell + "\n")
		b.WriteString("__continuum_bin=" + shQuote(bin) + "\n")
		b.WriteString("__continuum_run() {\n")
		b.WriteString("  local __h=\"$1\"; shift\n")
		b.WriteString("  if [ -z \"${CONTINUUM_WRAPPER_ID:-}\" ] && [ -z \"${CONTINUUM_NO_WRAP:-}\" ] && [ -t 0 ] && [ -t 1 ] && [ -x \"$__continuum_bin\" ]; then\n")
		b.WriteString("    \"$__continuum_bin\" run --auto --harness \"$__h\" -- \"$@\"\n")
		b.WriteString("  else\n")
		b.WriteString("    command \"$@\"\n")
		b.WriteString("  fi\n")
		b.WriteString("}\n")
		for _, a := range agents {
			fmt.Fprintf(&b, "function %s { __continuum_run %s %s \"$@\"; }\n", a.Command, a.Harness, a.Command)
		}
	case "fish":
		b.WriteString("# continuum shell-init fish\n")
		b.WriteString("set -g __continuum_bin " + shQuote(bin) + "\n")
		for _, a := range agents {
			fmt.Fprintf(&b, "function %s --wraps %s\n", a.Command, a.Command)
			b.WriteString("    if test -z \"$CONTINUUM_WRAPPER_ID\" -a -z \"$CONTINUUM_NO_WRAP\" -a -x \"$__continuum_bin\"; and isatty stdin; and isatty stdout\n")
			fmt.Fprintf(&b, "        $__continuum_bin run --auto --harness %s -- %s $argv\n", a.Harness, a.Command)
			b.WriteString("    else\n")
			fmt.Fprintf(&b, "        command %s $argv\n", a.Command)
			b.WriteString("    end\nend\n")
		}
	case "pwsh", "powershell":
		b.WriteString("# continuum shell-init " + shell + "\n")
		b.WriteString("$global:__continuum_bin = " + psQuote(bin) + "\n")
		b.WriteString("function global:__continuum_run {\n")
		b.WriteString("    param([string]$Harness, [string]$Command, [object[]]$Rest)\n")
		b.WriteString("    if (-not $env:CONTINUUM_WRAPPER_ID -and -not $env:CONTINUUM_NO_WRAP -and -not [Console]::IsInputRedirected -and -not [Console]::IsOutputRedirected -and $global:__continuum_bin -and (Test-Path -LiteralPath $global:__continuum_bin)) {\n")
		b.WriteString("        & $global:__continuum_bin run --auto --harness $Harness -- $Command @Rest\n")
		b.WriteString("    } else {\n")
		b.WriteString("        $__real = Get-Command -Name $Command -CommandType Application,ExternalScript -ErrorAction SilentlyContinue | Select-Object -First 1\n")
		b.WriteString("        if ($__real) { & $__real.Source @Rest } else { Write-Error \"$Command : command not found\"; $global:LASTEXITCODE = 127 }\n")
		b.WriteString("    }\n")
		b.WriteString("}\n")
		for _, a := range agents {
			fmt.Fprintf(&b, "function global:%s { __continuum_run %s %s $args }\n", a.Command, psQuote(a.Harness), psQuote(a.Command))
		}
	default:
		return "", fmt.Errorf("unsupported shell %q (zsh, bash, fish, pwsh, powershell)", shell)
	}
	return b.String(), nil
}

type rcFile struct {
	Shell string
	Path  string
}

func windowsShellRCs(home string) []rcFile {
	dirs := windowsDocumentsDirs(home)
	primary := dirs[0]
	var out []rcFile
	seen := map[string]bool{}
	for _, docs := range dirs {
		for _, c := range []rcFile{
			{"pwsh", filepath.Join(docs, "PowerShell", "Microsoft.PowerShell_profile.ps1")},
			{"powershell", filepath.Join(docs, "WindowsPowerShell", "Microsoft.PowerShell_profile.ps1")},
		} {
			if seen[c.Path] {
				continue
			}
			if _, err := os.Stat(c.Path); err == nil {
				seen[c.Path] = true
				out = append(out, c)
			}
		}
	}
	if len(out) == 0 {
		if _, err := proc.LookPath("pwsh"); err == nil {
			out = append(out, rcFile{"pwsh", filepath.Join(primary, "PowerShell", "Microsoft.PowerShell_profile.ps1")})
		} else {
			out = append(out, rcFile{"powershell", filepath.Join(primary, "WindowsPowerShell", "Microsoft.PowerShell_profile.ps1")})
		}
	}
	return out
}

// shellRCs returns the startup files to hook: the user's login shell always,
// plus other shells whose startup file already exists.
func shellRCs() []rcFile {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		return windowsShellRCs(home)
	}
	login := filepath.Base(os.Getenv("SHELL"))
	cands := []rcFile{
		{"zsh", filepath.Join(zdotdir(home), ".zshrc")},
		{"bash", filepath.Join(home, ".bashrc")},
		{"fish", filepath.Join(home, ".config", "fish", "conf.d", "continuum.fish")},
	}
	var out []rcFile
	for _, c := range cands {
		_, err := os.Stat(c.Path)
		exists := err == nil
		if c.Shell == "fish" {
			_, err := os.Stat(filepath.Dir(c.Path))
			exists = err == nil
		}
		if exists || c.Shell == login {
			out = append(out, c)
		}
	}
	return out
}

func zdotdir(home string) string {
	if z := os.Getenv("ZDOTDIR"); z != "" {
		return z
	}
	return home
}

// replaceBlock returns content with the Continuum block replaced by block
// (removed when block is empty), preserving LF or CRLF line endings.
func replaceBlock(content, block string) string {
	eol := "\n"
	if strings.Contains(content, "\r\n") {
		eol = "\r\n"
		if block != "" {
			block = strings.ReplaceAll(strings.ReplaceAll(block, "\r\n", "\n"), "\n", "\r\n")
		}
	}
	start := strings.Index(content, blockStart)
	if start >= 0 {
		if end := strings.Index(content[start:], blockEnd); end >= 0 {
			end += start + len(blockEnd)
			if strings.HasPrefix(content[end:], "\r\n") {
				end += 2
			} else if end < len(content) && content[end] == '\n' {
				end++
			}
			rest := content[end:]
			head := content[:start]
			if block == "" {
				if strings.HasSuffix(head, "\r\n\r\n") {
					head = head[:len(head)-2]
				} else if strings.HasSuffix(head, "\n\n") {
					head = head[:len(head)-1] // the blank line install added
				}
				return head + rest
			}
			return head + block + rest
		}
	}
	if block == "" {
		return content
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += eol
	}
	if content != "" {
		content += eol
	}
	return content + block
}

// blockIntact: every Continuum start marker has its end marker after it.
func blockIntact(content string) bool {
	rest := content
	for {
		i := strings.Index(rest, blockStart)
		j := strings.Index(rest, blockEnd)
		switch {
		case i < 0 && j < 0:
			return true
		case i < 0 || j < 0 || j < i:
			return false
		}
		rest = rest[j+len(blockEnd):]
	}
}

// matchingRCBackup returns the newest backup of path that equals content
// apart from trailing newlines, or nil.
func matchingRCBackup(path, content string) []byte {
	list, _ := filepath.Glob(path + ".continuum-backup-*")
	sort.Sort(sort.Reverse(sort.StringSlice(list)))
	want := strings.TrimRight(content, "\n")
	for _, b := range list {
		if data, err := os.ReadFile(b); err == nil && strings.TrimRight(string(data), "\n") == want {
			return data
		}
	}
	return nil
}

// installShell adds or refreshes the shell hook. It returns a line per file.
func installShell(bin string, dry bool) []string {
	var out []string
	for _, rc := range shellRCs() {
		snip, err := shellBlock(rc.Shell, bin)
		if err != nil {
			continue
		}
		old, _ := os.ReadFile(rc.Path)
		if isUTF16BOM(old) {
			out = append(out, fmt.Sprintf("%-10s %s is UTF-16 encoded; left alone (save it as UTF-8 and run install again)", "shell:", tildePath(rc.Path)))
			continue
		}
		if !blockIntact(string(old)) {
			out = append(out, fmt.Sprintf("%-10s %s has a damaged Continuum block (a %q line without its %q line, or the reverse); left alone. Remove those lines and run install again", "shell:", tildePath(rc.Path), blockStart, blockEnd))
			continue
		}
		next := replaceBlock(string(old), snip)
		if next == string(old) {
			out = append(out, fmt.Sprintf("%-10s already set up in %s", "shell:", tildePath(rc.Path)))
			continue
		}
		if dry {
			out = append(out, fmt.Sprintf("%-10s would add the agent hook to %s", "shell:", tildePath(rc.Path)))
			continue
		}
		if len(old) > 0 {
			if err := hooks.SaveBackup(rc.Path, old); err != nil {
				out = append(out, fmt.Sprintf("%-10s %s: backup failed, left alone: %v", "shell:", rc.Path, err))
				continue
			}
		}
		if err := core.WriteFileAtomic(rc.Path, []byte(next), filePerm(rc.Path)); err != nil {
			out = append(out, fmt.Sprintf("%-10s %s: %v", "shell:", rc.Path, err))
			continue
		}
		out = append(out, fmt.Sprintf("%-10s agent hook added to %s (new terminals)", "shell:", tildePath(rc.Path)))
	}
	return out
}

// allRCs lists every startup file the shell hook could have been added to,
// whatever the current login shell.
func allRCs() []rcFile {
	home, _ := os.UserHomeDir()
	var out []rcFile
	seen := map[string]bool{}
	candidates := []rcFile{
		{"zsh", filepath.Join(zdotdir(home), ".zshrc")},
		{"zsh", filepath.Join(home, ".zshrc")},
		{"bash", filepath.Join(home, ".bashrc")},
		{"bash", filepath.Join(home, ".bash_profile")},
		{"fish", filepath.Join(home, ".config", "fish", "conf.d", "continuum.fish")},
	}
	for _, docs := range windowsDocumentsDirs(home) {
		candidates = append(candidates,
			rcFile{"pwsh", filepath.Join(docs, "PowerShell", "Microsoft.PowerShell_profile.ps1")},
			rcFile{"powershell", filepath.Join(docs, "WindowsPowerShell", "Microsoft.PowerShell_profile.ps1")},
		)
	}
	for _, rc := range candidates {
		if !seen[rc.Path] {
			seen[rc.Path] = true
			out = append(out, rc)
		}
	}
	return out
}

func uninstallShell(dry bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, rc := range allRCs() {
		if seen[rc.Path] {
			continue
		}
		seen[rc.Path] = true
		old, err := os.ReadFile(rc.Path)
		if err != nil || !bytes.Contains(old, []byte(blockStart)) {
			continue
		}
		if !blockIntact(string(old)) {
			out = append(out, fmt.Sprintf("%-10s %s has a damaged Continuum block (a %q line without its %q line, or the reverse); left alone. Remove those lines by hand", "shell:", tildePath(rc.Path), blockStart, blockEnd))
			continue
		}
		next := replaceBlock(string(old), "")
		// If nothing else changed since install, give back the exact bytes
		// (install may have added a final newline).
		if b := matchingRCBackup(rc.Path, next); b != nil {
			next = string(b)
		}
		if dry {
			out = append(out, fmt.Sprintf("%-10s would remove the hook from %s", "shell:", rc.Path))
			continue
		}
		if rc.Shell == "fish" && strings.TrimSpace(next) == "" {
			os.Remove(rc.Path)
		} else if err := core.WriteFileAtomic(rc.Path, []byte(next), filePerm(rc.Path)); err != nil {
			out = append(out, fmt.Sprintf("%-10s %s: %v", "shell:", rc.Path, err))
			continue
		}
		out = append(out, fmt.Sprintf("%-10s hook removed from %s", "shell:", tildePath(rc.Path)))
	}
	return out
}

func cmdShellInit(args []string) error {
	shell := filepath.Base(os.Getenv("SHELL"))
	if (shell == "" || shell == ".") && runtime.GOOS == "windows" {
		shell = "pwsh"
	}
	if len(args) > 0 {
		shell = args[0]
	}
	if shell == "" || shell == "." || shell == "-h" || shell == "--help" {
		return errors.New("usage: continuum shell-init zsh|bash|fish|pwsh|powershell")
	}
	out, err := shellFunctions(shell, self(), shellHookAgents())
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func filePerm(path string) os.FileMode {
	if st, err := os.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return 0o644
}
