package state

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
)

func downEvent(name, group string, at time.Time) Event {
	return Event{
		Kind:  EventDown,
		Check: name,
		Group: group,
		At:    at,
		Alert: true,
		Result: check.Result{
			Name:       name,
			At:         at,
			Outcome:    check.OutcomeDown,
			StatusCode: 503,
			Duration:   100 * time.Millisecond,
			Failures:   []check.Failure{{Condition: "status", Want: "200-299", Got: "503"}},
			BodySample: "backend unavailable",
		},
	}
}

func TestOutboxEnqueuePreservesOrderAndIDs(t *testing.T) {
	var o Outbox
	o.SetLimit(32)
	t0 := epoch
	o.Enqueue(downEvent("A", "Core", t0), t0)
	o.Enqueue(downEvent("B", "Services", t0.Add(time.Second)), t0.Add(time.Second))
	if len(o.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(o.Items))
	}
	if o.Items[0].Event.Check != "A" || o.Items[1].Event.Check != "B" {
		t.Fatalf("order = %q then %q, want A then B", o.Items[0].Event.Check, o.Items[1].Event.Check)
	}
	if o.Items[0].ID == o.Items[1].ID {
		t.Fatal("two items share an id")
	}
}

func TestOutboxRemoveByIDLeavesTheOthers(t *testing.T) {
	var o Outbox
	o.SetLimit(32)
	o.Enqueue(downEvent("A", "Core", epoch), epoch)
	o.Enqueue(downEvent("B", "Core", epoch), epoch)
	o.Enqueue(downEvent("C", "Core", epoch), epoch)
	o.Remove([]int64{o.Items[1].ID})
	if len(o.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(o.Items))
	}
	if o.Items[0].Event.Check != "A" || o.Items[1].Event.Check != "C" {
		t.Errorf("remaining = %q, %q, want A, C", o.Items[0].Event.Check, o.Items[1].Event.Check)
	}
}

// Overflow must not drop events: they fold into a summary whose counts
// still add up to what was enqueued.
func TestOutboxOverflowCollapsesIntoASummary(t *testing.T) {
	var o Outbox
	o.SetLimit(4)
	for i := range 10 {
		name := string(rune('A' + i))
		at := epoch.Add(time.Duration(i) * time.Second)
		o.Enqueue(downEvent(name, "Core", at), at)
	}
	if len(o.Items) > o.Limit() {
		t.Fatalf("queue holds %d items, over the limit of %d", len(o.Items), o.Limit())
	}
	total := 0
	var sawSummary bool
	for _, it := range o.Items {
		if it.Event.Kind == EventSummary {
			sawSummary = true
			if it.Event.Summary == nil {
				t.Fatal("summary event has no payload")
			}
			total += it.Event.Summary.Count
			continue
		}
		total++
	}
	if !sawSummary {
		t.Fatal("overflow did not produce a summary")
	}
	if total != 10 {
		t.Errorf("accounted for %d events, want 10: collapse dropped some", total)
	}
}

func TestOutboxCollapseMergesAnExistingSummary(t *testing.T) {
	var o Outbox
	o.SetLimit(4)
	for i := range 20 {
		at := epoch.Add(time.Duration(i) * time.Second)
		o.Enqueue(downEvent("X", "Core", at), at)
	}
	summaries := 0
	total := 0
	for _, it := range o.Items {
		if it.Event.Kind == EventSummary {
			summaries++
			total += it.Event.Summary.Count
			continue
		}
		total++
	}
	if summaries != 1 {
		t.Errorf("summaries = %d, want 1: collapse should fold summaries together", summaries)
	}
	if total != 20 {
		t.Errorf("accounted for %d events, want 20", total)
	}
}

func TestOutboxJSONRoundTrip(t *testing.T) {
	var o Outbox
	o.SetLimit(32)
	ev := downEvent("Photos", "Services", epoch)
	ev.Result.BodySample = "Authorization: Bearer s3cret"
	o.Enqueue(ev, epoch)
	o.Attempts = 3
	o.NextTry = epoch.Add(time.Minute)

	data, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var got Outbox
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.NextID != o.NextID || got.Attempts != 3 || !got.NextTry.Equal(o.NextTry) {
		t.Errorf("header = %+v, want next_id %d attempts 3", got, o.NextID)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	if got.Items[0].Event.Check != "Photos" || got.Items[0].Event.Result.StatusCode != 503 {
		t.Errorf("event did not round-trip: %+v", got.Items[0].Event)
	}
	if !reflect.DeepEqual(got.Items[0].Event.Result.Failures, ev.Result.Failures) {
		t.Errorf("failures = %+v", got.Items[0].Event.Result.Failures)
	}
}

func TestStorePersistsTheOutbox(t *testing.T) {
	s := tempStore(t)
	var o Outbox
	o.Enqueue(downEvent("Photos", "Services", epoch), epoch)
	if err := s.Save(Snapshot{Checks: map[string]CheckState{}, Outbox: o}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Outbox.Items) != 1 || got.Outbox.Items[0].Event.Check != "Photos" {
		t.Errorf("outbox = %+v, want the queued Photos event", got.Outbox)
	}
}

func TestCloneDoesNotAliasSummaryMaps(t *testing.T) {
	var o Outbox
	o.SetLimit(2)
	for i := range 4 {
		at := epoch.Add(time.Duration(i) * time.Second)
		o.Enqueue(downEvent("A", "Core", at), at)
	}
	cloned := o.Clone()
	for _, it := range o.Items {
		if it.Event.Summary != nil {
			it.Event.Summary.Count = -1
			it.Event.Summary.ByKind["down"] = -1
		}
	}
	for _, it := range cloned.Items {
		if it.Event.Summary != nil && (it.Event.Summary.Count < 0 || it.Event.Summary.ByKind["down"] < 0) {
			t.Fatal("clone shares summary maps with the original")
		}
	}
}
