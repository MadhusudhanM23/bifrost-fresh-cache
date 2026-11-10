package watch

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freshness-companion/internal/evidence"
)

// Status is the body of GET /freshness/watch.
type Status struct {
	Adaptive     bool           `json:"adaptive"`
	FloorSeconds float64        `json:"floor_seconds"`
	MaxSeconds   float64        `json:"max_seconds"`
	Growth       float64        `json:"growth"`
	Watcher      Stats          `json:"watcher"`
	Evidence     evidence.Stats `json:"evidence"`
	Sources      []SourceState  `json:"sources"`
}

// Server serves the watch endpoints. Every field may be left at its zero
// value: with no Schedule the status says adaptive false and lists no
// sources; with no Watcher the check endpoint answers 409.
type Server struct {
	Schedule *Schedule
	Watcher  *Watcher
	// Stats returns the evidence cache counters (nil = zeros).
	Stats func() evidence.Stats
	// Token, when set, must be sent as "Authorization: Bearer <token>" to
	// POST a check (the FRESHNESS_NOTICE_TOKEN rule).
	Token string
	// Idle and MaxPerHour are shown when there is no Watcher.
	Idle       time.Duration
	MaxPerHour int
	Now        func() time.Time
}

// Register adds GET /freshness/watch and POST /freshness/watch/check to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /freshness/watch", s.handleStatus)
	mux.HandleFunc("POST /freshness/watch/check", s.handleCheck)
}

// Status builds the GET body.
func (s *Server) Status() Status {
	st := Status{Sources: []SourceState{}, Watcher: Stats{MaxPerHour: s.MaxPerHour, IdleAfterSeconds: s.Idle.Seconds()}}
	if s.Schedule != nil {
		cfg := s.Schedule.Config()
		st.Adaptive, st.FloorSeconds, st.MaxSeconds, st.Growth = cfg.Adaptive, cfg.Floor.Seconds(), cfg.Max.Seconds(), cfg.Growth
		if !cfg.Adaptive {
			st.Growth = 1
		}
	}
	if s.Stats != nil {
		st.Evidence = s.Stats()
	}
	switch {
	case s.Watcher != nil:
		st.Watcher = s.Watcher.Stats()
		st.Sources = s.Watcher.Sources()
	case s.Schedule != nil:
		now := time.Now()
		if s.Now != nil {
			now = s.Now()
		}
		st.Sources = s.Schedule.Sources()
		for i := range st.Sources {
			src := &st.Sources[i]
			src.Active = src.LastUsedAt != nil && (s.Idle <= 0 || now.Sub(*src.LastUsedAt) <= s.Idle)
		}
	}
	return st
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Status())
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "a check needs the bearer token", "unauthorized")
		return
	}
	var in struct {
		SourceURL string `json:"source_url"`
		Fact      string `json:"fact"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON like {\"source_url\": \"...\"} or {\"fact\": \"...\"}", "invalid_request_error")
		return
	}
	in.SourceURL, in.Fact = strings.TrimSpace(in.SourceURL), strings.TrimSpace(in.Fact)
	if (in.SourceURL == "") == (in.Fact == "") {
		writeError(w, http.StatusBadRequest, "send exactly one of source_url or fact", "invalid_request_error")
		return
	}
	if s.Watcher == nil || !s.Watcher.Enabled() {
		writeError(w, http.StatusConflict, ErrWatcherOff.Error(), "watcher_off")
		return
	}
	target := in.SourceURL
	if target == "" {
		target = in.Fact
	}
	st, err := s.Watcher.CheckNow(r.Context(), target)
	switch {
	case errors.Is(err, ErrUnknownSource):
		writeError(w, http.StatusNotFound, "no watched fact matches that source_url or fact", "not_found")
	case errors.Is(err, ErrOverBudget):
		writeError(w, http.StatusTooManyRequests, err.Error(), "over_budget")
	case errors.Is(err, ErrWatcherOff):
		writeError(w, http.StatusConflict, err.Error(), "watcher_off")
	case err != nil:
		writeJSON(w, http.StatusBadGateway, struct {
			SourceState
			Error string `json:"error"`
		}{st, err.Error()})
	default:
		writeJSON(w, http.StatusOK, st)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeError writes an OpenAI-style error body, like the proxy does.
func writeError(w http.ResponseWriter, status int, message, kind string) {
	type apiError struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}
	writeJSON(w, status, struct {
		Error apiError `json:"error"`
	}{Error: apiError{Message: message, Type: kind, Code: kind}})
}
