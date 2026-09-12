package monitor

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eeegoloauq/lookout/internal/state"
)

const pushConfig = `
checks:
  - name: ZFS tank
    group: Jobs
    type: push
    expect_every: 10m
    grace: 2m
    token: 0123456789abcdef
`

const pushToken = "0123456789abcdef"

// events collects what the monitor emitted. The monitor emits from the
// probe goroutine, so the slice needs a lock even in a synctest bubble.
type events struct {
	mu   sync.Mutex
	seen []state.Event
}

func (e *events) record(ev state.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, ev)
}

func (e *events) kinds() []state.EventKind {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]state.EventKind, len(e.seen))
	for i, ev := range e.seen {
		out[i] = ev.Kind
	}
	return out
}

func (e *events) reason(kind state.EventKind) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.seen {
		if ev.Kind == kind {
			return ev.Result.Reason()
		}
	}
	return ""
}

// A push check is evaluated on the scheduler's tick like every other check.
// That is the whole design: the event it exists for is the ping that never
// came, and nothing arrives to trigger it.
func TestPushCheckGoesDownOnTheTickAndBackUpOnAPing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t, pushConfig)
		p := newFakeProber()
		got := &events{}
		m := New(cfg, p, WithLogger(quietLogger()), WithEventFunc(got.record))

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- m.Run(ctx) }()
		synctest.Wait()

		m.Ping(pushToken, state.PingOK, "backup done", time.Now())
		time.Sleep(3 * time.Minute)
		if s := m.Machine().Status("ZFS tank"); s != state.StatusUp {
			t.Fatalf("status = %q after a good ping, want up", s)
		}

		// Nobody pings again. The deadline passes and the failure
		// threshold confirms it, without anything calling in.
		time.Sleep(15 * time.Minute)
		if s := m.Machine().Status("ZFS tank"); s != state.StatusDown {
			t.Fatalf("status = %q 15m into the silence, want down", s)
		}
		if r := got.reason(state.EventDown); !strings.HasPrefix(r, "no ping for") {
			t.Fatalf("down reason = %q, want it to say how long the silence has been", r)
		}

		m.Ping(pushToken, state.PingOK, "backup done late", time.Now())
		time.Sleep(3 * time.Minute)
		if s := m.Machine().Status("ZFS tank"); s != state.StatusUp {
			t.Fatalf("status = %q after the next ping, want up", s)
		}

		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}

		// The prober is for checks lookout performs. This one it waits for.
		if calls := p.times("ZFS tank"); len(calls) != 0 {
			t.Fatalf("the prober was called %d times for a push check", len(calls))
		}
		if kinds := got.kinds(); len(kinds) != 2 || kinds[0] != state.EventDown || kinds[1] != state.EventUp {
			t.Fatalf("events = %v, want one down then one up", kinds)
		}

		// The ping is durable, and a second process reads it back: a
		// restart must not invent an outage out of its own downtime.
		data, err := os.ReadFile(cfg.StateFile)
		if err != nil {
			t.Fatalf("state file: %v", err)
		}
		var snap state.Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			t.Fatalf("state file is not readable: %v", err)
		}
		if ping := snap.Pings["ZFS tank"]; ping.Msg != "backup done late" || ping.At.IsZero() {
			t.Fatalf("persisted ping = %+v, want the last one that arrived", ping)
		}

		restarted := New(cfg, newFakeProber(), WithLogger(quietLogger()))
		restarted.Restore()
		ping, ok := restarted.LastPing("ZFS tank")
		if !ok || ping.Msg != "backup done late" {
			t.Fatalf("after a restart the last ping is %+v, want the one on disk", ping)
		}
		if s := restarted.Machine().Status("ZFS tank"); s != state.StatusUp {
			t.Fatalf("after a restart status = %q, want the up it was left in", s)
		}
	})
}

// A ping with the wrong token changes nothing at all, including the state
// of the check whose token was nearly guessed.
func TestPingWithAnUnknownTokenIsIgnored(t *testing.T) {
	cfg := testConfig(t, pushConfig)
	m := New(cfg, newFakeProber(), WithLogger(quietLogger()))
	m.Restore()

	if m.Ping("0123456789abcdeF", state.PingOK, "", time.Now()) {
		t.Fatal("a ping with the wrong token was accepted")
	}
	if _, ok := m.LastPing("ZFS tank"); ok {
		t.Fatal("a ping with the wrong token was recorded")
	}
}
