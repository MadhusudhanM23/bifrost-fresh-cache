package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"freshness-companion/internal/decision"
	"freshness-companion/internal/openai"
	"freshness-companion/internal/policy"
)

// Headers of a verified answer.
const (
	// HeaderVerified is "human" on an answer served from the verified table,
	// or "stale_verified" when a verified answer exists for the question but
	// the source page contradicts it (it was not served; the request went on
	// to the model as usual).
	HeaderVerified      = "X-Freshness-Verified"
	HeaderVerifiedLabel = "X-Freshness-Verified-Label"
	// HeaderVerifiedCheck: confirmed | unable_to_verify | not_checked.
	HeaderVerifiedCheck = "X-Freshness-Verified-Check"
	// HeaderAnswerSource is "verified" when the companion answered by itself.
	HeaderAnswerSource = "X-Freshness-Answer-Source"
)

// DefaultVerifiedScopeHeader names the request header compared with an
// entry's "scope". It is Bifrost's virtual key header.
const DefaultVerifiedScopeHeader = "x-bf-vk"

// verifiedStats counts what the verified table did.
type verifiedStats struct {
	served, staleSkipped atomic.Int64
}

// tryVerified answers from the verified table when the last user message
// matches an entry. It returns true when it wrote the response. When the
// entry is contradicted by the source page it writes nothing but marks the
// response headers (stale_verified) and returns false, so the request takes
// the normal path.
func (s *Server) tryVerified(w http.ResponseWriter, r *http.Request, chat openai.ChatRequest) bool {
	if s.cfg.Verified == nil || chat.LastUserText == "" {
		return false
	}
	e, ok := s.cfg.Verified.Lookup(chat.LastUserText, r.Header.Get(s.scopeHeader))
	if !ok {
		return false
	}
	model := chat.Model
	created := s.cfg.Now().Unix()
	id := "chatcmpl-verified-" + e.ID
	check := "not_checked"
	// The answer may state a watched fact (a price). If the source page now
	// says something else, the curated answer is out of date: do not serve it.
	if eff, timeSensitive := s.cfg.Policy.Resolve(chat.LastUserText); timeSensitive && len(eff.Facts) > 0 {
		body, _ := json.Marshal(verifiedCompletion(id, created, model, e.Answer))
		check = s.checkVerified(r.Context(), eff, body, e.ID)
		if check == "stale" {
			s.verifiedStats.staleSkipped.Add(1)
			w.Header().Set(HeaderVerified, "stale_verified")
			if e.Label != "" {
				w.Header().Set(HeaderVerifiedLabel, headerValue(e.Label))
			}
			return false
		}
	}
	h := w.Header()
	h.Set(HeaderVerified, "human")
	if e.Label != "" {
		h.Set(HeaderVerifiedLabel, headerValue(e.Label))
	}
	h.Set(HeaderVerifiedCheck, check)
	h.Set(HeaderAnswerSource, "verified")
	s.verifiedStats.served.Add(1)
	if chat.Stream {
		writeVerifiedStream(w, id, created, model, e.Answer)
		return true
	}
	writeJSON(w, http.StatusOK, verifiedCompletion(id, created, model, e.Answer))
	return true
}

// checkVerified runs the existing new-answer check on the curated text and
// remembers whether the entry was found out of date.
func (s *Server) checkVerified(ctx context.Context, eff policy.Effective, body []byte, entryID string) string {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.UpstreamTimeout)
	defer cancel()
	d := s.cfg.Engine.CheckNewAnswer(ctx, eff, body)
	switch d.Outcome {
	case decision.Contradicted:
		s.staleVerified.Store(entryID, s.cfg.Now())
		return "stale"
	case decision.Confirmed:
		s.staleVerified.Delete(entryID)
		return "confirmed"
	}
	return "unable_to_verify"
}

func verifiedCompletion(id string, created int64, model, answer string) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": answer},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
}

// writeVerifiedStream sends the answer as a short server-sent-events stream
// in OpenAI's chunk format: one chunk with the whole text, a closing chunk
// and [DONE].
func writeVerifiedStream(w http.ResponseWriter, id string, created int64, model, answer string) {
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for _, c := range []map[string]any{
		chunk(map[string]any{"role": "assistant", "content": answer}, nil),
		chunk(map[string]any{}, "stop"),
	} {
		b, _ := json.Marshal(c)
		_, _ = w.Write(append(append([]byte("data: "), b...), '\n', '\n'))
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	_ = rc.Flush()
}

type verifiedListEntry struct {
	ID     string `json:"id"` // a hash of the question and scope, not the question
	Label  string `json:"label,omitempty"`
	Scoped bool   `json:"scoped"`
	// Stale: the source page contradicted this answer the last time it was
	// checked, so it is not being served.
	Stale bool `json:"stale"`
}

// handleVerified lists the table without questions or answers: ids (hashes)
// and labels only.
func (s *Server) handleVerified(w http.ResponseWriter, _ *http.Request) {
	out := struct {
		Enabled       bool                `json:"enabled"`
		Count         int                 `json:"count"`
		ScopeHeader   string              `json:"scope_header,omitempty"`
		LoadedAt      *time.Time          `json:"loaded_at,omitempty"`
		LastError     string              `json:"last_reload_error,omitempty"`
		Served        int64               `json:"served"`
		StaleSkipped  int64               `json:"stale_skipped"`
		Entries       []verifiedListEntry `json:"entries"`
		MatchingNotes string              `json:"matching,omitempty"`
	}{Entries: []verifiedListEntry{}}
	if s.cfg.Verified != nil {
		loaded, lastErr := s.cfg.Verified.Info()
		out.Enabled, out.ScopeHeader, out.LastError = true, s.scopeHeader, lastErr
		out.LoadedAt = &loaded
		out.MatchingNotes = "exact match after lower-casing and dropping punctuation; no similarity matching"
		for _, e := range s.cfg.Verified.Set().Entries() {
			_, stale := s.staleVerified.Load(e.ID)
			out.Entries = append(out.Entries, verifiedListEntry{ID: e.ID, Label: e.Label, Scoped: e.Scope != "", Stale: stale})
		}
		out.Count = len(out.Entries)
	}
	out.Served, out.StaleSkipped = s.verifiedStats.served.Load(), s.verifiedStats.staleSkipped.Load()
	writeJSON(w, http.StatusOK, out)
}
