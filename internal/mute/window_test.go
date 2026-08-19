package mute

import (
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/config"
)

func TestActiveWeekendWindow(t *testing.T) {
	w := config.MuteWindow{
		Every:    []time.Weekday{time.Saturday, time.Sunday},
		At:       2 * time.Hour,
		Duration: 3 * time.Hour,
		Location: time.UTC,
	}
	// 2026-08-22 is a Saturday.
	sat := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		at   time.Time
		want bool
	}{
		{sat.Add(2*time.Hour - time.Second), false},
		{sat.Add(2 * time.Hour), true},
		{sat.Add(4*time.Hour + 59*time.Minute), true},
		{sat.Add(5 * time.Hour), false},
		{sat.Add(-24 * time.Hour).Add(2 * time.Hour), false}, // Friday
		{sat.Add(24 * time.Hour).Add(2 * time.Hour), true},   // Sunday
	}
	for _, tc := range tests {
		_, ok := Active(w, tc.at)
		if ok != tc.want {
			t.Errorf("Active(%s) = %v, want %v", tc.at.Format("Mon 15:04"), ok, tc.want)
		}
	}
}

func TestActiveSpansMidnight(t *testing.T) {
	w := config.MuteWindow{
		Every:    []time.Weekday{time.Saturday},
		At:       23 * time.Hour,
		Duration: 3 * time.Hour,
		Location: time.UTC,
	}
	sat := time.Date(2026, 8, 22, 23, 30, 0, 0, time.UTC)
	sun := time.Date(2026, 8, 23, 1, 30, 0, 0, time.UTC)
	end, ok := Active(w, sat)
	if !ok {
		t.Fatal("Saturday 23:30 must be inside the window")
	}
	if !end.Equal(time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("until = %s", end)
	}
	if _, ok := Active(w, sun); !ok {
		t.Fatal("Sunday 01:30 is still Saturday's window")
	}
	if _, ok := Active(w, time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)); ok {
		t.Fatal("Sunday 02:00 must be outside")
	}
}

func TestActiveEveryDayWhenEveryIsEmpty(t *testing.T) {
	w := config.MuteWindow{
		At:       4 * time.Hour,
		Duration: time.Hour,
		Location: time.UTC,
	}
	wed := time.Date(2026, 8, 19, 4, 15, 0, 0, time.UTC)
	if _, ok := Active(w, wed); !ok {
		t.Fatal("an empty every: must fire every day")
	}
}

func TestCoversGroupAndCheck(t *testing.T) {
	w := config.MuteWindow{Group: "Public"}
	if !Covers(w, "Public", "MX") || Covers(w, "Core", "Router") {
		t.Fatal("group filter")
	}
	w.Check = "MX"
	if !Covers(w, "Public", "MX") || Covers(w, "Public", "NS") {
		t.Fatal("check filter")
	}
}

func TestNextBoundaryIsTheUpcomingStart(t *testing.T) {
	w := config.MuteWindow{
		Every:    []time.Weekday{time.Saturday},
		At:       2 * time.Hour,
		Duration: time.Hour,
		Location: time.UTC,
	}
	wed := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	next, ok := NextBoundary(w, wed)
	if !ok {
		t.Fatal("want a next Saturday")
	}
	want := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %s, want %s", next, want)
	}
}
