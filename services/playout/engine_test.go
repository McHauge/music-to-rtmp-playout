package playout

import (
	"testing"
	"time"
)

func TestEncReconnectBackoff(t *testing.T) {
	// Doubles from the min, clamped at the max, and never returns a bad value
	// even for large / non-positive attempt counts (shift-overflow guard).
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, encReconnectBackoffMin}, // clamped to n=1
		{1, encReconnectBackoffMin},
		{2, 2 * encReconnectBackoffMin},
		{3, 4 * encReconnectBackoffMin},
		{100, encReconnectBackoffMax}, // far past the cap
	}
	for _, c := range cases {
		got := encReconnectBackoff(c.n)
		if got != c.want {
			t.Errorf("encReconnectBackoff(%d)=%s, want %s", c.n, got, c.want)
		}
		if got < encReconnectBackoffMin || got > encReconnectBackoffMax {
			t.Errorf("encReconnectBackoff(%d)=%s out of [%s,%s]", c.n, got,
				encReconnectBackoffMin, encReconnectBackoffMax)
		}
	}
}
