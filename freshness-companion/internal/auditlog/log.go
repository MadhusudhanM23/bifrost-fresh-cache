// Package auditlog records the result of every wrong-match audit as one JSON
// line, and keeps the most recent ones in memory for GET /freshness/audits.
//
// An audit asks the model the same question again, bypassing the cache, and
// compares the fresh answer with the one the cache served. A record holds
// only scores and the verdict. Prompts and answers are never stored: Record
// has no field for them.
package auditlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultRingSize is how many recent audits the endpoint can return.
const DefaultRingSize = 1000

// Verdicts.
const (
	// Same: the fresh answer agrees with the cached one, so the match was right.
	Same = "same"
	// Different: the fresh answer disagrees, so the cache served a wrong match.
	Different = "different"
	// Error: the audit could not be completed; it counts for neither side.
	Error = "error"
)

// Record is one audit.
type Record struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	CacheID string    `json:"cache_id,omitempty"`
	// Similarity and Threshold are what Bifrost reported for the semantic hit.
	Similarity *float64 `json:"similarity,omitempty"`
	Threshold  *float64 `json:"threshold,omitempty"`
	// Near is true when the hit scored within the audit margin of the
	// threshold, so it was audited regardless of the sampling rate.
	Near bool `json:"near_threshold,omitempty"`
	// Gate is true when the check ran before the answer was served (a risky
	// semantic hit): a different verdict then means the wrong answer was
	// blocked, not that it reached the caller.
	Gate      bool    `json:"gate,omitempty"`
	Verdict   string  `json:"verdict"`
	Compare   string  `json:"compare"` // exact | judge
	Reason    string  `json:"reason,omitempty"`
	Error     string  `json:"error,omitempty"`
	LatencyMS float64 `json:"latency_ms"`
}

// Log appends records to a JSON Lines file and a bounded in-memory ring.
type Log struct {
	mu   sync.Mutex
	file *os.File
	ring []Record
	next int
	full bool
}

// Open opens (or creates) the JSONL file in append mode. An empty path keeps
// audits in memory only.
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
	defer l.mu.Unlock()
	l.ring[l.next] = r
	l.next = (l.next + 1) % len(l.ring)
	if l.next == 0 {
		l.full = true
	}
	if l.file == nil {
		return nil
	}
	_, err = l.file.Write(line)
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
		out = append(out, l.ring[(l.next-i+len(l.ring))%len(l.ring)])
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
