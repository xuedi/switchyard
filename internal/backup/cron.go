package backup

import (
	"strconv"
	"strings"
	"time"
)

var cronMacros = map[string]string{"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *"}

var cronNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "oct": 10, "nov": 11, "dec": 12, "sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

type cronField struct {
	ok   [61]bool
	star bool
}

// parseField reads one field as the CronJob controller does: lists, ranges, steps, names;
// a bare * (or ?) is a star, which matters for the day fields.
func parseField(s string, lo, hi int) (cronField, bool) {
	var f cronField
	f.star = s == "*" || s == "?"
	num := func(x string) (int, bool) {
		if n, ok := cronNames[strings.ToLower(x)]; ok {
			return n, true
		}
		n, err := strconv.Atoi(x)
		return n, err == nil
	}
	for _, part := range strings.Split(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n < 1 {
				return f, false
			}
			step = n
		}
		from, to := lo, hi
		if rng != "*" && rng != "?" {
			a, b, isRange := strings.Cut(rng, "-")
			var ok bool
			if from, ok = num(a); !ok {
				return f, false
			}
			to = from
			if isRange {
				if to, ok = num(b); !ok {
					return f, false
				}
			} else if hasStep {
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return f, false
		}
		for i := from; i <= to; i += step {
			f.ok[i] = true
		}
	}
	return f, true
}

// lastSlot is the newest time at or before t (in t's zone) the schedule fires.
func lastSlot(schedule string, t time.Time) (time.Time, bool) {
	if m, ok := cronMacros[schedule]; ok {
		schedule = m
	}
	fs := strings.Fields(schedule)
	if len(fs) != 5 {
		return time.Time{}, false
	}
	var f [5]cronField
	for i, r := range [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}} {
		var ok bool
		if f[i], ok = parseField(fs[i], r[0], r[1]); !ok {
			return time.Time{}, false
		}
	}
	f[4].ok[0] = f[4].ok[0] || f[4].ok[7]
	day := func(t time.Time) bool {
		dom, dow := f[2].ok[t.Day()], f[4].ok[int(t.Weekday())]
		if f[2].star || f[4].star {
			return dom && dow
		}
		return dom || dow
	}
	loc := t.Location()
	t = t.Truncate(time.Minute)
	for end := t.AddDate(-5, 0, 0); t.After(end); {
		y, mo, d := t.Date()
		switch {
		case !f[3].ok[int(mo)]:
			t = time.Date(y, mo, 1, 0, 0, 0, 0, loc).Add(-time.Minute)
		case !day(t):
			t = time.Date(y, mo, d, 0, 0, 0, 0, loc).Add(-time.Minute)
		case !f[1].ok[t.Hour()]:
			t = time.Date(y, mo, d, t.Hour(), 0, 0, 0, loc).Add(-time.Minute)
		case !f[0].ok[t.Minute()]:
			t = t.Add(-time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}
