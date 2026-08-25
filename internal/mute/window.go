// Package mute decides whether a check is in a quiet window.
//
// Two sources feed it: static windows from the config, and ad-hoc holds
// created at runtime. Probes always run; this package only answers
// "may we deliver this event right now?" and remembers what we didn't.
package mute

import (
	"time"

	"github.com/eeegoloauq/lookout/internal/config"
)

// Covers reports whether a window applies to this check. An empty
// group or check on the window means "all of them".
func Covers(w config.MuteWindow, group, check string) bool {
	if w.Group != "" && w.Group != group {
		return false
	}
	if w.Check != "" && w.Check != check {
		return false
	}
	return true
}

// Active reports whether w is quiet at now, and if so when it lifts.
// A window that spans midnight is still the window of the day it started.
func Active(w config.MuteWindow, now time.Time) (until time.Time, ok bool) {
	loc := w.Location
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)
	days := int(w.Duration/(24*time.Hour)) + 1
	if days < 1 {
		days = 1
	}
	for delta := 0; delta <= days; delta++ {
		day := now.AddDate(0, 0, -delta)
		if !dayMatch(w.Every, day.Weekday()) {
			continue
		}
		y, m, d := day.Date()
		start := time.Date(y, m, d, 0, 0, 0, 0, loc).Add(w.At)
		end := start.Add(w.Duration)
		if !now.Before(start) && now.Before(end) {
			return end, true
		}
	}
	return time.Time{}, false
}

// NextBoundary is the next time a window starts or ends after now.
// Used so the process can flush a held digest when a schedule lifts
// without waiting for the next probe.
func NextBoundary(w config.MuteWindow, now time.Time) (time.Time, bool) {
	loc := w.Location
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)
	if end, ok := Active(w, now); ok {
		return end, true
	}
	// Look up to a week plus a day: every is at most weekly.
	var next time.Time
	for i := 0; i <= 8; i++ {
		day := now.AddDate(0, 0, i)
		if !dayMatch(w.Every, day.Weekday()) {
			continue
		}
		y, m, d := day.Date()
		start := time.Date(y, m, d, 0, 0, 0, 0, loc).Add(w.At)
		if start.After(now) && (next.IsZero() || start.Before(next)) {
			next = start
		}
	}
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

func dayMatch(every []time.Weekday, day time.Weekday) bool {
	if len(every) == 0 {
		return true
	}
	for _, d := range every {
		if d == day {
			return true
		}
	}
	return false
}
