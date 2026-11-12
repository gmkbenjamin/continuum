// Package resettime turns the human wording agents use for limit resets
// ("resets 10am (Australia/Sydney)", "try again at 5:53 PM",
// "try again in 2 hours 13 minutes", "resets Nov 14, 3pm") into a time.
package resettime

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	anchorRe = regexp.MustCompile(`(?i)(?:\bresets?|\breset\s+(?:at|on|in)|try\s+again|available\s+(?:again\s+)?(?:at|on|in)|will\s+reset|until|wait\s+until)\b`)
	relRe    = regexp.MustCompile(`(?i)\b(?:in|after)\s+((?:\d+(?:\.\d+)?\s*(?:days?|d|hours?|hrs?|hr|h|minutes?|mins?|min|m|seconds?|secs?|sec|s)\b[\s,]*(?:and\s+)?)+)`)
	relPart  = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(days?|d|hours?|hrs?|hr|h|minutes?|mins?|min|m|seconds?|secs?|sec|s)\b`)
	tzRe     = regexp.MustCompile(`\(\s*([A-Za-z]+(?:/[A-Za-z0-9_+\-]+)+|UTC|GMT|[A-Z]{3,4})\s*\)|\b(UTC|GMT)\b`)
	isoRe    = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.\d+)?)?\s*(Z|[+-]\d{2}:?\d{2})?)?`)
	months   = `(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)`
	mdRe     = regexp.MustCompile(`(?i)\b` + months + `\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b(?:,?\s+(\d{4})\b)?`)
	dmRe     = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+` + months + `\.?(?:,?\s+(\d{4})\b)?`)
	ampmRe   = regexp.MustCompile(`(?i)\b(\d{1,2})(?:[:.](\d{2}))?\s*([ap])\.?\s?m\b\.?`)
	hhmmRe   = regexp.MustCompile(`\b([01]?\d|2[0-3]):([0-5]\d)\b`)
	dowRe    = regexp.MustCompile(`(?i)\b(tomorrow|today|mon(?:day)?|tue(?:s(?:day)?)?|wed(?:nesday)?|thu(?:r(?:s(?:day)?)?)?|fri(?:day)?|sat(?:urday)?|sun(?:day)?)\b`)
	epochRe  = regexp.MustCompile(`(?i)resets?_?at["'\s:=]+(\d{10})(?:\d{3})?\b`)
)

