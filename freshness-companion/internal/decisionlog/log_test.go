package decisionlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendWritesJSONLinesAndRecentIsNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "decisions.jsonl")
	l, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i, outcome := range []string{"confirmed", "stale", "unable_to_verify", "unknown"} {
		age := float64(i)
		if err := l.Append(Record{Time: time.Unix(int64(i), 0).UTC(), CacheID: outcome + "-id", Category: "price", Outcome: outcome, Action: "served", Reason: "r", AgeSeconds: &age}); err != nil {
			t.Fatal(err)
		}
	}
	got := l.Recent(10)
	if len(got) != 3 || got[0].Outcome != "unknown" || got[2].Outcome != "stale" {
		t.Fatalf("ring (size 3) newest first = %+v", got)
	}
	if one := l.Recent(1); len(one) != 1 || one[0].Outcome != "unknown" {
		t.Fatalf("Recent(1) = %+v", one)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line %d is not JSON: %v", lines+1, err)
		}
		for _, field := range []string{"time", "category", "outcome", "action", "reason", "latency_ms"} {
			if _, ok := rec[field]; !ok {
				t.Fatalf("line %d lacks %q: %s", lines+1, field, sc.Text())
			}
		}
		for _, forbidden := range []string{"prompt", "messages", "answer", "body"} {
			if _, ok := rec[forbidden]; ok {
				t.Fatalf("a decision record must not carry %q", forbidden)
			}
		}
		lines++
	}
	if lines != 4 {
		t.Fatalf("file has %d lines, want all 4 records", lines)
	}
}

func TestMemoryOnly(t *testing.T) {
	l, err := Open("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Recent(5); len(got) != 0 {
		t.Fatalf("empty log returned %d records", len(got))
	}
	_ = l.Append(Record{Outcome: "confirmed"})
	if got := l.Recent(0); len(got) != 1 {
		t.Fatalf("Recent(0) should return all, got %d", len(got))
	}
}

func TestSubscribeGetsEveryRecord(t *testing.T) {
	l, err := Open("", 2)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Append(Record{CacheID: "before"}) // not delivered: subscribed later
	var got []string
	var seenFromInside int
	l.Subscribe(func(r Record) {
		got = append(got, r.CacheID)
		// The callback runs after unlock, so it may read the log.
		seenFromInside = len(l.Recent(0))
	})
	l.Subscribe(nil) // ignored
	for _, id := range []string{"a", "b", "c"} {
		if err := l.Append(Record{CacheID: id, AnswerOf: "watcher", Evicted: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("subscriber got %v, want a b c in order", got)
	}
	if seenFromInside != 2 {
		t.Fatalf("Recent from inside the callback saw %d records, want the ring size 2", seenFromInside)
	}
	line, _ := json.Marshal(Record{AnswerOf: "watcher", Evicted: []string{"id-1"}})
	if s := string(line); !strings.Contains(s, `"evicted":["id-1"]`) || !strings.Contains(s, `"answer_of":"watcher"`) {
		t.Fatalf("watcher record JSON = %s", s)
	}
}
