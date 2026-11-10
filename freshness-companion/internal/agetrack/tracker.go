// Package agetrack remembers when each Bifrost cache entry was written.
//
// Bifrost does not expose an entry's age. On a cache miss it reports, in
// x-bifrost-cache-id, the id it writes the new entry under; a later hit names
// the entry it served by the same id. So the companion records the time of
// every miss response per cache id, and a hit's age is "now - that time".
// The most recent miss wins, because a miss (re)writes the entry. Entries
// persist to a small JSON file so a restart keeps them; an id the companion
// never saw on a miss has an unknown age.
package agetrack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DefaultMaxEntries bounds the tracker; the oldest entries are evicted first.
const DefaultMaxEntries = 10000

const fileVersion = 1

// Options configures a Tracker.
type Options struct {
	// Path is the JSON file. Empty keeps ages in memory only.
	Path string
	// MaxEntries bounds memory and file size (default 10000).
	MaxEntries int
	// Now is the clock (tests inject one).
	Now func() time.Time
	// Logger receives warnings (e.g. a corrupt file). Optional.
	Logger *slog.Logger
}

// Tracker maps cache ids to the time their entry was written.
type Tracker struct {
	mu         sync.Mutex
	path       string
	maxEntries int
	now        func() time.Time
	entries    map[string]time.Time
}

type fileFormat struct {
	Version int                  `json:"version"`
	Entries map[string]time.Time `json:"entries"`
}

// Open loads the tracker from opts.Path when the file exists. A corrupt file
// is moved aside (to <path>.corrupt) and the tracker starts empty, so a bad
// file can never stop the proxy; ages are then unknown until the next miss.
func Open(opts Options) (*Tracker, error) {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	t := &Tracker{
		path:       opts.Path,
		maxEntries: opts.MaxEntries,
		now:        opts.Now,
		entries:    make(map[string]time.Time),
	}
	if t.path == "" {
		return t, nil
	}
	data, err := os.ReadFile(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read age store: %w", err)
	}
	var ff fileFormat
	if err := json.Unmarshal(data, &ff); err != nil || ff.Version != fileVersion {
		aside := t.path + ".corrupt"
		_ = os.Rename(t.path, aside)
		if opts.Logger != nil {
			opts.Logger.Warn("age store unreadable; starting empty (entry ages unknown until their next miss)",
				"path", t.path, "moved_to", aside)
		}
		return t, nil
	}
	for id, ts := range ff.Entries {
		if id != "" && !ts.IsZero() {
			t.entries[id] = ts
		}
	}
	t.evictLocked()
	return t, nil
}

// RecordMiss records that Bifrost just (re)wrote the entry cacheID.
func (t *Tracker) RecordMiss(cacheID string) error {
	if cacheID == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[cacheID] = t.now().UTC()
	t.evictLocked()
	return t.saveLocked()
}

// Forget drops cacheID, e.g. after the companion cleared that entry.
func (t *Tracker) Forget(cacheID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.entries[cacheID]; !ok {
		return nil
	}
	delete(t.entries, cacheID)
	return t.saveLocked()
}

// Age returns how long ago the entry was written. ok is false when the id
// was never seen on a miss, or when the recorded time is in the future
// (clock change), because then the metadata is not reliable.
func (t *Tracker) Age(cacheID string) (age time.Duration, ok bool) {
	t.mu.Lock()
	ts, found := t.entries[cacheID]
	t.mu.Unlock()
	if !found {
		return 0, false
	}
	age = t.now().Sub(ts)
	if age < 0 {
		return 0, false
	}
	return age, true
}

// Len returns the number of tracked entries.
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

func (t *Tracker) evictLocked() {
	extra := len(t.entries) - t.maxEntries
	if extra <= 0 {
		return
	}
	type kv struct {
		id string
		ts time.Time
	}
	all := make([]kv, 0, len(t.entries))
	for id, ts := range t.entries {
		all = append(all, kv{id, ts})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ts.Equal(all[j].ts) {
			return all[i].id < all[j].id
		}
		return all[i].ts.Before(all[j].ts)
	})
	for _, e := range all[:extra] {
		delete(t.entries, e.id)
	}
}

// saveLocked writes the file atomically (temp file + rename).
func (t *Tracker) saveLocked() error {
	if t.path == "" {
		return nil
	}
	data, err := json.Marshal(fileFormat{Version: fileVersion, Entries: t.entries})
	if err != nil {
		return err
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(t.path)+".tmp-*")
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
	if err := os.Rename(tmpName, t.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
