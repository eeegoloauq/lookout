// Package state holds what lookout must remember: the confirmed up/down state
// of every check (durable, a single JSON file) and the recent result window
// used by the instability detector (in memory).
//
// The invariant that shapes everything here: losing the state file must never
// produce a false alert, in either direction. An unknown check is "not known
// yet", never "down", and never "recovered".
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SnapshotVersion is the on-disk format version. A snapshot written by a
// different version is discarded rather than guessed at — starting from empty
// state is safe by construction, misreading old state is not.
const SnapshotVersion = 1

// Status is the confirmed state of a check.
type Status string

const (
	// StatusUnknown means no threshold has been reached yet: at startup, or
	// after the state file was lost. It never alerts by itself.
	StatusUnknown Status = "unknown"
	StatusUp      Status = "up"
	StatusDown    Status = "down"
)

// CheckState is the durable state of one check (SPEC §9.1).
type CheckState struct {
	Status               Status `json:"status"`
	ConsecutiveFailures  int    `json:"consecutive_failures,omitempty"`
	ConsecutiveSuccesses int    `json:"consecutive_successes,omitempty"`
	// FirstFailureAt is when the current run of failures began; it becomes
	// IncidentStart once the failure threshold confirms the outage.
	FirstFailureAt time.Time `json:"first_failure_at,omitempty"`
	IncidentStart  time.Time `json:"incident_start,omitempty"`
	LastChange     time.Time `json:"last_change,omitempty"`
	// Unstable and UnstableNoticeAt implement the cooldown of the "N of the
	// last M" detector: the fact that a notice was sent has to survive a
	// restart, or a crash loop turns into a notification loop.
	Unstable         bool      `json:"unstable,omitempty"`
	UnstableNoticeAt time.Time `json:"unstable_notice_at,omitempty"`
}

// Snapshot is the whole durable state, as written to disk.
type Snapshot struct {
	Version   int                   `json:"version"`
	UpdatedAt time.Time             `json:"updated_at"`
	Checks    map[string]CheckState `json:"checks"`
}

// Store reads and writes the durable state file.
type Store struct {
	path string
}

// NewStore returns a store backed by the file at path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path returns the file the store writes to.
func (s *Store) Path() string { return s.path }

// Load reads the state file. A missing file is not an error: it is an empty
// state, which is the correct starting point. A corrupt or foreign file yields
// an empty state *and* an error — the caller is expected to log it and carry on
// monitoring rather than refuse to start.
func (s *Store) Load() (Snapshot, error) {
	empty := Snapshot{Version: SnapshotVersion, Checks: map[string]CheckState{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, nil
		}
		return empty, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return empty, fmt.Errorf("state file %s is corrupt: %w", s.path, err)
	}
	if snap.Version != SnapshotVersion {
		return empty, fmt.Errorf("state file %s has version %d, this build writes version %d; ignoring it", s.path, snap.Version, SnapshotVersion)
	}
	if snap.Checks == nil {
		snap.Checks = map[string]CheckState{}
	}
	return snap, nil
}

// Save writes the snapshot atomically: a temporary file in the same directory,
// fsync, rename, then fsync of the directory. A crash mid-write leaves either
// the old state or the new one, never a truncated file.
func (s *Store) Save(snap Snapshot) error {
	snap.Version = SnapshotVersion
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
