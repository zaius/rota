package events

import (
	"testing"
	"time"
)

// Regression: every daily bucket starts at midnight, so each one was labeled
// 00:00.
func TestChartBucketLabel(t *testing.T) {
	bucket := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"1h": "00:00",
		"4h": "00:00",
		"1d": "19 Sep",
	}
	for interval, want := range cases {
		if got := chartBucketLabel(bucket, interval); got != want {
			t.Errorf("chartBucketLabel(%s) = %q, want %q", interval, got, want)
		}
	}
}
