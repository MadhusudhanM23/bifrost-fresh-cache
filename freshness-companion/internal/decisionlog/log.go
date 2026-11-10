// Package decisionlog records every freshness decision as one JSON line,
// and keeps the most recent ones in memory for GET /freshness/decisions.
//
// A record holds only metadata and the values extracted by the policy
// patterns. Prompts and answers are never stored: Record has no field for them.
package decisionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultRingSize is how many recent decisions the endpoint can return.
const DefaultRingSize = 1000

// Record is one freshness decision.
type Record struct {
	Time             time.Time  `json:"time"`
	CacheID          string     `json:"cache_id,omitempty"`
	HitType          string     `json:"hit_type,omitempty"`
	Similarity       string     `json:"similarity,omitempty"`
	Category         string     `json:"category"`
	Fact             string     `json:"fact,omitempty"`
	Outcome          string     `json:"outcome"`
	Action           string     `json:"action"`
	Reason           string     `json:"reason"`
	SourceURL        string     `json:"source_url,omitempty"`
	Provider         string     `json:"provider,omitempty"`
	CachedValue      string     `json:"cached_value,omitempty"`
	CurrentValue     string     `json:"current_value,omitempty"`
	CheckedAt        *time.Time `json:"checked_at,omitempty"`
	EvidenceCached   bool       `json:"evidence_cached,omitempty"`
	AgeSeconds       *float64   `json:"age_seconds,omitempty"`
	RefreshStrategy  string     `json:"refresh_strategy,omitempty"`
	RefreshedCacheID string     `json:"refreshed_cache_id,omitempty"`
	RefreshedCheck   string     `json:"refreshed_check,omitempty"`
	Error            string     `json:"error,omitempty"`
	LatencyMS        float64    `json:"latency_ms"`

	// AnswerOf says what was checked: "cached" (a hit) or "new" (a fresh
	// answer on a miss, so CachedValue is the value that answer states).
	AnswerOf string `json:"answer_of,omitempty"`
	// ChangeNotice is the id of the change notice that forced this check.
	ChangeNotice string `json:"change_notice,omitempty"`
	// Correction is what on_contradicted did to a fresh answer that disagreed
	// with the evidence: reported, uncached or corrected.
	Correction string `json:"correction,omitempty"`
	// CorrectedValue and CorrectedCheck describe the answer asked for again
	// with the verified value (Correction == "corrected").
	CorrectedValue string `json:"corrected_value,omitempty"`
	CorrectedCheck string `json:"corrected_check,omitempty"`
	// SharedRefresh is true when this request waited for a refresh another
	// request was already running, instead of starting its own.
	SharedRefresh bool `json:"shared_refresh,omitempty"`
	// Gate is what the pre-serve check did to a risky semantic hit:
	// verified, trusted, blocked or unverified (empty = it did not apply).
	Gate string `json:"gate,omitempty"`
	// Evicted lists the cache ids the background watcher deleted because
	// they stated the old value of a fact that changed (AnswerOf "watcher").
	Evicted []string `json:"evicted,omitempty"`
	// Discovery (-discover). Discovered is true when a page Tavily Search
	// found decided the outcome; Discovery is the discovery result whenever
	// it was tried (confirmed, stale, unable_to_verify, no_source, over_cap,
	// ...). DiscoveryQuery is the exact query sent to Tavily Search: built
	// from the policy template and allowlisted names, never the prompt.
	Discovered     bool    `json:"discovered,omitempty"`
	Discovery      string  `json:"discovery,omitempty"`
	DiscoveryQuery string  `json:"discovery_query,omitempty"`
	DiscoveryRank  int     `json:"discovery_rank,omitempty"`
	DiscoveryScore float64 `json:"discovery_score,omitempty"`
	FoundBy        string  `json:"found_by,omitempty"`
}

// Log appends records to a JSON Lines file and a bounded in-memory ring.
type Log struct {
	mu   sync.Mutex
	file *os.File
	ring []Record
	next int
	full bool
	subs []func(Record)
}

// Subscribe registers fn to be called with every record appended from now
// on. Callbacks run on the appending goroutine after the log's lock is
// released, so they may call back into the log; they must not block for long.
func (l *Log) Subscribe(fn func(Record)) {
	if fn == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.subs = append(l.subs, fn)
}

// Open opens (or creates) the JSONL file in append mode. An empty path keeps
// decisions in memory only.
func Open(path string, ringSize int) (*Log, error) {
	if ringSize <= 0 {
		ringSize = DefaultRingSize
	}
	l := &Log{ring: make([]Record, ringSize)}
	if path == "" {
		return l, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	l.file = f
	return l, nil
}

// Append stores r. The in-memory copy is kept even if the file write fails.
func (l *Log) Append(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	l.mu.Lock()
	l.ring[l.next] = r
	l.next = (l.next + 1) % len(l.ring)
	if l.next == 0 {
		l.full = true
	}
	if l.file != nil {
		_, err = l.file.Write(line)
	}
	subs := l.subs
	l.mu.Unlock()
	for _, fn := range subs {
		fn(r)
	}
	return err
}

// Recent returns up to limit records, newest first.
func (l *Log) Recent(limit int) []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.next
	if l.full {
		n = len(l.ring)
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Record, 0, limit)
	for i := 1; i <= limit; i++ {
		idx := (l.next - i + len(l.ring)) % len(l.ring)
		out = append(out, l.ring[idx])
	}
	return out
}

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
