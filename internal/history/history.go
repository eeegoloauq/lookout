// Package history keeps the recent result history of every check in memory:
// a fixed ring per check, holding the last 24 hours.
//
// The ring is the source of truth while lookout is running. A JSONL seed
// file is replayed into the rings on start so a restart does not blank the
// 24-hour bar; it is not a database. Anything a restart must not lose
// (incident state, the outbox) lives in the durable state instead.
package history

import (
	"sync"
	"time"

	"github.com/eeegoloauq/lookout/internal/check"
)

// Retention is how far back the in-memory history reaches.
const Retention = 24 * time.Hour

// MaxPoints caps a single ring, so a check configured with a very short
// interval cannot grow the process without bound.
const MaxPoints = 1440

// Point is one recorded probe result.
type Point struct {
	At         time.Time
	Outcome    check.Outcome
	Duration   time.Duration
	StatusCode int
}

// CapacityFor returns the ring size that covers Retention at this interval,
// which is 1440 points at the default 60s.
func CapacityFor(interval time.Duration) int {
	if interval <= 0 {
		return MaxPoints
	}
	n := int((Retention + interval - 1) / interval)
	return min(max(n, 1), MaxPoints)
}

// Ring is a fixed-size circular buffer of points, oldest first. It is safe for
// concurrent use.
type Ring struct {
	mu   sync.Mutex
	buf  []Point
	head int // index of the oldest point
	n    int
}

// NewRing returns a ring holding at most capacity points.
func NewRing(capacity int) *Ring {
	return &Ring{buf: make([]Point, min(max(capacity, 1), MaxPoints))}
}

// Add appends a point, dropping the oldest when the ring is full and any point
// that has aged out of the retention window.
func (r *Ring) Add(p Point) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == len(r.buf) {
		r.buf[r.head] = p
		r.head = (r.head + 1) % len(r.buf)
	} else {
		r.buf[(r.head+r.n)%len(r.buf)] = p
		r.n++
	}
	for r.n > 0 && p.At.Sub(r.at(0).At) > Retention {
		r.head = (r.head + 1) % len(r.buf)
		r.n--
	}
}

// Len returns how many points are held.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// Points returns the held points, oldest first.
func (r *Ring) Points() []Point {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Point, r.n)
	for i := range r.n {
		out[i] = r.at(i)
	}
	return out
}

// Last returns the most recent point.
func (r *Ring) Last() (Point, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return Point{}, false
	}
	return r.at(r.n - 1), true
}

// Uptime returns the share of successful probes at or after since, and how many
// probes that share is computed from. A window with no samples reports 0
// samples: the caller must render that as "no data", never as 100%.
func (r *Ring) Uptime(since time.Time) (ratio float64, samples int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	up := 0
	for i := range r.n {
		p := r.at(i)
		if p.At.Before(since) {
			continue
		}
		if p.Outcome == check.OutcomeUnknown {
			// A missed lookup is not a sample of availability.
			continue
		}
		samples++
		if p.Outcome.Succeeded() {
			up++
		}
	}
	if samples == 0 {
		return 0, 0
	}
	return float64(up) / float64(samples), samples
}

// at returns the i-th oldest point. The caller holds the lock.
func (r *Ring) at(i int) Point {
	return r.buf[(r.head+i)%len(r.buf)]
}

// History is one ring per check.
type History struct {
	mu    sync.Mutex
	rings map[string]*Ring
}

// New returns an empty history.
func New() *History {
	return &History{rings: map[string]*Ring{}}
}

// Track creates the ring for a check, sized for its interval.
func (h *History) Track(name string, interval time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rings[name] = NewRing(CapacityFor(interval))
}

// Record stores a result. Results for checks that were never tracked are
// dropped rather than silently sized wrong.
func (h *History) Record(res check.Result) {
	h.mu.Lock()
	ring := h.rings[res.Name]
	h.mu.Unlock()
	if ring == nil {
		return
	}
	ring.Add(Point{
		At:         res.At,
		Outcome:    res.Outcome,
		Duration:   res.Duration,
		StatusCode: res.StatusCode,
	})
}

// Ring returns the ring of a check.
func (h *History) Ring(name string) (*Ring, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rings[name]
	return r, ok
}
