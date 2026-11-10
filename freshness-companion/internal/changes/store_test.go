package changes

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { c.t = c.t.Add(time.Second); return c.t }

func TestAddAndCovering(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s, err := Open(Options{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Covering([]string{"price"}, []string{"Pro"}); ok {
		t.Fatal("an empty store covers nothing")
	}
	n1, _ := s.Add(Notice{Kind: "fact", Target: "Pro", Facts: []string{"Pro"}})
	n2, _ := s.Add(Notice{Kind: "category", Target: "policy", Categories: []string{"policy"}})
	if n1.ID != "chg-1" || n2.ID != "chg-2" || !n2.Time.After(n1.Time) {
		t.Fatalf("ids/times = %+v %+v", n1, n2)
	}
	if got, ok := s.Covering(nil, []string{"Pro"}); !ok || got.ID != "chg-1" {
		t.Fatalf("fact cover = %+v %v", got, ok)
	}
	if got, ok := s.Covering([]string{"policy"}, nil); !ok || got.ID != "chg-2" {
		t.Fatalf("category cover = %+v %v", got, ok)
	}
	if got, ok := s.Covering([]string{"policy", "price"}, []string{"Pro"}); !ok || got.ID != "chg-2" {
		t.Fatalf("the newest covering notice wins, got %+v", got)
	}
	if _, ok := s.Covering([]string{"schedule"}, []string{"Other"}); ok {
		t.Fatal("unrelated categories and facts are not covered")
	}
	n3, _ := s.Add(Notice{Kind: "fact", Target: "Pro", Facts: []string{"Pro"}})
	if got, _ := s.Covering(nil, []string{"Pro"}); got.ID != n3.ID {
		t.Fatalf("a later notice replaces the earlier one, got %s", got.ID)
	}
}

func TestRecentIsNewestFirstAndBounded(t *testing.T) {
	s, _ := Open(Options{MaxRecent: 3})
	for i := 0; i < 5; i++ {
		s.Add(Notice{Kind: "category", Target: "price", Categories: []string{"price"}})
	}
	got := s.Recent(10)
	if len(got) != 3 || got[0].ID != "chg-5" || got[2].ID != "chg-3" {
		t.Fatalf("recent = %+v", got)
	}
	if len(s.Recent(2)) != 2 {
		t.Fatal("limit not applied")
	}
	// The history is bounded, the latest notice per category is not.
	if _, ok := s.Covering([]string{"price"}, nil); !ok {
		t.Fatal("coverage must outlive the history window")
	}
}

func TestPersistsAcrossRestartAndSurvivesCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "changes.json")
	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Add(Notice{Kind: "fact", Target: "Pro", Facts: []string{"Pro"}, Note: "raised"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Covering(nil, []string{"Pro"})
	if !ok || got.ID != n.ID || !got.Time.Equal(n.Time) || got.Note != "raised" {
		t.Fatalf("after restart = %+v %v", got, ok)
	}
	next, _ := again.Add(Notice{Kind: "category", Target: "x", Categories: []string{"x"}})
	if next.ID != "chg-2" {
		t.Fatalf("ids must keep counting after a restart, got %s", next.ID)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("a corrupt file must not stop the proxy: %v", err)
	}
	if len(fresh.Recent(5)) != 0 {
		t.Fatal("a corrupt store starts empty")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("the corrupt file should be kept aside: %v", err)
	}
}
