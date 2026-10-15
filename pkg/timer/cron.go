package timer

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronSpec is a parsed simplified 5-field cron: 分 时 日 月 周.
// Supported per field: `*`, `a`, `a-b`, `*/n`, `a-b/n`, comma lists
// thereof; month accepts JAN..DEC, dow accepts SUN..SAT (0/7 = Sunday).
// Day semantics follow POSIX: when both day-of-month and day-of-week
// are restricted the fire matches on EITHER; when only one is
// restricted that one decides.
type cronSpec struct {
	minute        map[int]bool // 0-59
	hour          map[int]bool // 0-23
	dom           map[int]bool // 1-31
	month         map[int]bool // 1-12
	dow           map[int]bool // 0-6 (7 normalized to 0)
	domRestricted bool
	dowRestricted bool
}

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var dowNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

func parseCron(expr string) (*cronSpec, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (min hour dom month dow), got %d", expr, len(fields))
	}
	minute, err := parseField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: minute: %w", expr, err)
	}
	hour, err := parseField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: hour: %w", expr, err)
	}
	dom, err := parseField(fields[2], 1, 31, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: day-of-month: %w", expr, err)
	}
	month, err := parseField(fields[3], 1, 12, monthNames)
	if err != nil {
		return nil, fmt.Errorf("cron %q: month: %w", expr, err)
	}
	dow, err := parseField(fields[4], 0, 7, dowNames)
	if err != nil {
		return nil, fmt.Errorf("cron %q: day-of-week: %w", expr, err)
	}
	if _, ok := dow[7]; ok { // 7 = Sunday, normalize onto 0
		delete(dow, 7)
		dow[0] = true
	}
	return &cronSpec{
		minute: minute, hour: hour, dom: dom, month: month, dow: dow,
		domRestricted: fields[2] != "*",
		dowRestricted: fields[4] != "*",
	}, nil
}

// parseField expands one cron field into a value set. names maps
// textual tokens (JAN, MON, ...) onto numbers; nil for numeric fields.
func parseField(field string, min, max int, names map[string]int) (map[int]bool, error) {
	out := map[int]bool{}
	for _, tok := range strings.Split(field, ",") {
		if tok == "" {
			return nil, fmt.Errorf("empty list element in %q", field)
		}
		body, step := tok, 1
		if i := strings.IndexByte(tok, '/'); i >= 0 {
			body = tok[:i]
			n, err := strconv.Atoi(tok[i+1:])
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("bad step in %q", tok)
			}
			step = n
		}
		lo, hi := min, max
		switch {
		case body == "*":
			// full range
		case strings.Contains(body, "-"):
			parts := strings.SplitN(body, "-", 2)
			var err error
			if lo, err = parseValue(parts[0], min, max, names); err != nil {
				return nil, err
			}
			if hi, err = parseValue(parts[1], min, max, names); err != nil {
				return nil, err
			}
			if lo > hi {
				return nil, fmt.Errorf("inverted range %q", body)
			}
		default:
			v, err := parseValue(body, min, max, names)
			if err != nil {
				return nil, err
			}
			if step == 1 {
				out[v] = true
				continue
			}
			lo, hi = v, max
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("field %q matches nothing", field)
	}
	return out, nil
}

func parseValue(s string, min, max int, names map[string]int) (int, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if names != nil {
		if v, ok := names[s]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("value %d out of range %d-%d", v, min, max)
	}
	return v, nil
}

// dayMatches applies POSIX dom/dow semantics at t's calendar date.
func (c *cronSpec) dayMatches(t time.Time) bool {
	domOK := c.dom[t.Day()]
	dowOK := c.dow[int(t.Weekday())]
	if c.domRestricted && c.dowRestricted {
		return domOK || dowOK
	}
	if c.domRestricted {
		return domOK
	}
	if c.dowRestricted {
		return dowOK
	}
	return true // neither restricted — every day
}

// Next returns the first fire time strictly after from, at minute
// granularity in from's location. ok=false when nothing matches within
// four years (e.g. Feb 30). The scan walks day-by-day past mismatched
// dates, then hour-by-hour, then minute-by-minute — cheap even for
// sparse expressions like "0 0 29 2 *".
func (c *cronSpec) Next(from time.Time) (time.Time, bool) {
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := from.AddDate(4, 0, 0)
	for t.Before(limit) {
		if !c.month[int(t.Month())] {
			// jump to first minute of next month
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !c.minute[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}
