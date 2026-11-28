package proc

import (
	"reflect"
	"testing"
	"unicode/utf16"
)

func TestSplitWinCommandLine(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{
			`"C:\Program Files\nodejs\node.exe" "C:\Users\dev\codex.js" resume 01a21f63`,
			[]string{`C:\Program Files\nodejs\node.exe`, `C:\Users\dev\codex.js`, "resume", "01a21f63"},
		},
		{
			`claude.exe --resume "abc-123" --prompt "say \"hi\""`,
			[]string{"claude.exe", "--resume", "abc-123", "--prompt", `say "hi"`},
		},
		{
			`codex.exe "C:\dir with space\\" arg2`,
			[]string{"codex.exe", `C:\dir with space\`, "arg2"},
		},
		{
			`   `,
			nil,
		},
	}
	for _, tc := range cases {
		if got := splitWinCommandLine(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitWinCommandLine(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestParseWinEnvBlock(t *testing.T) {
	var units []uint16
	for _, s := range []string{`=C:=C:\Users\dev`, `CONTINUUM_WRAPPER_ID=w-win1`, `TERM_PROGRAM=WezTerm`, `EMPTY=`} {
		units = append(units, utf16.Encode([]rune(s))...)
		units = append(units, 0)
	}
	units = append(units, 0)
	got := parseWinEnvBlock(units)
	if got["CONTINUUM_WRAPPER_ID"] != "w-win1" || got["TERM_PROGRAM"] != "WezTerm" {
		t.Fatalf("unexpected env: %#v", got)
	}
	if _, hasDrive := got["=C"]; hasDrive {
		t.Fatalf("per-drive entry leaked: %#v", got)
	}
	if v, ok := got["EMPTY"]; !ok || v != "" {
		t.Fatalf("empty value not preserved: %#v", got)
	}
}
