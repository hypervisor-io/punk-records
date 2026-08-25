package memory

import (
	"testing"
	"time"
)

func TestParseTemporal(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		query   string
		wantOK  bool
		from    time.Time
		to      time.Time
		cleaned string
	}{
		{
			name:    "last month",
			query:   "deploy incidents last month",
			wantOK:  true,
			from:    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			cleaned: "deploy incidents",
		},
		{
			name:    "bare year",
			query:   "what changed in 2025",
			wantOK:  true,
			from:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			cleaned: "what changed",
		},
		{
			name:    "last N days",
			query:   "errors last 3 days",
			wantOK:  true,
			from:    time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
			to:      now,
			cleaned: "errors",
		},
		{
			name:    "last spring",
			query:   "retro last spring",
			wantOK:  true,
			from:    time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			cleaned: "retro",
		},
		{
			name:   "no phrase",
			query:  "just a normal query",
			wantOK: false,
		},
		{
			name:    "yesterday",
			query:   "what happened yesterday",
			wantOK:  true,
			from:    time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC),
			cleaned: "what happened",
		},
		{
			name:    "last week",
			query:   "standups last week",
			wantOK:  true,
			from:    time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC),
			cleaned: "standups",
		},
		{
			name:    "last year",
			query:   "revenue last year",
			wantOK:  true,
			from:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			cleaned: "revenue",
		},
		{
			name:    "last N weeks",
			query:   "on-call last 2 weeks",
			wantOK:  true,
			from:    now.AddDate(0, 0, -14),
			to:      now,
			cleaned: "on-call",
		},
		{
			name:    "in month year",
			query:   "postmortem in june 2026",
			wantOK:  true,
			from:    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			cleaned: "postmortem",
		},
		{
			name:    "month abbrev year, no in",
			query:   "outage jan 2026 report",
			wantOK:  true,
			from:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			cleaned: "outage report",
		},
		{
			name:    "last fall/autumn synonym",
			query:   "hiring last autumn",
			wantOK:  true,
			from:    time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
			cleaned: "hiring",
		},
		{
			// now (2026-07-07) is mid-year: the completed winter is the one
			// that just ended, Dec 2025 - Mar 2026.
			name:    "last winter, now mid-year",
			query:   "report last winter",
			wantOK:  true,
			from:    time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
			to:      time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			cleaned: "report",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, to, cleaned, ok := ParseTemporal(tt.query, now)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if !from.Equal(tt.from) {
				t.Errorf("from = %v, want %v", from, tt.from)
			}
			if !to.Equal(tt.to) {
				t.Errorf("to = %v, want %v", to, tt.to)
			}
			if cleaned != tt.cleaned {
				t.Errorf("cleaned = %q, want %q", cleaned, tt.cleaned)
			}
		})
	}
}

// TestParseTemporalLastWinterBoundary is the regression case for the
// Dec-year-crossing bug: when now falls in Jan/Feb, the naive "this year's
// December" anchor points at a winter that hasn't happened yet, so a single
// step-back landed on the still-in-progress winter (to > now). The window
// returned for "last winter" must always be a completed one (to <= now).
func TestParseTemporalLastWinterBoundary(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		from time.Time
		to   time.Time
	}{
		{
			name: "now in january",
			now:  time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
			from: time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC),
			to:   time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "now mid-year",
			now:  time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC),
			from: time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, to, _, ok := ParseTemporal("report last winter", tt.now)
			if !ok {
				t.Fatal("ok = false")
			}
			if to.After(tt.now) {
				t.Fatalf("to = %v is after now = %v: winter window not completed", to, tt.now)
			}
			if !from.Equal(tt.from) || !to.Equal(tt.to) {
				t.Errorf("window = [%v, %v), want [%v, %v)", from, to, tt.from, tt.to)
			}
		})
	}
}
