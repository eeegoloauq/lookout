package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/eeegoloauq/lookout/internal/state"
)

// pushMaxBody is what the endpoint will read from a ping. A heartbeat is a
// URL and at most a short line of text; anything larger is not a cron job,
// and an endpoint anyone on the network can reach must say where it stops.
const pushMaxBody = 4 << 10

// push records one heartbeat: POST (or GET) /api/push/{token}.
//
// Unlike mute it is not loopback-only. The whole point is that the machine
// doing the work is somewhere else, and the token is what stands in for
// knowing who is calling.
func (s *server) push(w http.ResponseWriter, r *http.Request) {
	// no-store matters more here than anywhere else on this server: a cache
	// in front of lookout that answered a ping out of its own copy would
	// leave the deadline running against a heartbeat lookout never saw.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	r.Body = http.MaxBytesReader(w, r.Body, pushMaxBody)
	if err := r.ParseForm(); err != nil {
		// A ping that could not be read is not a ping. Falling through to
		// the default would record an oversized `status=fail` as a healthy
		// heartbeat and push the deadline out — the failing job would read
		// as the fine one.
		code := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "ping could not be read", code)
		return
	}

	status := strings.ToLower(strings.TrimSpace(r.Form.Get("status")))
	switch status {
	case "":
		status = state.PingOK
	case state.PingOK, state.PingFail:
	default:
		// A word nobody recognises must not be read as "fine": that is how
		// a job reports success for a year while failing every night.
		http.Error(w, "status must be ok or fail", http.StatusBadRequest)
		return
	}
	if !s.mon.Ping(r.PathValue("token"), status, r.Form.Get("msg"), time.Now()) {
		// The same answer a wrong path gets, with nothing said about which
		// half of the guess was right.
		http.NotFound(w, r)
		return
	}
	// Nothing to say back. A cron line pipes its output to mail, and a body
	// would be a nightly message saying everything is normal.
	w.WriteHeader(http.StatusNoContent)
}
