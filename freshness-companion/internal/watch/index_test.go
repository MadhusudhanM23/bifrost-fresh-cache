package watch

import (
	"strconv"
	"testing"

	"freshness-companion/internal/decisionlog"
)

const fact = "Acme Pro plan price"

func TestIndexLearnsFromDecisionRecords(t *testing.T) {
	x := NewIndex(0)
	// A served hit names the entry and the value it states.
	x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "served", Fact: fact, CacheID: "id-1", CachedValue: "49"})
	x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "served_with_warning", Fact: fact, CacheID: "id-2", CachedValue: "49"})
	// A new answer that was checked.
	x.Learn(decisionlog.Record{AnswerOf: "new", Action: "served", Fact: fact, CacheID: "id-3", CachedValue: "49"})
	// A stale entry refreshed: old id gone, fresh id learned with the current value.
	x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "refreshed", Fact: fact, CacheID: "id-1", RefreshStrategy: "clear_and_resend", RefreshedCacheID: "id-4", RefreshedCheck: "matches_current", CurrentValue: "59"})
	// A refreshed answer that still differs is not trusted into the index.
	x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "refreshed", Fact: fact, CacheID: "id-2", RefreshStrategy: "clear_and_resend", RefreshedCacheID: "id-5", RefreshedCheck: "differs_from_current", CurrentValue: "59"})
	// Uncached or corrected new answers are dropped.
	x.Learn(decisionlog.Record{AnswerOf: "new", Action: "uncached", Correction: "uncached", Fact: fact, CacheID: "id-3", CachedValue: "49"})
	// Records without a fact, or without a value, teach nothing.
	x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "served", CacheID: "id-9", CachedValue: "1"})
	x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "served", Fact: fact, CacheID: "id-10"})

	got := x.Entries(fact)
	want := map[string]string{"id-4": "59"}
	if len(got) != len(want) || got["id-4"] != "59" {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	if x.Count(fact) != 1 || x.Len() != 1 {
		t.Fatalf("count=%d len=%d", x.Count(fact), x.Len())
	}
	// With resend_with_headers the stale entry stays in Bifrost, so it is kept.
	x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "served", Fact: fact, CacheID: "id-6", CachedValue: "49"})
	x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "refreshed", Fact: fact, CacheID: "id-6", RefreshStrategy: "resend_with_headers", RefreshedCacheID: "", RefreshedCheck: "matches_current", CurrentValue: "59"})
	if x.Entries(fact)["id-6"] != "49" {
		t.Fatalf("resend_with_headers must keep the stale entry indexed: %v", x.Entries(fact))
	}
	x.Forget(fact, "id-6")
	x.Forget(fact, "never-there")
	if x.Count(fact) != 1 {
		t.Fatalf("after Forget count = %d", x.Count(fact))
	}
}

func TestIndexIgnoresWatcherRecords(t *testing.T) {
	x := NewIndex(0)
	x.Learn(decisionlog.Record{AnswerOf: "watcher", Action: "evicted", Fact: fact, CacheID: "id-1", CachedValue: "49", CurrentValue: "59", Evicted: []string{"id-1"}})
	x.Learn(decisionlog.Record{AnswerOf: "watcher", Action: "noted", Fact: fact, CachedValue: "49", CurrentValue: "59"})
	if x.Len() != 0 {
		t.Fatalf("watcher records must not be indexed: %v", x.Entries(fact))
	}
}

func TestIndexIsBounded(t *testing.T) {
	x := NewIndex(100)
	for i := 0; i < 250; i++ {
		x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "served", Fact: fact, CacheID: "id-" + strconv.Itoa(i), CachedValue: "49"})
	}
	if x.Len() != 100 {
		t.Fatalf("len = %d, want the bound 100", x.Len())
	}
	e := x.Entries(fact)
	if _, old := e["id-0"]; old {
		t.Fatal("the oldest entry should have been dropped")
	}
	if _, newest := e["id-249"]; !newest {
		t.Fatal("the newest entry must be kept")
	}
	// Re-learning a known id updates it without growing the index.
	x.Learn(decisionlog.Record{AnswerOf: "cached", Action: "served", Fact: fact, CacheID: "id-249", CachedValue: "59"})
	if x.Len() != 100 || x.Entries(fact)["id-249"] != "59" {
		t.Fatalf("len=%d value=%q", x.Len(), x.Entries(fact)["id-249"])
	}
}

func TestIndexForgetDoesNotLeakOrder(t *testing.T) {
	x := NewIndex(10)
	for i := 0; i < 1000; i++ {
		x.put("f", "same-id", "49")
		x.Forget("f", "same-id")
	}
	if x.Len() != 0 || x.order.Len() != 0 || len(x.elems) != 0 {
		t.Fatalf("after put/forget cycles: count=%d order=%d elems=%d, want all 0", x.Len(), x.order.Len(), len(x.elems))
	}
}

func TestIndexEvictsOldestWhenFull(t *testing.T) {
	x := NewIndex(2)
	x.put("f", "a", "1")
	x.Forget("f", "a")
	x.put("f", "b", "1") // oldest live entry
	x.put("f", "a", "1") // re-learned, newest
	x.put("f", "c", "1") // over the bound: b must go, not a
	got := x.Entries("f")
	if _, ok := got["b"]; ok {
		t.Fatalf("oldest entry b kept: %v", got)
	}
	if _, ok := got["a"]; !ok {
		t.Fatalf("newest entry a dropped: %v", got)
	}
	if x.Len() != 2 {
		t.Fatalf("Len = %d, want 2", x.Len())
	}
}
