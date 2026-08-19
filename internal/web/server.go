// Package web is lookout's outward face: the JSON status API, the
// Prometheus scrape, the status page and the liveness probe.
//
// The JSON documents are a public contract consumed by other projects,
// so they are shaped for a foreign reader rather than as a dump of
// internal types. They are versioned; bump APIVersion when the shape
// changes in a way a consumer would notice.
package web

import (
	"encoding/json"
	"net/http"

	"github.com/eeegoloauq/lookout/internal/monitor"
)

// APIVersion is the status document version. It lives in the body, not
// the URL: a start page fetching GET /api/status can branch on it
// without us committing to /api/v1/ before the contract has users.
const APIVersion = 1

// New returns the HTTP handler for every outward endpoint.
func New(m *monitor.Monitor, version string) http.Handler {
	s := &server{mon: m, version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	// Browsers request this on every visit; 204 is "we have no icon",
	// not a missing asset, so the console stays quiet.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/checks/{name}", s.checkHistory)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /healthz", s.healthz)
	return mux
}

type server struct {
	mon     *monitor.Monitor
	version string
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
