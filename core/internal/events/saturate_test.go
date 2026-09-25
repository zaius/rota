package events

import (
	"math"
	"testing"
)

func TestSaturateInt32(t *testing.T) {
	for in, want := range map[int]int32{
		0:                     0,
		90_000:                90_000,
		math.MaxInt32 + 1:     math.MaxInt32,
		40 * 24 * 3600 * 1000: math.MaxInt32, // a 40-day tunnel in milliseconds
		math.MinInt32 - 1:     math.MinInt32,
	} {
		if got := saturateInt32(in); got != want {
			t.Errorf("saturateInt32(%d) = %d, want %d", in, got, want)
		}
	}
}
