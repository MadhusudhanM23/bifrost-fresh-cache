package watch

import (
	"container/list"
	"sync"

	"freshness-companion/internal/decisionlog"
)

// DefaultIndexSize bounds the entry index.
const DefaultIndexSize = 10000

// Index remembers which Bifrost cache entries state which value of a fact:
// fact -> cache id -> value. It learns from the decision log (every served
// hit and every checked new answer names the entry and the value it
// states), so the watcher knows what to delete when a fact changes.
type Index struct {
	mu     sync.Mutex
	max    int
	byFact map[string]map[string]string
	order  *list.List                 // of indexKey, oldest first, for bounding
	elems  map[indexKey]*list.Element // each live entry's place in order
	count  int
}

type indexKey struct{ fact, id string }

// NewIndex bounds the index at max entries (<= 0 uses DefaultIndexSize).
func NewIndex(max int) *Index {
	if max <= 0 {
		max = DefaultIndexSize
	}
	return &Index{max: max, byFact: map[string]map[string]string{}, order: list.New(), elems: map[indexKey]*list.Element{}}
}

// Learn reads one decision record. Watcher records and records without a
// fact are ignored. It is meant to be passed to decisionlog.Subscribe.
func (x *Index) Learn(r decisionlog.Record) {
	if r.Fact == "" || r.AnswerOf == "watcher" {
		return
	}
	switch r.AnswerOf {
	case "cached":
		switch r.Action {
		case "served", "served_with_warning":
			x.put(r.Fact, r.CacheID, r.CachedValue)
		case "refreshed", "corrected", "uncached":
			if r.RefreshStrategy == "clear_and_resend" {
				x.Forget(r.Fact, r.CacheID) // the stale entry was deleted
			}
			if r.Action == "refreshed" && r.RefreshedCheck == "matches_current" {
				x.put(r.Fact, r.RefreshedCacheID, r.CurrentValue)
			}
		}
	case "new":
		switch r.Correction {
		case "uncached", "corrected":
			// The contradicted entry was deleted; a corrected answer's own id
			// is not in the record, so it is learned on its first hit.
			x.Forget(r.Fact, r.CacheID)
		default:
			x.put(r.Fact, r.CacheID, r.CachedValue)
		}
	}
}

func (x *Index) put(fact, id, value string) {
	if fact == "" || id == "" || value == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	m := x.byFact[fact]
	if m == nil {
		m = map[string]string{}
		x.byFact[fact] = m
	}
	if _, had := m[id]; !had {
		x.count++
		k := indexKey{fact, id}
		x.elems[k] = x.order.PushBack(k)
	}
	m[id] = value
	for x.count > x.max && x.order.Len() > 0 {
		oldest := x.order.Front().Value.(indexKey)
		x.forgetLocked(oldest.fact, oldest.id)
	}
}

// Entries returns a copy of cache id -> value for fact.
func (x *Index) Entries(fact string) map[string]string {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make(map[string]string, len(x.byFact[fact]))
	for id, v := range x.byFact[fact] {
		out[id] = v
	}
	return out
}

// Forget drops one entry (after it was deleted from Bifrost).
func (x *Index) Forget(fact, id string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.forgetLocked(fact, id)
}

func (x *Index) forgetLocked(fact, id string) {
	m := x.byFact[fact]
	if _, ok := m[id]; !ok {
		return
	}
	delete(m, id)
	x.count--
	k := indexKey{fact, id}
	if e, ok := x.elems[k]; ok {
		x.order.Remove(e)
		delete(x.elems, k)
	}
	if len(m) == 0 {
		delete(x.byFact, fact)
	}
}

// Count returns how many entries are known for fact.
func (x *Index) Count(fact string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.byFact[fact])
}

// Len returns the total number of entries.
func (x *Index) Len() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.count
}
