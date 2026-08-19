package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "sub", "state.json"))
}

func TestStoreRoundTrip(t *testing.T) {
	s := tempStore(t)
	snap := Snapshot{
		UpdatedAt: epoch,
		Checks: map[string]CheckState{
			"Example": {
				Status:              StatusDown,
				ConsecutiveFailures: 3,
				FirstFailureAt:      epoch,
				IncidentStart:       epoch,
				LastChange:          epoch.Add(2 * time.Minute),
			},
		},
	}
	if err := s.Save(snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != SnapshotVersion || !got.UpdatedAt.Equal(epoch) {
		t.Errorf("header = %+v", got)
	}
	if got.Checks["Example"] != snap.Checks["Example"] {
		t.Errorf("state = %+v, want %+v", got.Checks["Example"], snap.Checks["Example"])
	}
}

func TestStoreFileIsPrivate(t *testing.T) {
	s := tempStore(t)
	if err := s.Save(Snapshot{Checks: map[string]CheckState{}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %o, want 600", perm)
	}
}

func TestStoreLeavesNoTemporaryFiles(t *testing.T) {
	s := tempStore(t)
	for range 3 {
		if err := s.Save(Snapshot{Checks: map[string]CheckState{}}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want only the state file: %v", len(entries), entries)
	}
}

// Every way of losing the file degrades to empty state, and never to a refusal
// to start: a monitor that will not start is a monitor that is silent.
func TestStoreDegradesToEmptyState(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"missing file", "", false},
		{"truncated json", `{"version":1,"checks":{"A":`, true},
		{"not json at all", "\x00\x01garbage", true},
		{"future version", `{"version":99,"checks":{"A":{"status":"down"}}}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			snap, err := NewStore(path).Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if len(snap.Checks) != 0 {
				t.Errorf("checks = %v, want an empty state", snap.Checks)
			}
			m := NewMachine()
			m.Restore(snap)
			if s := m.Status("A"); s != StatusUnknown {
				t.Errorf("status = %q, want %q", s, StatusUnknown)
			}
		})
	}
}

// SPEC §9: no loss of state may cause a false alert, in either direction.
func TestLosingTheStateFileProducesNoFalseAlerts(t *testing.T) {
	c := testCheck()
	s := tempStore(t)

	// A check goes down and the outage is reported and persisted.
	m := NewMachine()
	if kinds(feed(m, c, "UUDDD")) != "down" {
		t.Fatal("setup: expected a down event")
	}
	if err := s.Save(m.Snapshot()); err != nil {
		t.Fatal(err)
	}

	t.Run("state survives a restart", func(t *testing.T) {
		snap, err := s.Load()
		if err != nil {
			t.Fatal(err)
		}
		restarted := NewMachine()
		restarted.Restore(snap)
		if restarted.Status(c.Name) != StatusDown {
			t.Fatalf("status after restart = %q, want down", restarted.Status(c.Name))
		}
		// The outage is not announced a second time...
		if got := kinds(feed(restarted, c, "DD")); got != "" {
			t.Errorf("events = %q, want none: the outage was already reported", got)
		}
		// ...and the recovery still is, because we know there was an outage.
		if got := kinds(feed(restarted, c, "UU")); got != "up" {
			t.Errorf("events = %q, want %q", got, "up")
		}
	})

	t.Run("losing the file while down invents no recovery", func(t *testing.T) {
		if err := os.Remove(s.Path()); err != nil {
			t.Fatal(err)
		}
		snap, err := s.Load()
		if err != nil {
			t.Fatalf("a missing state file must not be an error: %v", err)
		}
		restarted := NewMachine()
		restarted.Restore(snap)

		// The service is fine now. Nobody was told it was down by this
		// process, so nobody may be told it recovered.
		if got := kinds(feed(restarted, c, "UUUUU")); got != "" {
			t.Errorf("events = %q, want none after losing the state file", got)
		}
	})

	t.Run("losing the file while up invents no outage", func(t *testing.T) {
		restarted := NewMachine()
		restarted.Restore(Snapshot{Checks: map[string]CheckState{}})
		if got := kinds(feed(restarted, c, "UUUUU")); got != "" {
			t.Errorf("events = %q, want none: an empty state is not an outage", got)
		}
		if restarted.Status(c.Name) != StatusUp {
			t.Errorf("status = %q, want up", restarted.Status(c.Name))
		}
	})

	t.Run("a real outage after state loss is still reported", func(t *testing.T) {
		restarted := NewMachine()
		restarted.Restore(Snapshot{Checks: map[string]CheckState{}})
		if got := kinds(feed(restarted, c, "DDD")); got != "down" {
			t.Errorf("events = %q, want %q: silence must not be the fallback either", got, "down")
		}
	})
}

// The instability cooldown is durable, or a crash loop becomes a
// notification loop.
func TestInstabilityCooldownSurvivesRestart(t *testing.T) {
	c := testCheck()
	c.Instability.Cooldown = time.Hour

	m := NewMachine()
	events := feed(m, c, "DUDUDUDUDU")
	if kinds(events) != "unstable" {
		t.Fatalf("setup: events = %q, want one unstable event", kinds(events))
	}

	s := tempStore(t)
	if err := s.Save(m.Snapshot()); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewMachine()
	restarted.Restore(snap)

	// Same alternating pattern half an hour later, still inside the cooldown.
	if got := kinds(feedFrom(restarted, c, "DUDUDUDUDU", epoch.Add(30*time.Minute))); got != "" {
		t.Errorf("events = %q, want none while the cooldown holds", got)
	}
}
