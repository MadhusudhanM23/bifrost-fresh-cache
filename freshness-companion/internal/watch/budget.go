package watch

import (
	"sync"
	"time"
)

// Budget is a rolling one-hour cap on watcher fetches. Over the cap the
// watcher skips a check; the request path still fetches when evidence
// expires, so the watcher is never worse than no watcher.
type Budget struct {
	mu    sync.Mutex
	max   int
	now   func() time.Time
	times []time.Time // fetch times within the last hour, oldest first
}

// NewBudget allows max fetches per rolling hour. Zero allows none.
func NewBudget(max int, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	if max < 0 {
		max = 0
	}
	return &Budget{max: max, now: now}
}

// Take reserves one fetch if the hour's budget allows it.
func (b *Budget) Take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.pruneLocked(now)
	if len(b.times) >= b.max {
		return false
	}
	b.times = append(b.times, now)
	return true
}

// Used returns how many fetches were taken in the last hour.
func (b *Budget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(b.now())
	return len(b.times)
}

// Max returns the hourly cap.
func (b *Budget) Max() int { return b.max }

func (b *Budget) pruneLocked(now time.Time) {
	cutoff := now.Add(-time.Hour)
	i := 0
	for i < len(b.times) && !b.times[i].After(cutoff) {
		i++
	}
	if i > 0 {
		b.times = append(b.times[:0], b.times[i:]...)
	}
}
