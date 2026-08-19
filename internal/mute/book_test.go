package mute

import (
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/state"
)

func TestAdhocMuteCatchesThenReleasesADigest(t *testing.T) {
	b := NewBook(nil)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	if _, err := b.Mute(now, 30*time.Minute, "Public", ""); err != nil {
		t.Fatal(err)
	}
	ev := state.Event{Kind: state.EventDown, Check: "MX", Group: "Public", At: now, Alert: true}
	if !b.Catch(ev, now) {
		t.Fatal("a muted group must catch the event")
	}
	if b.Catch(state.Event{Kind: state.EventDown, Check: "Router", Group: "Core", At: now, Alert: true}, now) {
		t.Fatal("a different group must still deliver")
	}

	// Still inside the window: nothing to flush.
	if got := b.Expire(now.Add(10 * time.Minute)); len(got) != 0 {
		t.Fatalf("expire while muted: %+v", got)
	}

	got := b.Expire(now.Add(31 * time.Minute))
	if len(got) != 1 || got[0].Kind != state.EventHeld {
		t.Fatalf("after mute: %+v", got)
	}
	if got[0].Summary == nil || got[0].Summary.Count != 1 {
		t.Fatalf("digest = %+v, the down event was dropped", got[0].Summary)
	}
	if got[0].Summary.ByKind["down"] != 1 || got[0].Summary.Checks[0] != "MX" {
		t.Errorf("digest = %+v", got[0].Summary)
	}
}

func TestUnmuteReleasesTheDigest(t *testing.T) {
	b := NewBook(nil)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	b.Mute(now, time.Hour, "", "")
	b.Catch(state.Event{Kind: state.EventDown, Check: "Photos", Group: "Services", At: now, Alert: true}, now)
	got := b.Unmute(now.Add(time.Minute), "", "")
	if len(got) != 1 || got[0].Kind != state.EventHeld || got[0].Summary.Count != 1 {
		t.Fatalf("unmute: %+v", got)
	}
	if b.Muted("Services", "Photos", now.Add(time.Minute)) {
		t.Fatal("unmute must lift the hold")
	}
}

func TestMuteSurvivesRestore(t *testing.T) {
	b := NewBook(nil)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	b.Mute(now, time.Hour, "Public", "")
	b.Catch(state.Event{Kind: state.EventDown, Check: "MX", Group: "Public", At: now, Alert: true}, now)
	snap := b.Snapshot()

	restored := NewBook(nil)
	restored.Restore(snap)
	if !restored.Muted("Public", "MX", now.Add(time.Minute)) {
		t.Fatal("restored mute is gone")
	}
	got := restored.Expire(now.Add(2 * time.Hour))
	if len(got) != 1 || got[0].Summary.Count != 1 {
		t.Fatalf("restored digest lost: %+v", got)
	}
}

func TestScheduleWindowCatchesWithoutAnAdhocMute(t *testing.T) {
	w := config.MuteWindow{
		Every:    []time.Weekday{time.Saturday},
		At:       2 * time.Hour,
		Duration: 3 * time.Hour,
		Location: time.UTC,
		Group:    "Public",
	}
	b := NewBook([]config.MuteWindow{w})
	sat := time.Date(2026, 8, 22, 2, 30, 0, 0, time.UTC)
	if !b.Catch(state.Event{Kind: state.EventDown, Check: "MX", Group: "Public", At: sat, Alert: true}, sat) {
		t.Fatal("scheduled window must catch")
	}
	got := b.Expire(sat.Add(3 * time.Hour))
	if len(got) != 1 || got[0].Kind != state.EventHeld {
		t.Fatalf("schedule lift: %+v", got)
	}
}

func TestMuteRejectsTooLong(t *testing.T) {
	b := NewBook(nil)
	_, err := b.Mute(time.Now(), 8*24*time.Hour, "", "")
	if err == nil {
		t.Fatal("an 8-day mute must be rejected")
	}
}

func TestRepeatingMuteExtendsAndKeepsTheDigest(t *testing.T) {
	b := NewBook(nil)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	b.Mute(now, 10*time.Minute, "Public", "")
	b.Catch(state.Event{Kind: state.EventDown, Check: "MX", Group: "Public", At: now, Alert: true}, now)
	b.Mute(now.Add(5*time.Minute), 30*time.Minute, "Public", "")
	if !b.Muted("Public", "MX", now.Add(20*time.Minute)) {
		t.Fatal("extended mute ended too soon")
	}
	got := b.Expire(now.Add(40 * time.Minute))
	if len(got) != 1 || got[0].Summary.Count != 1 {
		t.Fatalf("digest after extend: %+v", got)
	}
}
