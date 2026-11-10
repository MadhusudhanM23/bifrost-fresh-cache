package decisionlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
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
