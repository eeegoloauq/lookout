package history

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
)

// A crash mid-write leaves a half line. Refusing to start over that is
// how a monitor goes silent, which is the failure lookout exists to
// prevent.
func TestSamplesFileSkipsATruncatedLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	now := time.Now().UTC().Truncate(time.Second)
	good, err := json.Marshal(sampleLine{T: now.Unix(), Check: "Photos", Outcome: check.OutcomeUp, MS: 40, Status: 200})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(good, '\n'), []byte(`{"t":1,"c":"Pho`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSamples(path)
	recs, err := s.Load(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Name != "Photos" {
		t.Fatalf("load = %+v, truncated line must not become a sample", recs)
	}
}

// Samples older than the ring's window cannot seed it, and leaving them
// on disk is how a forgotten file becomes a disk-full outage of its own.
func TestSamplesFileDropsPointsOlderThanRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	now := time.Now().UTC().Truncate(time.Second)
	old := sampleLine{T: now.Add(-Retention - time.Hour).Unix(), Check: "Photos", Outcome: check.OutcomeDown, MS: 5, Status: 500}
	fresh := sampleLine{T: now.Unix(), Check: "Photos", Outcome: check.OutcomeUp, MS: 40, Status: 200}
	var buf bytes.Buffer
	for _, rec := range []sampleLine{old, fresh} {
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSamples(path)
	recs, err := s.Load(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Outcome != check.OutcomeUp {
		t.Fatalf("load = %+v, want only the in-window sample", recs)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte(`"o":"down"`)) {
		t.Fatalf("file still holds the aged-out sample:\n%s", onDisk)
	}
	if !bytes.Contains(onDisk, []byte(`"o":"up"`)) {
		t.Fatalf("file lost the in-window sample:\n%s", onDisk)
	}
}

// A 1s interval, or a check that never stops writing, must not be able
// to fill the disk waiting for the next midnight prune.
func TestSamplesFileDoesNotGrowWithoutBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	s := NewSamples(path)
	s.maxLines = 10
	now := time.Now().UTC()
	for i := range 50 {
		err := s.Append(check.Result{
			Name:       "Photos",
			At:         now.Add(time.Duration(i) * time.Second),
			Outcome:    check.OutcomeUp,
			Duration:   40 * time.Millisecond,
			StatusCode: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	n := countLines(t, path)
	if n == 0 || n > 10 {
		t.Fatalf("file has %d lines, want between 1 and the cap of 10", n)
	}
}

// fsync-per-line is how a 60s check becomes a disk-bound one; the
// buffer is the contract that a hard reset may lose the last few
// seconds of the graph and nothing else.
func TestAppendedSamplesStayBufferedUntilFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	s := NewSamples(path)
	res := check.Result{Name: "Photos", At: time.Now(), Outcome: check.OutcomeUp, Duration: 40 * time.Millisecond, StatusCode: 200}
	if err := s.Append(res); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte("Photos")) {
		t.Fatal("a single sample was written through without a flush")
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	onDisk, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(onDisk, []byte("Photos")) {
		t.Fatalf("flush did not write the sample:\n%s", onDisk)
	}
}

// The ring drops the oldest relative to the newest add; feeding it
// newest-first would wipe the window we just restored.
func TestLoadReplaysInChronologicalOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	now := time.Now().UTC().Truncate(time.Second)
	second := sampleLine{T: now.Unix(), Check: "Photos", Outcome: check.OutcomeDown, MS: 80, Status: 500}
	first := sampleLine{T: now.Add(-time.Minute).Unix(), Check: "Photos", Outcome: check.OutcomeUp, MS: 40, Status: 200}
	var buf bytes.Buffer
	for _, rec := range []sampleLine{second, first} {
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := NewSamples(path).Load(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Outcome != check.OutcomeUp || recs[1].Outcome != check.OutcomeDown {
		t.Fatalf("order = %+v, want oldest first", recs)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}
