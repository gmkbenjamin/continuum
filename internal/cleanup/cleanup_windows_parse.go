package cleanup

import (
	"regexp"
	"strings"
	"time"
)

var (
	winCodexTest   = regexp.MustCompile(`(?i)/temp/testcodexexecrequiresstartedturnforthetarget[[:alnum:]_-]+/[0-9]{3}$`)
	winCodexArg    = regexp.MustCompile(`(?i)native-codex-exec-resume-[0-9]+\.log`)
	winDaemonTest  = regexp.MustCompile(`(?i)/temp/(?:continuum-hook-test|claude-[0-9]+/[^/]+/[^/]+/scratchpad/hook(?:time|test))$`)
	winLauncherErr = regexp.MustCompile(`(?i)^[a-z]:/(?:[^/]+/)*\.tmp[[:alnum:]]+/launcher\.err$`)
)

func isWindowsShell(name string) bool {
	switch strings.ToLower(strings.TrimSuffix(name, ".exe")) {
	case "sh", "bash", "cmd", "powershell", "pwsh":
		return true
	}
	return false
}

func isWindowsCleanupCandidateName(name string) bool {
	lower := strings.ToLower(name)
	return isWindowsShell(lower) || strings.HasPrefix(lower, "continuum")
}

func normWinPath(p string) string {
	return strings.TrimSuffix(strings.ReplaceAll(p, `\`, `/`), `/`)
}

func describeWindows(name, cwd string, now, startedAt time.Time, args []string) string {
	lowerName := strings.ToLower(name)
	normCwd := normWinPath(cwd)
	age := now.Sub(startedAt)
	switch {
	case isWindowsShell(lowerName) && strings.HasSuffix(strings.ToLower(normCwd), "/neo-rsh/crates/neo-launcher") && age >= time.Hour:
		for _, a := range args {
			if winLauncherErr.MatchString(normWinPath(strings.Trim(a, `"'`))) {
				return launcherReason
			}
		}
		return ""
	case isWindowsShell(lowerName) && winCodexTest.MatchString(normCwd) && age >= time.Hour:
		for _, a := range args {
			if winCodexArg.MatchString(a) {
				return codexReason
			}
		}
		return ""
	case strings.HasPrefix(lowerName, "continuum") && winDaemonTest.MatchString(normCwd) &&
		age >= 12*time.Hour && len(args) >= 2 && strings.EqualFold(args[1], "daemon"):
		return daemonReason
	default:
		return ""
	}
}
