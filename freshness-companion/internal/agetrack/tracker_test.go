package agetrack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

var t0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func TestAgeFromMissTime(t *testing.T) {
	c := &clock{t: t0}
	tr, err := Open(Options{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.Age("entry-1"); ok {
		t.Fatal("an id never seen on a miss must have an unknown age")
	}
	if err := tr.RecordMiss("entry-1"); err != nil {
		t.Fatal(err)
	}
	c.Set(t0.Add(7 * time.Minute))
	if age, ok := tr.Age("entry-1"); !ok || age != 7*time.Minute {
		t.Fatalf("age = %v, %v; want 7m", age, ok)
	}
	// A new miss on the same id means Bifrost rewrote the entry: the age restarts.
	if err := tr.RecordMiss("entry-1"); err != nil {
		t.Fatal(err)
	}
	c.Set(t0.Add(8 * time.Minute))
	if age, _ := tr.Age("entry-1"); age != time.Minute {
		t.Fatalf("age after rewrite = %v, want 1m", age)
	}
	// A clock that went backwards makes the metadata unreliable.
	c.Set(t0)
	if _, ok := tr.Age("entry-1"); ok {
		t.Fatal("a write time in the future must give an unknown age")
	}
	if err := tr.Forget("entry-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.Age("entry-1"); ok || tr.Len() != 0 {
		t.Fatal("Forget should drop the entry")
	}
}

func TestAgesSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "cache-ages.json")
	c := &clock{t: t0}
	tr, err := Open(Options{Path: path, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.RecordMiss("entry-a"); err != nil {
		t.Fatal(err)
	}
	c.Set(t0.Add(time.Minute))
	if err := tr.RecordMiss("entry-b"); err != nil {
		t.Fatal(err)
	}

	// A new process: same file, later clock.
	c.Set(t0.Add(30 * time.Minute))
	reopened, err := Open(Options{Path: path, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if age, ok := reopened.Age("entry-a"); !ok || age != 30*time.Minute {
		t.Fatalf("entry-a after restart: %v, %v; want 30m", age, ok)
	}
	if age, ok := reopened.Age("entry-b"); !ok || age != 29*time.Minute {
		t.Fatalf("entry-b after restart: %v, %v; want 29m", age, ok)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ff fileFormat
	if err := json.Unmarshal(data, &ff); err != nil || ff.Version != 1 || len(ff.Entries) != 2 {
		t.Fatalf("file content = %s (%v)", data, err)
	}
}

func TestEvictsOldestBeyondMax(t *testing.T) {
	c := &clock{t: t0}
	tr, _ := Open(Options{MaxEntries: 2, Now: c.Now})
	for i, id := range []string{"old", "mid", "new"} {
		c.Set(t0.Add(time.Duration(i) * time.Minute))
		if err := tr.RecordMiss(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := tr.Age("old"); ok {
		t.Fatal("the oldest entry should have been evicted")
	}
	if _, ok := tr.Age("new"); !ok || tr.Len() != 2 {
		t.Fatalf("len = %d", tr.Len())
	}
}

func TestCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache-ages.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("a corrupt file must not stop the companion: %v", err)
	}
	if tr.Len() != 0 {
		t.Fatal("want an empty tracker")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file not moved aside: %v", err)
	}
	if err := tr.RecordMiss("x"); err != nil {
		t.Fatalf("tracker unusable after a corrupt file: %v", err)
	}
}
