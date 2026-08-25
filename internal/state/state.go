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
	"reflect"
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

// CheckState is the durable state of one check.
type CheckState struct {
	Status               Status `json:"status"`
	ConsecutiveFailures  int    `json:"consecutive_failures,omitempty"`
	ConsecutiveSuccesses int    `json:"consecutive_successes,omitempty"`
	// FirstFailureAt is when the current run of failures began; it becomes
	// IncidentStart once the failure threshold confirms the outage.
	FirstFailureAt time.Time `json:"first_failure_at,omitzero"`
	IncidentStart  time.Time `json:"incident_start,omitzero"`
	LastChange     time.Time `json:"last_change,omitzero"`
	// Unstable and UnstableNoticeAt implement the cooldown of the "N of the
	// last M" detector: the fact that a notice was sent has to survive a
	// restart, or a crash loop turns into a notification loop.
	Unstable         bool      `json:"unstable,omitempty"`
	UnstableNoticeAt time.Time `json:"unstable_notice_at,omitzero"`

	// LastFailureAt and LastFailureReason are the most recent failing
	// probe and why it failed, kept so the status page can answer "why is
	// it down" without the operator going to the logs. Durable because a
	// restart during an outage is exactly when that question is asked.
	LastFailureAt     time.Time `json:"last_failure_at,omitzero"`
	LastFailureReason string    `json:"last_failure_reason,omitempty"`

	// Incidents is the log of closed outages, newest first and capped at
	// MaxIncidents. Counting incidents per day answers "how often"; only
	// a log answers "what happened at 03:40 last night".
	Incidents []Incident `json:"incidents,omitempty"`

	// RemoteAddr is the address the last probe connected to. Kept so the
	// page can answer "am I still talking to the old box?" after a
	// migration, without anyone reaching for dig.
	RemoteAddr string `json:"remote_addr,omitempty"`

	// DownNoticeAt is when the operator was last told about the current
	// incident — the DOWN alert or the latest reminder — and DownReminders
	// how many reminders it has already produced. Both are durable for the
	// same reason UnstableNoticeAt is: a restart must not re-page.
	DownNoticeAt  time.Time `json:"down_notice_at,omitzero"`
	DownReminders int       `json:"down_reminders,omitempty"`

	// ZoneSnapshot is the last decoded DNS answer set. The first
	// successful decode is a baseline, not a drift.
	ZoneSnapshot   string    `json:"zone_snapshot,omitempty"`
	ZoneSnapshotAt time.Time `json:"zone_snapshot_at,omitzero"`

	// Certificate expiry, taken from the HTTPS handshake. TiersFired is
	// a bitmask of numbered thresholds already sent; DailyOn is the UTC
	// date of the last daily notice after the last numbered tier.
	CertNotAfter   time.Time `json:"cert_not_after,omitzero"`
	CertTiersFired uint32    `json:"cert_tiers_fired,omitempty"`
	CertDailyOn    string    `json:"cert_daily_on,omitempty"`

	// Domain expiry from RDAP or WHOIS. UnknownSince is when the
	// registry stopped answering; a stale-source alert fires only after
	// DomainStaleAfter.
	DomainExpiresAt    time.Time `json:"domain_expires_at,omitzero"`
	DomainFreeDate     time.Time `json:"domain_free_date,omitzero"`
	DomainState        string    `json:"domain_state,omitempty"`
	DomainSource       string    `json:"domain_source,omitempty"`
	DomainUpdatedAt    time.Time `json:"domain_updated_at,omitzero"`
	DomainUnknownSince time.Time `json:"domain_unknown_since,omitzero"`
	DomainTiersFired   uint32    `json:"domain_tiers_fired,omitempty"`
	DomainDailyOn      string    `json:"domain_daily_on,omitempty"`
	DomainStaleNotice  bool      `json:"domain_stale_notice,omitempty"`

	// DomainDelegated / DomainDelegationKnown / DomainUndelegatedNotice
	// track the tcinet DELEGATED flag. gTLD status codes do not use it,
	// so a missing token is only an emergency after we have seen the
	// tcinet vocabulary (or REGISTERED without DELEGATED on first sight).
	DomainDelegated         bool `json:"domain_delegated,omitempty"`
	DomainDelegationKnown   bool `json:"domain_delegation_known,omitempty"`
	DomainUndelegatedNotice bool `json:"domain_undelegated_notice,omitempty"`
}

