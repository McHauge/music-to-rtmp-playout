package playout

import (
	"testing"
	"time"
)

// Status.NextIndex is what the rundown's "up next" marker follows; it must
// mirror player.nextIdx exactly or the console highlights the wrong row.
func TestStatusNextIndex(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		want   int
	}{
		{"nothing queued", Status{ItemIndex: 3, QueuedIndex: -1}, 4},
		{"queued wins", Status{ItemIndex: 3, QueuedIndex: 7}, 7},
		{"queued may point backwards", Status{ItemIndex: 5, QueuedIndex: 1}, 1},
		{"queue index zero is a real target, not 'unset'", Status{ItemIndex: 5, QueuedIndex: 0}, 0},
		{"past the end is allowed — it simply matches no row", Status{ItemIndex: 9, QueuedIndex: -1}, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.NextIndex(); got != tt.want {
				t.Errorf("NextIndex() = %d, want %d", got, tt.want)
			}
		})
	}
}

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
