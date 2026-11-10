package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"freshness-companion/internal/auditlog"
	"freshness-companion/internal/changes"
	"freshness-companion/internal/policy"
)

// invalidator is implemented by the evidence cache.
type invalidator interface{ Invalidate(sourceURL string) bool }

const maxClearKeys = 10

type changeRequest struct {
	Fact      string `json:"fact"`
	Category  string `json:"category"`
	SourceURL string `json:"source_url"`
	Note      string `json:"note"`
	// ClearCacheKeys also deletes every Bifrost cache entry under these keys
	// right away, instead of letting each entry be refreshed on its next hit.
	ClearCacheKeys []string `json:"clear_cache_keys"`
}

func (s *Server) authorizedNotice(r *http.Request) bool {
	if s.cfg.ChangeToken == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.ChangeToken)) == 1
}

// handleChangeNotice accepts "this changed" from the system that owns a
// fact. It drops the cached evidence of the facts it covers and remembers
// the notice, so every entry written before it is checked (or, with no
// watched fact to check it against, refreshed) on its next hit.
func (s *Server) handleChangeNotice(w http.ResponseWriter, r *http.Request) {
	if !s.authorizedNotice(r) {
		writeError(w, http.StatusUnauthorized, "a change notice needs the bearer token", "unauthorized")
		return
	}
	var in changeRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON like {\"fact\": \"...\"}: "+oneLine(err.Error(), 150), "invalid_request_error")
		return
	}
	selectors := 0
	for _, v := range []string{in.Fact, in.Category, in.SourceURL} {
		if strings.TrimSpace(v) != "" {
			selectors++
		}
	}
	if selectors != 1 || len(in.ClearCacheKeys) > maxClearKeys {
		writeError(w, http.StatusBadRequest, "send exactly one of fact, category or source_url (and at most "+strconv.Itoa(maxClearKeys)+" clear_cache_keys)", "invalid_request_error")
		return
	}

	n := changes.Notice{Note: oneLine(in.Note, 300)}
	var facts []*policy.Fact
	switch {
	case strings.TrimSpace(in.Fact) != "":
		f := s.cfg.Policy.Fact(in.Fact)
		if f == nil {
			writeError(w, http.StatusNotFound, "no watched fact with that name", "not_found")
			return
		}
		n.Kind, n.Target, facts = "fact", f.Name, []*policy.Fact{f}
	case strings.TrimSpace(in.Category) != "":
		name := strings.TrimSpace(in.Category)
		if !s.cfg.Policy.HasCategory(name) {
			writeError(w, http.StatusNotFound, "no category with that name", "not_found")
			return
		}
		n.Kind, n.Target, n.Categories = "category", name, []string{name}
		facts = s.cfg.Policy.FactsInCategory(name)
	default:
		facts = s.cfg.Policy.FactsWithSource(in.SourceURL)
		if len(facts) == 0 {
			writeError(w, http.StatusNotFound, "no watched fact reads that source_url", "not_found")
			return
		}
		n.Kind, n.Target = "source_url", strings.TrimSpace(in.SourceURL)
	}

	seen := map[string]bool{}
	inv, _ := s.cfg.Engine.Evidence.(invalidator)
	for _, f := range facts {
		n.Facts = append(n.Facts, f.Name)
		if inv != nil && !seen[f.SourceURL] {
			seen[f.SourceURL] = true
			if inv.Invalidate(f.SourceURL) {
				n.EvidenceDropped = append(n.EvidenceDropped, f.SourceURL)
			}
		}
	}
	for _, key := range in.ClearCacheKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if s.cfg.Clearer == nil {
			n.ClearErrors = append(n.ClearErrors, "no cache clearer is configured")
			break
		}
		if err := s.cfg.Clearer.ClearKey(r.Context(), key); err != nil {
			n.ClearErrors = append(n.ClearErrors, oneLine(err.Error(), 200))
			continue
		}
		n.ClearedCacheKeys = append(n.ClearedCacheKeys, key)
	}
	saved, err := s.changes.Add(n)
	if err != nil {
		s.log.Warn("could not persist the change notice (kept in memory)", "error", err)
	}
	s.log.Info("change notice", "id", saved.ID, "kind", saved.Kind, "target", saved.Target,
		"facts", len(saved.Facts), "evidence_dropped", len(saved.EvidenceDropped), "cleared_keys", len(saved.ClearedCacheKeys))
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r, 50)
	if !ok {
		return
	}
	recs := s.changes.Recent(limit)
	writeJSON(w, http.StatusOK, struct {
		Count   int              `json:"count"`
		Notices []changes.Notice `json:"notices"`
	}{Count: len(recs), Notices: recs})
}

func (s *Server) handleAudits(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r, 100)
	if !ok {
		return
	}
	out := struct {
		Enabled bool              `json:"enabled"`
		Compare string            `json:"compare,omitempty"`
		Stats   AuditStats        `json:"stats"`
		Count   int               `json:"count"`
		Audits  []auditlog.Record `json:"audits"`
	}{Audits: []auditlog.Record{}}
	if s.auditor != nil {
		out.Enabled, out.Compare, out.Stats = true, s.auditor.cfg.Compare, s.auditor.stats()
		out.Audits = s.auditor.cfg.Log.Recent(limit)
		out.Count = len(out.Audits)
	}
	writeJSON(w, http.StatusOK, out)
}

func parseLimit(w http.ResponseWriter, r *http.Request, def int) (int, bool) {
	limit := def
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer", "invalid_request_error")
			return 0, false
		}
		limit = min(n, maxDecisionsLimit)
	}
	return limit, true
}
