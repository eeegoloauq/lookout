package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// MuteRequest is POST /api/mute. `for` is a Go duration ("30m").
type MuteRequest struct {
	For   string `json:"for"`
	Group string `json:"group,omitempty"`
	Check string `json:"check,omitempty"`
}

// UnmuteRequest is POST /api/unmute. Empty group and check lift every
// ad-hoc mute. Scheduled windows are not affected.
type UnmuteRequest struct {
	Group string `json:"group,omitempty"`
	Check string `json:"check,omitempty"`
}

// MuteResponse is what mute/unmute return.
type MuteResponse struct {
	OK     bool       `json:"ok"`
	Until  *time.Time `json:"until,omitempty"`
	Group  string     `json:"group,omitempty"`
	Check  string     `json:"check,omitempty"`
	Cleared int       `json:"cleared,omitempty"`
	Error  string     `json:"error,omitempty"`
}

func (s *server) mute(w http.ResponseWriter, r *http.Request) {
	var req MuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, MuteResponse{Error: "body is not valid JSON"})
		return
	}
	d, err := time.ParseDuration(req.For)
	if err != nil || d <= 0 {
		writeJSON(w, http.StatusBadRequest, MuteResponse{Error: `for must be a positive duration such as "30m"`})
		return
	}
	h, err := s.mon.Mute(d, req.Group, req.Check)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, MuteResponse{Error: err.Error()})
		return
	}
	until := h.Until.UTC()
	writeJSON(w, http.StatusOK, MuteResponse{OK: true, Until: &until, Group: h.Group, Check: h.Check})
}

func (s *server) unmute(w http.ResponseWriter, r *http.Request) {
	var req UnmuteRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, MuteResponse{Error: "body is not valid JSON"})
			return
		}
	}
	n := s.mon.Unmute(req.Group, req.Check)
	writeJSON(w, http.StatusOK, MuteResponse{OK: true, Group: req.Group, Check: req.Check, Cleared: n})
}
