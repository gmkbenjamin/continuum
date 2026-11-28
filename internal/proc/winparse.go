package proc

import (
	"strings"
	"unicode/utf16"
)

// splitWinCommandLine parses a Windows CreateProcess command line according to
// standard CommandLineToArgvW quoting and backslash-escaping rules.
func splitWinCommandLine(cmd string) []string {
	var args []string
	i, n := 0, len(cmd)
	for i < n {
		for i < n && (cmd[i] == ' ' || cmd[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		var b strings.Builder
		inQuotes := false
		for i < n {
			c := cmd[i]
			if !inQuotes && (c == ' ' || c == '\t') {
				break
			}
			if c == '\\' {
				bs := 0
				for i < n && cmd[i] == '\\' {
					bs++
					i++
				}
				if i < n && cmd[i] == '"' {
					for k := 0; k < bs/2; k++ {
						b.WriteByte('\\')
					}
					if bs%2 == 1 {
						b.WriteByte('"')
						i++
					} else {
						if inQuotes && i+1 < n && cmd[i+1] == '"' {
							b.WriteByte('"')
							i += 2
						} else {
							inQuotes = !inQuotes
							i++
						}
					}
				} else {
					for k := 0; k < bs; k++ {
						b.WriteByte('\\')
					}
				}
				continue
			}
			if c == '"' {
				if inQuotes && i+1 < n && cmd[i+1] == '"' {
					b.WriteByte('"')
					i += 2
				} else {
					inQuotes = !inQuotes
					i++
				}
				continue
			}
			b.WriteByte(c)
			i++
		}
		args = append(args, b.String())
	}
	return args
}

// parseWinEnvBlock decodes a Windows UTF-16 double-NUL-terminated environment
// block into key=value pairs, skipping Windows per-drive "=C:=..." entries.
func parseWinEnvBlock(units []uint16) map[string]string {
	if len(units) == 0 {
		return nil
	}
	env := map[string]string{}
	start := 0
	for i := 0; i < len(units); i++ {
		if units[i] != 0 {
			continue
		}
		if i == start {
			break // double-NUL terminator
		}
		entry := string(utf16.Decode(units[start:i]))
		start = i + 1
		if entry == "" || entry[0] == '=' {
			continue
		}
		if eq := strings.IndexByte(entry, '='); eq > 0 {
			env[entry[:eq]] = entry[eq+1:]
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}
