package web

import (
	"fmt"
	"net/http"
	"time"
)

// degradedAfterAttempts is how many failed deliveries make /healthz
// report degraded. One failure is a blip on a flaky proxy; several
// means the channel is not delivering, and a monitor that cannot call
// for help must look sick from the outside (research O3 / O20).
const degradedAfterAttempts = 3

// HealthDocument is GET /healthz.
type HealthDocument struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	doc, code := s.health(time.Now())
	writeJSON(w, code, doc)
}

func (s *server) health(now time.Time) (HealthDocument, int) {
	box := s.mon.Outbox()
	if len(box.Items) == 0 || box.Attempts < degradedAfterAttempts {
		return HealthDocument{Status: "ok"}, http.StatusOK
	}
	oldest := box.Items[0].Enqueued
	age := now.Sub(oldest)
	if age < 0 {
		age = 0
	}
	reason := fmt.Sprintf("alert delivery has failed %d times; %s still queued (oldest %s)",
		box.Attempts, plural(len(box.Items), "event"), age.Round(time.Second))
	return HealthDocument{Status: "degraded", Reason: reason}, http.StatusServiceUnavailable
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
