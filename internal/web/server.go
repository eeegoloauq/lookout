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
	"net"
	"net/http"

	"github.com/eeegoloauq/lookout/internal/monitor"
)

// APIVersion is the status document version. It lives in the body, not
// the URL: a start page fetching GET /api/status can branch on it
// without us committing to /api/v1/ before the contract has users.
const APIVersion = 1

// New returns the HTTP handler for every outward endpoint.
func New(m *monitor.Monitor, version, source string) http.Handler {
	s := &server{mon: m, version: version, source: source}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	// Browsers request this on every visit; 204 is "we have no icon",
	// not a missing asset, so the console stays quiet.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/checks/{name}", s.checkHistory)
	// Silencing the monitor is the one thing an attacker on the network
	// would want to do to it, and the read-only surface is often bound to
	// a LAN address so a browser can reach the page. Reading is open to
	// whoever can reach the port; muting is not.
	mux.HandleFunc("POST /api/mute", localOnly(s.mute))
	mux.HandleFunc("POST /api/unmute", localOnly(s.unmute))
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /healthz", s.healthz)
	return mux
}

// localOnly rejects a request that did not come from the loopback
// interface. It deliberately does not trust X-Forwarded-For: behind a
// proxy that header is whatever the client typed.
func localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			writeJSON(w, http.StatusForbidden, MuteResponse{
				Error: "muting is only allowed from the host running lookout",
			})
			return
		}
		next(w, r)
	}
}

type server struct {
	mon     *monitor.Monitor
	version string
	// source is where this build can be read, or empty when the binary
	// carries no usable provenance. The footer links the version only
	// when it is set.
	source string
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