// MaxIncidents is how many closed outages are kept per check. Ten covers
// "has this been happening all week"; a full ledger belongs in the JSONL
// history, not in the file that is rewritten on every state change.
const MaxIncidents = 10

// Incident is one closed outage.
type Incident struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Reason string    `json:"reason,omitempty"`
}

// Duration is how long the outage lasted.
func (i Incident) Duration() time.Duration {
	if i.Start.IsZero() || i.End.Before(i.Start) {
		return 0
	}
	return i.End.Sub(i.Start)
}

// sameAs reports whether two states are identical. A plain == stopped
// compiling when the incident log arrived (a struct holding a slice is not
// comparable); this runs once per probe on a small struct, which is nothing
// next to the probe itself.
func (cs CheckState) sameAs(other CheckState) bool {
	return reflect.DeepEqual(cs, other)
}

// Snapshot is the whole durable state, as written to disk.
type Snapshot struct {
	Version   int                   `json:"version"`
	UpdatedAt time.Time             `json:"updated_at,omitzero"`
	Checks    map[string]CheckState `json:"checks"`
	// Outbox is the undelivered alert queue. It lives in this file so a state
	// change and the notification it must produce are persisted together:
	// surviving a restart with "already down" and no queued alert is the
	// failure this design exists to prevent.
	Outbox Outbox `json:"outbox"`

	// Registry is the cached IANA RDAP bootstrap and WHOIS referrals.
	// It is process-wide, not per-check, and is safe to lose: the next
	// domain probe refetches it.
	Registry RegistryCache `json:"registry,omitzero"`

	// Holds are the durable ad-hoc (and in-flight schedule) mutes.
	// A restart must not lift a mute the operator asked for, and must
	// not forget the digest of events that fired while it was on.
	Holds []Hold `json:"holds,omitempty"`

	// Days is the in-progress UTC-day accumulator per check, written
	// so a restart at 23:00 does not lose the day. Flushed to the
	// JSONL history file at midnight.
	Days map[string]DayAcc `json:"days,omitempty"`

	// LastHeartbeat is when the still-alive message was last queued.
	// Durable so a restart cannot turn a weekly ping into one per boot,
	// and a week of downtime cannot queue seven of them.
	LastHeartbeat time.Time `json:"last_heartbeat,omitzero"`
	// ClosedSinceHeartbeat is how many confirmed recoveries have been
	// recorded since LastHeartbeat. Capped incident logs would under-count
	// a noisy week; this counter is the number the next ping should say.
	ClosedSinceHeartbeat int `json:"closed_since_heartbeat,omitempty"`
}

// Hold is one active mute. Until is when it lifts. Suppressed is the
// digest of events that were not delivered while it was on: they become
// one EventHeld rather than vanishing.
type Hold struct {
	ID         string    `json:"id"`
	Until      time.Time `json:"until"`
	Group      string    `json:"group,omitempty"`
	Check      string    `json:"check,omitempty"`
	Source     string    `json:"source"` // "adhoc" or "schedule"
	Created    time.Time `json:"created,omitzero"`
	Suppressed *Summary  `json:"suppressed,omitempty"`
}

const (
	HoldAdhoc    = "adhoc"
	HoldSchedule = "schedule"
)

// MaxDayDurations bounds the persisted sample of response times used
// for p50/p95. 1440 is a day of 60s probes; extra samples still count
// toward uptime, they just stop changing the percentiles.
const MaxDayDurations = 1440

// DayAcc is one check's stats for a single UTC date, accumulated as
// probes land so a restart cannot lose or duplicate the day.
type DayAcc struct {
	Date      string  `json:"date"`
	Samples   int     `json:"samples"`
	Up        int     `json:"up"`
	Incidents int     `json:"incidents"`
	Durations []int64 `json:"durations,omitempty"` // milliseconds
}

// RegistryCache is the weekly RDAP bootstrap plus any WHOIS servers we
// have already asked IANA for. A hard-coded TLD table would go stale
// silently; this is the live equivalent.
type RegistryCache struct {
	RDAPFetchedAt time.Time         `json:"rdap_fetched_at,omitzero"`
	RDAP          map[string]string `json:"rdap,omitempty"`  // tld → base URL
	WHOIS         map[string]string `json:"whois,omitempty"` // tld → host
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
