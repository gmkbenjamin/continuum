//go:build windows

package wrapper

import (
	"reflect"
	"testing"
	"unicode/utf16"
)

func decodeEnvBlock(b []uint16) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c != 0 {
			continue
		}
		if i == start {
			break
		}
		out = append(out, string(utf16.Decode(b[start:i])))
		start = i + 1
	}
	return out
}

func TestMakeEnvBlockDeduplicatesAndSortsCaseInsensitively(t *testing.T) {
	block := makeEnvBlock([]string{
		"Path=C:\\first",
		"ZED=1",
		"=C:=C:\\work",
		"alpha=first",
		"ALPHA=second",
		"path=C:\\last",
		"badentry",
		"WITHNUL=a\x00b",
	})
	got := decodeEnvBlock(block)
	want := []string{
		"=C:=C:\\work",
		"ALPHA=second",
		"path=C:\\last",
		"ZED=1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if empty := makeEnvBlock(nil); !reflect.DeepEqual(empty, []uint16{0, 0}) {
		t.Fatalf("empty env block = %v, want double NUL [0 0]", empty)
	}
}

func TestBuildCommandLine(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		argv    []string
		comspec string
		want    string
	}{
		{
			name: "exe with spaces",
			path: `C:\Program Files\Claude\claude.exe`,
			argv: []string{"claude", "--prompt", "hello world"},
			want: `"C:\Program Files\Claude\claude.exe" --prompt "hello world"`,
		},
		{
			name:    "cmd shim with multiple quoted parts",
			path:    `C:\Program Files\nodejs\claude.cmd`,
			argv:    []string{"claude", "--prompt", "hello world"},
			comspec: `C:\Windows\system32\cmd.exe`,
			want:    `C:\Windows\system32\cmd.exe /d /s /c ""C:\Program Files\nodejs\claude.cmd" --prompt "hello world""`,
		},
		{
			name:    "uppercase bat with spaced comspec and embedded quote",
			path:    `C:\Tools\My Agent\run.BAT`,
			argv:    []string{"run.BAT", `say "hi"`},
			comspec: `C:\Custom Shell\cmd.exe`,
			want:    `"C:\Custom Shell\cmd.exe" /d /s /c ""C:\Tools\My Agent\run.BAT" "say \"hi\"""`,
		},
		{
			name: "cmd fallback to default cmd.exe",
			path: `C:\bin\agent.cmd`,
			argv: []string{"agent"},
			want: `cmd.exe /d /s /c "C:\bin\agent.cmd"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildCommandLine(tc.path, tc.argv, tc.comspec); got != tc.want {
				t.Fatalf("buildCommandLine(%q, %q, %q) =\n  %s\nwant:\n  %s", tc.path, tc.argv, tc.comspec, got, tc.want)
			}
		})
	}
}
