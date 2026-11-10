// Package changes keeps "this changed" notices. A system that owns a fact (a
// pricing service, a CMS, a release script) tells the companion that a
// watched fact, a source page or a whole category changed, instead of the
// companion waiting for an evidence TTL or max_age to run out.
//
// A notice has two effects elsewhere: the proxy drops the cached evidence of
// the facts it covers, so the next check fetches the page again, and the
// decision engine treats an entry written before the notice as stale when no
// watched fact can check it. This package only remembers notices. The latest
// notice per fact and per category is kept for good (both are bounded by the
// policy) and persisted, so a restart cannot bring back an outdated answer.
package changes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultMaxRecent bounds the notice history shown by GET /freshness/changes.
const DefaultMaxRecent = 200

const fileVersion = 1

// Notice is one accepted change notice.
type Notice struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// Kind is "fact", "category" or "source_url"; Target is the name or URL
	// as the caller sent it.
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Note   string `json:"note,omitempty"`
	// Facts and Categories are what the notice covers. A source_url notice
	// covers the facts read from that page; a category notice covers the
	// category and its facts.
	Facts      []string `json:"facts,omitempty"`
	Categories []string `json:"categories,omitempty"`
	// EvidenceDropped lists the source URLs whose cached evidence was dropped.
	EvidenceDropped []string `json:"evidence_dropped,omitempty"`
	// ClearedCacheKeys lists Bifrost cache keys cleared on request, and
	// ClearErrors why a clear failed.
	ClearedCacheKeys []string `json:"cleared_cache_keys,omitempty"`
	ClearErrors      []string `json:"clear_errors,omitempty"`
}

// Subject describes what the notice says changed, for reasons and logs.
func (n Notice) Subject() string {
	return fmt.Sprintf("%s %q (%s)", n.Kind, n.Target, n.ID)
}

// Options configures a Store.
type Options struct {
	// Path is the JSON file. Empty keeps notices in memory only.
	Path string
	// MaxRecent bounds the history (default 200). It never bounds the
	// latest notice per fact or category.
	MaxRecent int
	// Now is the clock (tests inject one).
	Now func() time.Time
	// Logger receives warnings (e.g. a corrupt file). Optional.
	Logger *slog.Logger
}

// Store remembers change notices.
type Store struct {
	mu         sync.Mutex
	path       string
	maxRecent  int
	now        func() time.Time
	nextSeq    int
	recent     []Notice          // oldest first
	byFact     map[string]Notice // latest notice covering each fact
	byCategory map[string]Notice // latest notice covering each category
}

type fileFormat struct {
	Version    int               `json:"version"`
	NextSeq    int               `json:"next_seq"`
	Recent     []Notice          `json:"recent"`
	ByFact     map[string]Notice `json:"by_fact"`
	ByCategory map[string]Notice `json:"by_category"`
}

// Open loads the store from opts.Path when the file exists. A corrupt file
// is moved aside (to <path>.corrupt) and the store starts empty, like the
// age store, so a bad file can never stop the proxy.
func Open(opts Options) (*Store, error) {
	if opts.MaxRecent <= 0 {
		opts.MaxRecent = DefaultMaxRecent
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Store{
		path:       opts.Path,
		maxRecent:  opts.MaxRecent,
		now:        opts.Now,
		nextSeq:    1,
		byFact:     make(map[string]Notice),
		byCategory: make(map[string]Notice),
	}
	if s.path == "" {
		return s, nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read change store: %w", err)
	}
	var ff fileFormat
	if err := json.Unmarshal(data, &ff); err != nil || ff.Version != fileVersion {
		aside := s.path + ".corrupt"
		_ = os.Rename(s.path, aside)
		if opts.Logger != nil {
			opts.Logger.Warn("change store unreadable; starting empty (earlier change notices are forgotten)",
				"path", s.path, "moved_to", aside)
		}
		return s, nil
	}
	if ff.NextSeq > 0 {
		s.nextSeq = ff.NextSeq
	}
	s.recent = ff.Recent
	if len(s.recent) > s.maxRecent {
		s.recent = s.recent[len(s.recent)-s.maxRecent:]
	}
	for k, n := range ff.ByFact {
		s.byFact[k] = n
	}
	for k, n := range ff.ByCategory {
		s.byCategory[k] = n
	}
	return s, nil
}

// Add stamps n with an id and the current time, remembers it and persists
// the store. The notice is kept in memory even if the file write fails.
func (s *Store) Add(n Notice) (Notice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n.ID = fmt.Sprintf("chg-%d", s.nextSeq)
	s.nextSeq++
	n.Time = s.now().UTC()
	s.recent = append(s.recent, n)
	if len(s.recent) > s.maxRecent {
		s.recent = s.recent[len(s.recent)-s.maxRecent:]
	}
	for _, f := range n.Facts {
		s.byFact[f] = n
	}
	for _, c := range n.Categories {
		s.byCategory[c] = n
	}
	return n, s.saveLocked()
}

// Covering returns the newest notice that covers any of the categories or
// facts. ok is false when none does.
func (s *Store) Covering(categories, facts []string) (n Notice, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	consider := func(c Notice, found bool) {
		if found && (!ok || c.Time.After(n.Time)) {
			n, ok = c, true
		}
	}
	for _, c := range categories {
		got, found := s.byCategory[c]
		consider(got, found)
	}
	for _, f := range facts {
		got, found := s.byFact[f]
		consider(got, found)
	}
	return n, ok
}

// Recent returns up to limit notices, newest first.
func (s *Store) Recent(limit int) []Notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.recent) {
		limit = len(s.recent)
	}
	out := make([]Notice, 0, limit)
	for i := len(s.recent) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.recent[i])
	}
	return out
}

// saveLocked writes the file atomically (temp file + rename).
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(fileFormat{
		Version: fileVersion, NextSeq: s.nextSeq, Recent: s.recent,
		ByFact: s.byFact, ByCategory: s.byCategory,
	})
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
