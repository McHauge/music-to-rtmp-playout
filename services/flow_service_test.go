package services

import "testing"

// SumRuntimeSec drives the rundown footer's "est. runtime". Gates contribute
// nothing (they wait for an operator, so their length is unknowable), and a
// song row whose track was deleted must not panic.
func TestSumRuntimeSec(t *testing.T) {
	tests := []struct {
		name  string
		items []FlowItem
		want  float64
	}{
		{"empty", nil, 0},
		{
			"songs and breaks",
			[]FlowItem{
				{Type: ItemSong, Track: &Track{DurationSec: 180.5}},
				{Type: ItemBreak, BreakSec: 20},
				{Type: ItemSong, Track: &Track{DurationSec: 200}},
			},
			400.5,
		},
		{
			"gate contributes nothing",
			[]FlowItem{
				{Type: ItemGate, Label: "Top of hour"},
				{Type: ItemSong, Track: &Track{DurationSec: 60}},
			},
			60,
		},
		{
			"song with a missing track is skipped, not a nil deref",
			[]FlowItem{
				{Type: ItemSong, Track: nil},
				{Type: ItemBreak, BreakSec: 5},
			},
			5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SumRuntimeSec(tt.items); got != tt.want {
				t.Errorf("SumRuntimeSec = %v, want %v", got, tt.want)
			}
		})
	}
}
