package deliver

import (
	"strings"
	"testing"
)

func TestBuildWinCmdLineBatchSafety(t *testing.T) {
	// Normal executable passes through Win32 quoting without cmd.exe wrapper.
	got, err := buildWinCmdLine(`C:\Program Files\claude\claude.exe`, []string{"claude", "--resume", "abc 123"}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := `"C:\Program Files\claude\claude.exe" --resume "abc 123"`
	if got != want {
		t.Fatalf("exe cmdline = %q, want %q", got, want)
	}

	// .cmd shim escapes every cmd.exe metacharacter (BatBadBut mitigation).
	cmdLine, err := buildWinCmdLine(
		`C:\Users\dev\AppData\Roaming\npm\codex.cmd`,
		[]string{"codex", `resume`, `"&calc.exe&%TEMP%|echo hi`},
		`C:\Windows\System32\cmd.exe`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cmdLine, `C:\Windows\System32\cmd.exe /d /s /c "`) {
		t.Fatalf("missing cmd.exe /d /s /c wrapper: %q", cmdLine)
	}
	// Every &, |, %, and inner " must be caret-escaped.
	argPart := cmdLine[strings.Index(cmdLine, "resume ")+len("resume "):]
	argPart = strings.TrimSuffix(argPart, `"`)
	for _, unescaped := range []string{"&", "|", "%"} {
		idx := 0
		for {
			pos := strings.Index(argPart[idx:], unescaped)
			if pos < 0 {
				break
			}
			abs := idx + pos
			if abs == 0 || argPart[abs-1] != '^' {
				t.Fatalf("unescaped %q in batch arg %q", unescaped, argPart)
			}
			idx = abs + 1
		}
	}

	// Newlines in batch args are rejected outright.
	if _, err := buildWinCmdLine(`C:\npm\codex.cmd`, []string{"codex", "a\ncalc.exe"}, ""); err == nil {
		t.Fatal("expected error for newline in batch argument")
	}
	if _, err := buildWinCmdLine(`C:\npm\codex&evil.cmd`, []string{"codex"}, ""); err == nil {
		t.Fatal("expected error for unsafe characters in batch path")
	}
}
