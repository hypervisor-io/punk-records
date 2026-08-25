package memory

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var monthNames = map[string]time.Month{
	"jan": time.January, "january": time.January,
	"feb": time.February, "february": time.February,
	"mar": time.March, "march": time.March,
	"apr": time.April, "april": time.April,
	"may": time.May,
	"jun": time.June, "june": time.June,
	"jul": time.July, "july": time.July,
	"aug": time.August, "august": time.August,
	"sep": time.September, "september": time.September,
	"oct": time.October, "october": time.October,
	"nov": time.November, "november": time.November,
	"dec": time.December, "december": time.December,
}

// ponytail: N-hemisphere meteorological seasons; make configurable if a user is south of the equator
var seasonStartMonths = map[string]time.Month{
	"spring": time.March,
	"summer": time.June,
	"fall":   time.September,
	"autumn": time.September,
	"winter": time.December,
}

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func monthStart(year int, month time.Month, loc *time.Location) time.Time {
	return time.Date(year, month, 1, 0, 0, 0, 0, loc)
}

type temporalMatcher struct {
	re      *regexp.Regexp
	resolve func(m []string, now time.Time) (time.Time, time.Time)
}

// temporalMatchers is ordered most-specific-first: multi-word/numeric forms
// before their bare counterparts (last N months before last month; in
// <month> <year> before bare <year>) so the first match always wins the
// more precise interpretation.
var temporalMatchers = []temporalMatcher{
	{
		re: regexp.MustCompile(`(?i)\byesterday\b`),
		resolve: func(_ []string, now time.Time) (time.Time, time.Time) {
			today := midnight(now)
			return today.AddDate(0, 0, -1), today
		},
	},
	{
		re: regexp.MustCompile(`(?i)\blast\s+(\d+)\s+days?\b`),
		resolve: func(m []string, now time.Time) (time.Time, time.Time) {
			n, _ := strconv.Atoi(m[1])
			return now.AddDate(0, 0, -n), now
		},
	},
	{
		re: regexp.MustCompile(`(?i)\blast\s+(\d+)\s+weeks?\b`),
		resolve: func(m []string, now time.Time) (time.Time, time.Time) {
			n, _ := strconv.Atoi(m[1])
			return now.AddDate(0, 0, -n*7), now
		},
	},
	{
		re: regexp.MustCompile(`(?i)\blast\s+(\d+)\s+months?\b`),
		resolve: func(m []string, now time.Time) (time.Time, time.Time) {
			n, _ := strconv.Atoi(m[1])
			return now.AddDate(0, -n, 0), now
		},
	},
	{
		re: regexp.MustCompile(`(?i)\blast\s+week\b`),
		resolve: func(_ []string, now time.Time) (time.Time, time.Time) {
			today := midnight(now)
			offset := int(today.Weekday())
			if offset == 0 {
				offset = 7 // treat Sunday as day 7 of a Mon-Sun week
			}
			mondayThisWeek := today.AddDate(0, 0, -(offset - 1))
			return mondayThisWeek.AddDate(0, 0, -7), mondayThisWeek
		},
	},
	{
		re: regexp.MustCompile(`(?i)\blast\s+month\b`),
		resolve: func(_ []string, now time.Time) (time.Time, time.Time) {
			firstThisMonth := monthStart(now.Year(), now.Month(), now.Location())
			return firstThisMonth.AddDate(0, -1, 0), firstThisMonth
		},
	},
	{
		re: regexp.MustCompile(`(?i)\blast\s+year\b`),
		resolve: func(_ []string, now time.Time) (time.Time, time.Time) {
			firstThisYear := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location())
			return firstThisYear.AddDate(-1, 0, 0), firstThisYear
		},
	},
	{
		re: regexp.MustCompile(`(?i)\blast\s+(spring|summer|fall|autumn|winter)\b`),
		resolve: func(m []string, now time.Time) (time.Time, time.Time) {
			season := strings.ToLower(m[1])
			startMonth := seasonStartMonths[season]
			year := now.Year()
			// Winter's Dec-Mar span crosses the calendar-year boundary: in
			// Jan/Feb, "this year's" winter started in December of the
			// previous year. Anchor there first so the single step-back
			// below (shared with the other seasons) still lands on a fully
			// completed window.
			if season == "winter" && now.Month() <= time.February {
				year--
			}
			start := monthStart(year, startMonth, now.Location())
			end := start.AddDate(0, 3, 0)
			if end.After(now) {
				year--
				start = monthStart(year, startMonth, now.Location())
				end = start.AddDate(0, 3, 0)
			}
			return start, end
		},
	},
	{
		re: regexp.MustCompile(`(?i)\b(?:in\s+)?(january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|oct|nov|dec)\s+(20\d{2})\b`),
		resolve: func(m []string, now time.Time) (time.Time, time.Time) {
			mo := monthNames[strings.ToLower(m[1])]
			year, _ := strconv.Atoi(m[2])
			start := monthStart(year, mo, now.Location())
			return start, start.AddDate(0, 1, 0)
		},
	},
	{
		re: regexp.MustCompile(`(?i)\b(?:in\s+)?(20\d{2})\b`),
		resolve: func(m []string, now time.Time) (time.Time, time.Time) {
			year, _ := strconv.Atoi(m[1])
			start := time.Date(year, 1, 1, 0, 0, 0, 0, now.Location())
			return start, start.AddDate(1, 0, 0)
		},
	},
}

// ParseTemporal extracts a time window from a natural-language query
// relative to now. Returns the window, the query with the temporal
// phrase removed (so FTS matches content words, not "last spring"),
// and ok=false when no phrase is recognized. Deterministic — no model.
func ParseTemporal(query string, now time.Time) (from, to time.Time, cleaned string, ok bool) {
	for _, tm := range temporalMatchers {
		loc := tm.re.FindStringSubmatchIndex(query)
		if loc == nil {
			continue
		}
		m := make([]string, len(loc)/2)
		for i := range m {
			if loc[2*i] >= 0 {
				m[i] = query[loc[2*i]:loc[2*i+1]]
			}
		}
		from, to = tm.resolve(m, now)
		cleaned = strings.Join(strings.Fields(query[:loc[0]]+" "+query[loc[1]:]), " ")
		return from, to, cleaned, true
	}
	return time.Time{}, time.Time{}, query, false
}
