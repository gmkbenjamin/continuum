package deliver

import (
	"errors"
	"path/filepath"
	"strings"
)

// escapeWinArg quotes one argument using standard Win32 CommandLineToArgvW rules.
func escapeWinArg(s string) string {
	if len(s) == 0 {
		return `""`
	}
	needsQuotes := false
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '"' {
			needsQuotes = true
			break
		}
	}
	if !needsQuotes {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	bs := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			bs++
		case '"':
			for k := 0; k < bs*2+1; k++ {
				b.WriteByte('\\')
			}
			b.WriteByte('"')
			bs = 0
		default:
			for k := 0; k < bs; k++ {
				b.WriteByte('\\')
			}
			b.WriteByte(s[i])
			bs = 0
		}
	}
	for k := 0; k < bs*2; k++ {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

// escapeCmdExeMeta escapes cmd.exe metacharacters outside quote mode so
// arguments passed through `cmd.exe /d /s /c` cannot trigger shell injection
// (CVE-2024-24576 / BatBadBut) or environment variable expansion.
func escapeCmdExeMeta(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', ')', '%', '!', '^', '"', '<', '>', '&', '|':
			b.WriteByte('^')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isBatchFile(path string) bool {
	ext := filepath.Ext(path)
	return strings.EqualFold(ext, ".cmd") || strings.EqualFold(ext, ".bat")
}

// buildWinCmdLine safely constructs a Windows CreateProcess command line,
// defending against cmd.exe injection when executing .cmd/.bat shims.
func buildWinCmdLine(path string, argv []string, comspec string) (string, error) {
	if strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("executable path contains control characters")
	}
	for _, a := range argv[1:] {
		if strings.IndexByte(a, 0) >= 0 {
			return "", errors.New("argument contains NUL byte")
		}
	}
	if !isBatchFile(path) {
		parts := make([]string, len(argv))
		parts[0] = escapeWinArg(path)
		for i := 1; i < len(argv); i++ {
			parts[i] = escapeWinArg(argv[i])
		}
		return strings.Join(parts, " "), nil
	}
	if strings.ContainsAny(path, `"%&|<>^!()`) {
		return "", errors.New("batch script path contains unsafe cmd.exe characters")
	}
	for _, a := range argv[1:] {
		if strings.ContainsAny(a, "\r\n") {
			return "", errors.New("batch script argument contains newline")
		}
	}
	if comspec == "" {
		comspec = "cmd.exe"
	}
	parts := make([]string, len(argv))
	parts[0] = escapeWinArg(path)
	for i := 1; i < len(argv); i++ {
		parts[i] = escapeCmdExeMeta(escapeWinArg(argv[i]))
	}
	return escapeWinArg(comspec) + ` /d /s /c "` + strings.Join(parts, " ") + `"`, nil
}
