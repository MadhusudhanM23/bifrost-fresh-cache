package verified

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"  What's the PRICE?! ": "what s the price",
		"How   much\tis it":     "how much is it",
		"€5 / month":            "5 month",
		"???":                   "",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseValidates(t *testing.T) {
	bad := map[string]string{
		"not an array":     `{"question":"a","answer":"b"}`,
		"empty question":   `[{"question":"  ","answer":"b"}]`,
		"empty answer":     `[{"question":"a","answer":""}]`,
		"punctuation only": `[{"question":"?!","answer":"b"}]`,
		"duplicate":        `[{"question":"Is it open?","answer":"a"},{"question":"is it OPEN","answer":"b"}]`,
		"unknown field":    `[{"question":"a","answer":"b","colour":"red"}]`,
		"extra content":    `[] []`,
		"huge answer":      `[{"question":"a","answer":"` + strings.Repeat("x", MaxAnswerBytes+1) + `"}]`,
	}
	for name, doc := range bad {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte(strings.Repeat(" ", MaxFileBytes+1))); err == nil {
		t.Error("oversize file accepted")
	}
	// The same question for two different scopes is fine.
	s, err := Parse([]byte(`[{"question":"Refund?","answer":"30 days","scope":"acme"},{"question":"refund","answer":"14 days","scope":"globex"},{"question":"refund","answer":"7 days"}]`))
	if err != nil || s.Len() != 3 {
		t.Fatalf("scoped duplicates: %v, %d", err, s.Len())
	}
	// Errors never quote the text.
	_, err = Parse([]byte(`[{"question":"SECRET-Q","answer":"SECRET-A"},{"question":"secret q","answer":"x"}]`))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error must name the entry, not quote it: %v", err)
	}
}

func TestLookupScopes(t *testing.T) {
	s, err := Parse([]byte(`[
	  {"question":"Refund policy?","answer":"everyone"},
	  {"question":"refund policy","answer":"acme only","scope":"acme"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Lookup("REFUND POLICY!", "acme"); !ok || e.Answer != "acme only" {
		t.Fatalf("scoped match must win: %+v %v", e, ok)
	}
	if e, ok := s.Lookup("refund policy", "globex"); !ok || e.Answer != "everyone" {
		t.Fatalf("other tenants get the global entry only: %+v %v", e, ok)
	}
	if e, ok := s.Lookup("refund policy", ""); !ok || e.Answer != "everyone" {
		t.Fatalf("no scope gets the global entry: %+v %v", e, ok)
	}
	only, _ := Parse([]byte(`[{"question":"secret","answer":"a","scope":"acme"}]`))
	if _, ok := only.Lookup("secret", "globex"); ok {
		t.Fatal("a scoped entry leaked to another scope")
	}
	if _, ok := only.Lookup("secret", ""); ok {
		t.Fatal("a scoped entry leaked to a request with no scope")
	}
	if _, ok := s.Lookup("refund policy please", ""); ok {
		t.Fatal("matching must be exact, not fuzzy")
	}
}

func writeFile(t *testing.T, path, doc string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestStoreReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verified.json")
	base := time.Now().Add(-time.Hour)
	writeFile(t, path, `[{"question":"one","answer":"1"}]`, base)
	st, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Reload() {
		t.Fatal("an unchanged file must not reload")
	}
	writeFile(t, path, `[{"question":"one","answer":"1"},{"question":"two","answer":"2"}]`, base.Add(time.Minute))
	if !st.Reload() || st.Set().Len() != 2 {
		t.Fatal("a changed file must reload")
	}
	// A broken edit is rejected; the old entries stay.
	writeFile(t, path, `[{"question":"one","answer":""}]`, base.Add(2*time.Minute))
	if st.Reload() || st.Set().Len() != 2 {
		t.Fatal("a bad file must be rejected and the old set kept")
	}
	if _, msg := st.Info(); msg == "" {
		t.Fatal("the rejection must be visible")
	}
	// Fixing it recovers.
	writeFile(t, path, `[{"question":"three","answer":"3"}]`, base.Add(3*time.Minute))
	if !st.Reload() || st.Set().Len() != 1 {
		t.Fatal("a fixed file must reload")
	}
	if _, msg := st.Info(); msg != "" {
		t.Fatalf("the error must clear: %q", msg)
	}
}

func TestOpenRejectsBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.json")
	writeFile(t, path, `[{"question":"a","answer":"b"},{"question":"A","answer":"c"}]`, time.Now())
	if _, err := Open(path, nil); err == nil {
		t.Fatal("startup must fail on duplicates")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing.json"), nil); err == nil {
		t.Fatal("startup must fail on a missing file")
	}
}

func TestWatchPicksUpChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.json")
	base := time.Now().Add(-time.Hour)
	writeFile(t, path, `[{"question":"one","answer":"1"}]`, base)
	st, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer close(done)
	go st.Watch(done, 10*time.Millisecond)
	writeFile(t, path, `[{"question":"one","answer":"1"},{"question":"two","answer":"2"}]`, base.Add(time.Minute))
	deadline := time.Now().Add(3 * time.Second)
	for st.Set().Len() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the watcher did not reload")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
