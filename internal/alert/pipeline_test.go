package alert

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/state"
)

type fakeNotifier struct {
	mu       sync.Mutex
	err      error
	messages []string
}

func (f *fakeNotifier) Notify(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.err != nil {
		return f.err
	}
	f.messages = append(f.messages, text)
	return nil
}

func (f *fakeNotifier) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeNotifier) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.messages...)
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ev(kind state.EventKind, name, group string) state.Event {
	return state.Event{
		Kind:  kind,
		Check: name,
		Group: group,
		At:    time.Now(),
		Alert: true,
		Result: check.Result{
			Name:       name,
			At:         time.Now(),
			Outcome:    check.OutcomeDown,
			StatusCode: 503,
			Duration:   100 * time.Millisecond,
			Failures:   []check.Failure{{Condition: "status", Want: "200-299", Got: "503"}},
		},
	}
}

func start(t *testing.T, p *Pipeline) (cancel func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	synctest.Wait()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

// A dead channel must not drop an alert, and a restart must still have
// every event that was queued (SPEC §6, §13).
func TestDeadChannelLosesNoAlertAndSurvivesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNotifier{err: errors.New("telegram unreachable")}
		p := NewPipeline(n, 45*time.Second, quiet())
		start(t, p)

		names := []string{"Router", "Photos", "API"}
		for _, name := range names {
			p.Enqueue(ev(state.EventDown, name, "Services"))
		}
		synctest.Wait()

		time.Sleep(45 * time.Second)
		synctest.Wait()
		if got := n.got(); len(got) != 0 {
			t.Fatalf("delivered while the channel was down: %v", got)
		}
		if pending := p.Items(); len(pending) != 3 {
			t.Fatalf("pending = %d, want 3", len(pending))
		}

		// The queue is a value: persist it, throw the process away, restore.
		snap := p.Snapshot()
		if snap.Attempts < 1 {
			t.Fatal("a failed send must record an attempt so backoff survives restart")
		}

		n2 := &fakeNotifier{err: errors.New("still down")}
		restarted := NewPipeline(n2, 45*time.Second, quiet())
		restarted.Restore(snap)
		start(t, restarted)

		if pending := restarted.Items(); len(pending) != 3 {
			t.Fatalf("after restart pending = %d, want 3", len(pending))
		}
		seen := map[string]bool{}
		for _, it := range restarted.Items() {
			seen[it.Event.Check] = true
		}
		for _, name := range names {
			if !seen[name] {
				t.Errorf("restart lost %q", name)
			}
		}

		// Channel recovers. Backoff from the first failure is 1s; wait it
		// out. Everything that was queued leaves in one message.
		n2.setErr(nil)
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		got := n2.got()
		if len(got) != 1 {
			t.Fatalf("messages after recovery = %d, want 1 (one batch): %v", len(got), got)
		}
		for _, name := range names {
			if !strings.Contains(got[0], name) {
				t.Errorf("recovered batch missing %q:\n%s", name, got[0])
			}
		}
		if pending := restarted.Items(); len(pending) != 0 {
			t.Fatalf("outbox still holds %d items after a successful send", len(pending))
		}

		// The same events must not be sent again.
		time.Sleep(time.Minute)
		if len(n2.got()) != 1 {
			t.Fatalf("events were sent more than once: %v", n2.got())
		}
	})
}

func TestBatchOfNEventsIsOneMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNotifier{}
		p := NewPipeline(n, 45*time.Second, quiet())
		start(t, p)

		for _, name := range []string{"A", "B", "C", "D", "E"} {
			p.Enqueue(ev(state.EventDown, name, "Services"))
		}
		synctest.Wait()
		if got := n.got(); len(got) != 0 {
			t.Fatalf("sent before the batch window closed: %v", got)
		}

		time.Sleep(44 * time.Second)
		synctest.Wait()
		if got := n.got(); len(got) != 0 {
			t.Fatalf("sent %d ms early: %v", 1000, got)
		}

		time.Sleep(time.Second)
		synctest.Wait()
		got := n.got()
		if len(got) != 1 {
			t.Fatalf("got %d messages, want 1:\n%v", len(got), got)
		}
		for _, name := range []string{"A", "B", "C", "D", "E"} {
			if !strings.Contains(got[0], name) {
				t.Errorf("batch missing %q:\n%s", name, got[0])
			}
		}
		if !strings.Contains(got[0], "5 checks") {
			t.Errorf("batch header missing:\n%s", got[0])
		}
	})
}

