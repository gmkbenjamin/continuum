package resettime

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestParse(t *testing.T) {
	syd := mustLoc(t, "Australia/Sydney")
	ny := mustLoc(t, "America/New_York")
	// Wed 11 Nov 2026, 08:15 in Sydney.
	ref := time.Date(2026, 11, 11, 8, 15, 0, 0, syd)

	cases := []struct {
		name string
		text string
		want time.Time
	}{
		{"claude session, later today", "You've hit your session limit · resets 10am (Australia/Sydney)", time.Date(2026, 11, 11, 10, 0, 0, 0, syd)},
		{"claude session, tomorrow", "You've hit your session limit · resets 1am (Australia/Sydney)", time.Date(2026, 11, 12, 1, 0, 0, 0, syd)},
		{"claude with minutes", "You've hit your limit · resets 3:30pm (Australia/Sydney)", time.Date(2026, 11, 11, 15, 30, 0, 0, syd)},
		{"claude weekly with date", "You've hit your weekly limit · resets Nov 14, 6pm (Australia/Sydney)", time.Date(2026, 11, 14, 18, 0, 0, 0, syd)},
		{"claude other zone", "Claude usage limit reached. Your limit will reset at 5pm (America/New_York).", time.Date(2026, 11, 10, 17, 0, 0, 0, ny)},
		{"codex local time", "You’ve hit your usage limit. Visit https://chatgpt.com/settings/usage to purchase more credits or try again at 5:53 PM.", time.Date(2026, 11, 11, 17, 53, 0, 0, syd)},
		{"codex relative", "You've hit your usage limit. Upgrade to Pro or try again in 2 hours 13 minutes.", ref.Add(2*time.Hour + 13*time.Minute)},
		{"codex relative days", "You've hit your usage limit. Try again in 3 days 4 hours.", ref.Add(76 * time.Hour)},
		{"antigravity compact hms", "⚠ Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 3h32m34s.\nError ID: c14a5727-903a-4050-b53c-86224c0d83e5-364", ref.Add(3*time.Hour + 32*time.Minute + 34*time.Second)},
		{"compact hm", "Quota reached. Resets in 1h5m.", ref.Add(time.Hour + 5*time.Minute)},
		{"codex date", "You've hit your usage limit. Try again at 12 Nov 2026 9:05 AM.", time.Date(2026, 11, 12, 9, 5, 0, 0, syd)},
		{"short relative", "Rate limited. Try again in 45s", ref.Add(45 * time.Second)},
		{"24h clock", "Quota exceeded; resets at 14:00", time.Date(2026, 11, 11, 14, 0, 0, 0, syd)},
		{"iso utc", "limit reached, resets at 2026-11-11T03:00:00Z", time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)},
		{"epoch field", `{"error":"rate_limit","resets_at": 1793742179}`, time.Unix(1793742179, 0)},
		{"weekday", "Weekly limit reached, resets Monday 9am", time.Date(2026, 11, 16, 9, 0, 0, 0, syd)},
		{"tomorrow", "Your quota resets tomorrow at 7am", time.Date(2026, 11, 12, 7, 0, 0, 0, syd)},
		{"utc word", "usage limit reached · resets 6pm (UTC)", time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC).Add(24 * time.Hour)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Parse(c.text, ref, syd)
			if !ok {
				t.Fatalf("no time found in %q", c.text)
			}
			if !got.Equal(c.want) {
				t.Fatalf("Parse(%q)\n got %v\nwant %v", c.text, got, c.want)
			}
		})
	}
}

func TestParseNewYork(t *testing.T) {
	// "resets 5pm (America/New_York)" seen at 08:15 Sydney (16:15 NY the day
	// before) is 45 minutes later.
	syd := mustLoc(t, "Australia/Sydney")
	ny := mustLoc(t, "America/New_York")
	ref := time.Date(2026, 11, 11, 8, 15, 0, 0, syd)
	got, ok := Parse("resets 5pm (America/New_York)", ref, syd)
	if !ok || !got.Equal(time.Date(2026, 11, 10, 17, 0, 0, 0, ny)) {
		t.Fatalf("got %v %v", got, ok)
	}
	if d := got.Sub(ref); d != 45*time.Minute {
		t.Fatalf("expected 45m away, got %v", d)
	}
}

func TestNoTime(t *testing.T) {
	ref := time.Now()
	for _, s := range []string{
		"You've hit your session limit",
		"Your claude.ai usage limit has reset. Continue the task.",
		"API Error: 529 Overloaded",
		"",
	} {
		if got, ok := Parse(s, ref, time.UTC); ok {
			t.Errorf("Parse(%q) = %v, want none", s, got)
		}
	}
}

func TestOldMessageUsesItsOwnTimestamp(t *testing.T) {
	syd := mustLoc(t, "Australia/Sydney")
	// The message was printed at 09:58; read back at 10:30 it still means 10:00 that day.
	printed := time.Date(2026, 11, 11, 9, 58, 0, 0, syd)
	got, _ := Parse("resets 10am (Australia/Sydney)", printed, syd)
	if !got.Equal(time.Date(2026, 11, 11, 10, 0, 0, 0, syd)) {
		t.Fatalf("got %v", got)
	}
}

func TestWeekdayResetEarlierTodayMeansNextWeek(t *testing.T) {
	syd, _ := time.LoadLocation("Australia/Sydney")
	mon7pm := time.Date(2026, 11, 16, 19, 0, 0, 0, syd)
	got, ok := Parse("You've hit your weekly limit · resets Mon 6pm", mon7pm, syd)
	want := time.Date(2026, 11, 23, 18, 0, 0, 0, syd)
	if !ok || !got.Equal(want) {
		t.Fatalf("got %v ok=%v, want %v", got, ok, want)
	}
	// Later the same day is still today.
	got, _ = Parse("resets Mon 9pm", mon7pm, syd)
	if want := time.Date(2026, 11, 16, 21, 0, 0, 0, syd); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseEdgeCases(t *testing.T) {
	syd := mustLoc(t, "Australia/Sydney")
	ref := time.Date(2026, 11, 11, 14, 0, 0, 0, syd)

	// Last resets_at epoch wins (newest screen line).
	got, ok := Parse(`"resets_at": 1793700000 ... "resets_at": 1793742179`, ref, syd)
	if !ok || !got.Equal(time.Unix(1793742179, 0)) {
		t.Fatalf("epoch last-match = (%v, %v)", got, ok)
	}

	for _, bad := range []string{
		"Quota resets today",
		"Try again at 15am",
		"Resets 0am",
		"Resets 13pm",
	} {
		if got, ok := Parse(bad, ref, syd); ok {
			t.Errorf("Parse(%q) = (%v, true), want ok=false", bad, got)
		}
	}
}