// Parse finds the reset moment described in text. ref is when the message was
// shown (use the transcript timestamp for old messages, not the current
// time); local is the zone used when the text names none.
func Parse(text string, ref time.Time, local *time.Location) (time.Time, bool) {
	if local == nil {
		local = time.Local
	}
	text = strings.ReplaceAll(text, " ", " ")
	text = strings.ReplaceAll(text, " ", " ")
	if m := epochRe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return time.Unix(n, 0), true
		}
	}

	locs := anchorRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return time.Time{}, false
	}
	// Try the last anchor first: it is the newest message on screen.
	for i := len(locs) - 1; i >= 0; i-- {
		start := locs[i][0]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if end-start > 160 {
			end = start + 160
		}
		seg := text[start:end]
		if nl := strings.IndexAny(seg, "\n"); nl > 0 && nl < len(seg)-1 {
			// Allow the time to sit on the next line, but no further.
			if nl2 := strings.IndexAny(seg[nl+1:], "\n"); nl2 >= 0 {
				seg = seg[:nl+1+nl2]
			}
		}
		if t, ok := parseSegment(seg, ref, local); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseSegment(seg string, ref time.Time, local *time.Location) (time.Time, bool) {
	if m := relRe.FindStringSubmatch(seg); m != nil {
		var total time.Duration
		for _, p := range relPart.FindAllStringSubmatch(m[1], -1) {
			v, _ := strconv.ParseFloat(p[1], 64)
			unit := strings.ToLower(p[2])
			switch {
			case strings.HasPrefix(unit, "d"):
				total += time.Duration(v * float64(24*time.Hour))
			case strings.HasPrefix(unit, "h"):
				total += time.Duration(v * float64(time.Hour))
			case unit == "m" || strings.HasPrefix(unit, "min"):
				total += time.Duration(v * float64(time.Minute))
			default:
				total += time.Duration(v * float64(time.Second))
			}
		}
		if total > 0 {
			return ref.Add(total), true
		}
	}

	loc := local
	if m := tzRe.FindStringSubmatch(seg); m != nil {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if name == "GMT" {
			name = "UTC"
		}
		if l, err := time.LoadLocation(name); err == nil {
			loc = l
		}
	}

	if m := isoRe.FindStringSubmatch(seg); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		hh, _ := strconv.Atoi(m[4])
		mm, _ := strconv.Atoi(m[5])
		ss, _ := strconv.Atoi(m[6])
		l := loc
		if z := m[7]; z != "" {
			if z == "Z" {
				l = time.UTC
			} else if off, ok := parseOffset(z); ok {
				l = time.FixedZone(z, off)
			}
		}
		return time.Date(y, time.Month(mo), d, hh, mm, ss, 0, l), true
	}

	refIn := ref.In(loc)
	year, month, day := refIn.Date()
	haveDate := false
	explicitYear := false
	if m := mdRe.FindStringSubmatch(seg); m != nil {
		month = monthOf(m[1])
		day, _ = strconv.Atoi(m[2])
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
			explicitYear = true
		}
		haveDate = true
	} else if m := dmRe.FindStringSubmatch(seg); m != nil {
		day, _ = strconv.Atoi(m[1])
		month = monthOf(m[2])
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
			explicitYear = true
		}
		haveDate = true
	}
	dayShift := -1
	if !haveDate {
		if m := dowRe.FindStringSubmatch(seg); m != nil {
			dayShift = weekdayShift(strings.ToLower(m[1]), refIn.Weekday())
		}
	}

	hour, minute := 0, 0
	haveTime := false
	if m := ampmRe.FindStringSubmatch(seg); m != nil {
		hour, _ = strconv.Atoi(m[1])
		if m[2] != "" {
			minute, _ = strconv.Atoi(m[2])
		}
		pm := strings.EqualFold(m[3], "p")
		if hour == 12 {
			hour = 0
		}
		if pm {
			hour += 12
		}
		haveTime = hour < 24 && minute < 60
	} else if m := hhmmRe.FindStringSubmatch(seg); m != nil {
		hour, _ = strconv.Atoi(m[1])
		minute, _ = strconv.Atoi(m[2])
		haveTime = true
	}
	if !haveTime && !haveDate && dayShift < 0 {
		return time.Time{}, false
	}

	t := time.Date(year, month, day, hour, minute, 0, 0, loc)
	switch {
	case haveDate:
		if !explicitYear && t.Before(ref.Add(-36*time.Hour)) {
			t = t.AddDate(1, 0, 0)
		}
	case dayShift >= 0:
		t = t.AddDate(0, 0, dayShift)
		// "resets Mon 6pm" seen on Monday after 6pm means next Monday.
		if t.Before(ref.Add(-5 * time.Minute)) {
			t = t.AddDate(0, 0, 7)
		}
	default:
		// Time of day only: the next occurrence after the message appeared.
		if t.Before(ref.Add(-5 * time.Minute)) {
			t = t.AddDate(0, 0, 1)
		}
	}
	return t, true
}

func parseOffset(z string) (int, bool) {
	z = strings.ReplaceAll(z, ":", "")
	if len(z) != 5 {
		return 0, false
	}
	h, err1 := strconv.Atoi(z[1:3])
	m, err2 := strconv.Atoi(z[3:5])
	if err1 != nil || err2 != nil {
		return 0, false
	}
	off := h*3600 + m*60
	if z[0] == '-' {
		off = -off
	}
	return off, true
}

func monthOf(s string) time.Month {
	s = strings.ToLower(s)
	for i, p := range []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"} {
		if strings.HasPrefix(s, p) {
			return time.Month(i + 1)
		}
	}
	return time.January
}

func weekdayShift(word string, from time.Weekday) int {
	switch word {
	case "today":
		return 0
	case "tomorrow":
		return 1
	}
	for i, p := range []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"} {
		if strings.HasPrefix(word, p) {
			shift := (i - int(from) + 7) % 7
			return shift
		}
	}
	return -1
}