func TestRecoveriesBatchTheSameWay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNotifier{}
		p := NewPipeline(n, 45*time.Second, quiet())
		start(t, p)
		p.Enqueue(ev(state.EventUp, "A", "Core"))
		p.Enqueue(ev(state.EventUp, "B", "Core"))
		synctest.Wait()
		time.Sleep(45 * time.Second)
		synctest.Wait()
		got := n.got()
		if len(got) != 1 {
			t.Fatalf("got %d messages, want 1", len(got))
		}
		if !strings.Contains(got[0], "UP A") || !strings.Contains(got[0], "UP B") {
			t.Errorf("recovery batch:\n%s", got[0])
		}
	})
}

func TestAlertFalseNeverEntersTheOutbox(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNotifier{}
		p := NewPipeline(n, time.Second, quiet())
		start(t, p)
		e := ev(state.EventDown, "Noisy", "Lab")
		e.Alert = false
		p.Enqueue(e)
		time.Sleep(2 * time.Second)
		if len(p.Items()) != 0 {
			t.Fatal("alert:false was queued")
		}
		if len(n.got()) != 0 {
			t.Fatal("alert:false was delivered")
		}
	})
}

func TestSuccessfulSendIsNotRepeated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNotifier{}
		p := NewPipeline(n, 45*time.Second, quiet())
		start(t, p)
		p.Enqueue(ev(state.EventDown, "A", "Core"))
		synctest.Wait()
		time.Sleep(45 * time.Second)
		synctest.Wait()
		if len(n.got()) != 1 {
			t.Fatalf("first send = %d messages", len(n.got()))
		}
		p.Enqueue(ev(state.EventDown, "B", "Core"))
		synctest.Wait()
		time.Sleep(45 * time.Second)
		synctest.Wait()
		got := n.got()
		if len(got) != 2 {
			t.Fatalf("got %d messages, want 2", len(got))
		}
		if strings.Contains(got[1], "DOWN A") {
			t.Errorf("already-delivered A was sent again:\n%s", got[1])
		}
		if !strings.Contains(got[1], "DOWN B") {
			t.Errorf("second message missing B:\n%s", got[1])
		}
	})
}

func TestBackoffGrowsThenCaps(t *testing.T) {
	if got := backoff(1); got != time.Second {
		t.Errorf("attempt 1 = %s, want 1s", got)
	}
	if got := backoff(2); got != 2*time.Second {
		t.Errorf("attempt 2 = %s, want 2s", got)
	}
	if got := backoff(3); got != 4*time.Second {
		t.Errorf("attempt 3 = %s, want 4s", got)
	}
	if got := backoff(20); got != backoffCap {
		t.Errorf("attempt 20 = %s, want the cap %s", got, backoffCap)
	}
}

func TestOutboxOnDiskSurvivesANewPipeline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		store := state.NewStore(dir + "/state.json")

		n := &fakeNotifier{err: errors.New("down")}
		p := NewPipeline(n, 45*time.Second, quiet())
		p.SetPersist(func() {
			if err := store.Save(state.Snapshot{Checks: map[string]state.CheckState{}, Outbox: p.Snapshot()}); err != nil {
				t.Errorf("save: %v", err)
			}
		})
		start(t, p)
		p.Enqueue(ev(state.EventDown, "Photos", "Services"))
		if err := store.Save(state.Snapshot{Checks: map[string]state.CheckState{}, Outbox: p.Snapshot()}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(45 * time.Second)

		loaded, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(loaded.Outbox.Items) != 1 {
			t.Fatalf("state file outbox = %d items, want 1", len(loaded.Outbox.Items))
		}

		n2 := &fakeNotifier{}
		p2 := NewPipeline(n2, 45*time.Second, quiet())
		p2.Restore(loaded.Outbox)
		start(t, p2)
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		got := n2.got()
		if len(got) != 1 || !strings.Contains(got[0], "Photos") {
			t.Fatalf("after loading the file: %v", got)
		}
	})
}
