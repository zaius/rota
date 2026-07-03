package services

import (
	"testing"
	"time"
)

// TestCronDueAt covers the scheduler predicate that decides whether a pool's
// health-check cron fires in the current minute. The previous implementation
// silently ran any non-"*/N" expression every 30 minutes; these cases lock in
// that arbitrary standard cron expressions are honoured and that empty/invalid
// expressions never fire.
func TestCronDueAt(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("bad test time %q: %v", s, err)
		}
		return ts
	}

	cases := []struct {
		name string
		expr string
		now  string
		want bool
	}{
		{"every-5-min due at :10", "*/5 * * * *", "2026-07-02T12:10:30Z", true},
		{"every-5-min not due at :12", "*/5 * * * *", "2026-07-02T12:12:30Z", false},
		{"every-6-hours due at 12:00", "0 */6 * * *", "2026-07-02T12:00:20Z", true},
		{"every-6-hours not due at 12:01", "0 */6 * * *", "2026-07-02T12:01:20Z", false},
		{"every-6-hours not due at 13:00", "0 */6 * * *", "2026-07-02T13:00:20Z", false},
		{"daily 09:30 due", "30 9 * * *", "2026-07-02T09:30:05Z", true},
		{"daily 09:30 not due at 09:31", "30 9 * * *", "2026-07-02T09:31:05Z", false},
		{"every-minute always due", "* * * * *", "2026-07-02T00:00:00Z", true},
		{"empty never due", "", "2026-07-02T12:00:00Z", false},
		{"whitespace never due", "   ", "2026-07-02T12:00:00Z", false},
		{"invalid never due", "not a cron", "2026-07-02T12:00:00Z", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cronDueAt(tc.expr, at(tc.now)); got != tc.want {
				t.Fatalf("cronDueAt(%q, %s) = %v, want %v", tc.expr, tc.now, got, tc.want)
			}
		})
	}
}

// TestCronDueAtFiresOncePerDueMinute verifies that across a minute's worth of
// per-second ticks, an every-5-minute schedule reports due for exactly the ticks
// inside the due minute (the scheduler ticks once a minute, but this guards the
// window arithmetic against off-by-one at the minute boundaries).
func TestCronDueAtFiresOncePerDueMinute(t *testing.T) {
	start := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	dueMinute, notDueMinute := 0, 0
	for i := 0; i < 120; i++ { // two minutes, second by second
		now := start.Add(time.Duration(i) * time.Second)
		if cronDueAt("*/5 * * * *", now) {
			if now.Minute() == 0 {
				dueMinute++
			} else {
				notDueMinute++
			}
		}
	}
	if dueMinute != 60 {
		t.Fatalf("expected all 60 ticks in the due minute to report due, got %d", dueMinute)
	}
	if notDueMinute != 0 {
		t.Fatalf("expected no ticks in the non-due minute to report due, got %d", notDueMinute)
	}
}
