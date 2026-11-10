package watch

import (
	"errors"
	"sync"
	"testing"
	"time"

	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
)

// fakeClock is a settable clock shared by every component under test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const testPolicy = `{
  "version": 1,
  "categories": [
    {"name": "price", "keywords": ["price", "cost", "per month", "plan"], "max_age": "10m", "on_unverifiable": "bypass", "verify_new_answers": true}
  ],
  "facts": [
    {"name": "Acme Pro plan price", "category": "price", "source_url": "http://demoserver:8000/",
     "source_pattern": "Acme Pro plan[^$0-9]{0,20}\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*per month",
     "answer_pattern": "(?i)\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*(?:/\\s*mo(?:nth)?|per\\s+month|a\\s+month|monthly)\\b",
     "compare": "numeric"},
    {"name": "Acme Pro seats", "category": "price", "source_url": "http://demoserver:8000/",
     "source_pattern": "includes ([0-9]+) seats",
     "answer_pattern": "([0-9]+) seats",
     "compare": "numeric"},
    {"name": "Acme support hours", "category": "price", "source_url": "http://demoserver:8000/hours",
     "source_pattern": "Support: ([^.]+)\\.",
     "answer_pattern": "Support: ([^.]+)\\.",
     "compare": "exact"}
  ]
}`

func testPol(t *testing.T) *policy.Policy {
	t.Helper()
	p, err := policy.Parse([]byte(testPolicy))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	srcURL   = "http://demoserver:8000/"
	hoursURL = "http://demoserver:8000/hours"
)

func page(price string, seats int) string {
	s := "Acme Pro plan — $" + price + " per month."
	if seats > 0 {
		s += " It includes " + itoa(seats) + " seats."
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newTestSchedule(t *testing.T, clock *fakeClock, adaptive bool) (*Schedule, *[]Change) {
	t.Helper()
	s, err := NewSchedule(Config{Floor: 5 * time.Second, Max: 60 * time.Second, Growth: 2, Adaptive: adaptive, Now: clock.Now}, testPol(t).Facts)
	if err != nil {
		t.Fatal(err)
	}
	var changes []Change
	s.SetOnChange(func(c Change) { changes = append(changes, c) })
	return s, &changes
}

func fetched(s *Schedule, url, content string) {
	s.Fetched(url, evidence.Evidence{URL: url, Content: content, Provider: "test", FetchedAt: s.cfg.Now()}, nil)
}

func TestScheduleStartsAtFloorAndGrowsToCap(t *testing.T) {
	clock := newClock()
	s, changes := newTestSchedule(t, clock, true)
	if got := s.TTL(srcURL); got != 5*time.Second {
		t.Fatalf("initial TTL = %s, want the 5 s floor", got)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}
	for i, w := range want {
		fetched(s, srcURL, page("49", 5))
		if got := s.TTL(srcURL); got != w {
			t.Fatalf("after check %d TTL = %s, want %s", i+1, got, w)
		}
		clock.Advance(w)
	}
	if len(*changes) != 0 {
		t.Fatalf("no change was expected, got %+v", *changes)
	}
	st, _ := s.Source(srcURL)
	if st.Checks != 6 || st.UnchangedStreak != 5 || st.Values["Acme Pro plan price"] != "49" || st.Values["Acme Pro seats"] != "5" {
		t.Fatalf("state = %+v", st)
	}
	if st.NextCheckAt == nil || !st.NextCheckAt.Equal(st.LastCheckedAt.Add(60*time.Second)) {
		t.Fatalf("next check %v, want last check + 60 s (%v)", st.NextCheckAt, st.LastCheckedAt)
	}
	if got := s.TTL("http://nobody-reads-this/"); got != 5*time.Second {
		t.Fatalf("unknown URL TTL = %s, want the floor", got)
	}
}

func TestScheduleResetsToFloorOnChangeAndEmitsOldNew(t *testing.T) {
	clock := newClock()
	s, changes := newTestSchedule(t, clock, true)
	for i := 0; i < 4; i++ {
		fetched(s, srcURL, page("49", 5))
	}
	if got := s.TTL(srcURL); got != 40*time.Second {
		t.Fatalf("TTL before the change = %s, want 40 s", got)
	}
	clock.Advance(time.Minute)
	fetched(s, srcURL, page("59", 5))
	if got := s.TTL(srcURL); got != 5*time.Second {
		t.Fatalf("TTL after the change = %s, want the floor", got)
	}
	if len(*changes) != 1 {
		t.Fatalf("changes = %+v, want exactly one", *changes)
	}
	c := (*changes)[0]
	if c.SourceURL != srcURL || c.Provider != "test" || !c.At.Equal(clock.Now()) {
		t.Fatalf("change = %+v", c)
	}
	if len(c.Facts) != 1 || c.Facts[0].Name != "Acme Pro plan price" || c.Facts[0].Old != "49" || c.Facts[0].New != "59" || !c.Facts[0].NewReadable {
		t.Fatalf("change facts = %+v: only the price changed, seats did not", c.Facts)
	}
	st, _ := s.Source(srcURL)
	if st.Changes != 1 || st.UnchangedStreak != 0 || st.LastChangeAt == nil {
		t.Fatalf("state = %+v", st)
	}
	// Equivalent spellings are not a change.
	fetched(s, srcURL, page("59.00", 5))
	if len(*changes) != 1 || s.TTL(srcURL) != 10*time.Second {
		t.Fatalf("\"59.00\" must equal \"59\": changes=%d TTL=%s", len(*changes), s.TTL(srcURL))
	}
}

func TestScheduleFixedWhenNotAdaptive(t *testing.T) {
	clock := newClock()
	s, changes := newTestSchedule(t, clock, false)
	for i := 0; i < 5; i++ {
		fetched(s, srcURL, page("49", 5))
		if got := s.TTL(srcURL); got != 5*time.Second {
			t.Fatalf("check %d: TTL = %s, want the floor every time", i+1, got)
		}
	}
	// Changes are still noticed and reported with adaptive off.
	fetched(s, srcURL, page("59", 5))
	if len(*changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(*changes))
	}
}

func TestScheduleUnreadableValueResetsAndDoesNotGrow(t *testing.T) {
	clock := newClock()
	s, changes := newTestSchedule(t, clock, true)
	fetched(s, srcURL, page("49", 5))
	fetched(s, srcURL, page("49", 5))
	if s.TTL(srcURL) != 10*time.Second {
		t.Fatalf("TTL = %s, want 10 s", s.TTL(srcURL))
	}
	// The seats sentence disappears: that fact is unreadable.
	fetched(s, srcURL, page("49", 0))
	if s.TTL(srcURL) != 5*time.Second {
		t.Fatalf("TTL with an unreadable fact = %s, want the floor", s.TTL(srcURL))
	}
	if len(*changes) != 1 || (*changes)[0].Facts[0].Name != "Acme Pro seats" || (*changes)[0].Facts[0].NewReadable || (*changes)[0].Facts[0].Old != "5" {
		t.Fatalf("a fact that became unreadable must be reported with NewReadable false: %+v", *changes)
	}
	fetched(s, srcURL, page("49", 0))
	fetched(s, srcURL, page("49", 0))
	if s.TTL(srcURL) != 5*time.Second {
		t.Fatalf("TTL must not grow while a fact is unreadable, got %s", s.TTL(srcURL))
	}
	// Conflicting values on the page are unreadable too.
	fetched(s, hoursURL, "Support: 9-5. Support: 10-6.")
	fetched(s, hoursURL, "Support: 9-5. Support: 10-6.")
	if s.TTL(hoursURL) != 5*time.Second {
		t.Fatalf("conflicting values: TTL = %s, want the floor", s.TTL(hoursURL))
	}
}

func TestScheduleFetchErrorKeepsInterval(t *testing.T) {
	clock := newClock()
	s, changes := newTestSchedule(t, clock, true)
	for i := 0; i < 3; i++ {
		fetched(s, srcURL, page("49", 5))
	}
	if s.TTL(srcURL) != 20*time.Second {
		t.Fatalf("TTL = %s, want 20 s", s.TTL(srcURL))
	}
	clock.Advance(20 * time.Second)
	s.Fetched(srcURL, evidence.Evidence{}, errors.New("connection refused"))
	if s.TTL(srcURL) != 20*time.Second {
		t.Fatalf("a fetch error must leave the interval alone, got %s", s.TTL(srcURL))
	}
	st, _ := s.Source(srcURL)
	if st.Errors != 1 || st.LastError != "connection refused" || st.Checks != 4 {
		t.Fatalf("state = %+v", st)
	}
	if st.NextCheckAt == nil || !st.NextCheckAt.Equal(clock.Now().Add(5*time.Second)) {
		t.Fatalf("after an error the retry is due after the floor, got %v", st.NextCheckAt)
	}
	if len(*changes) != 0 {
		t.Fatalf("an error is not a change: %+v", *changes)
	}
	fetched(s, srcURL, page("49", 5))
	if s.TTL(srcURL) != 40*time.Second {
		t.Fatalf("the next good check continues from 20 s, got %s", s.TTL(srcURL))
	}
	if st, _ := s.Source(srcURL); st.LastError != "" {
		t.Fatalf("a good check clears last_error, got %q", st.LastError)
	}
}

func TestScheduleNoticeResetsToFloor(t *testing.T) {
	clock := newClock()
	s, changes := newTestSchedule(t, clock, true)
	for i := 0; i < 4; i++ {
		fetched(s, srcURL, page("49", 5))
	}
	if s.TTL(srcURL) != 40*time.Second {
		t.Fatalf("TTL = %s, want 40 s", s.TTL(srcURL))
	}
	s.Invalidated(srcURL)
	if s.TTL(srcURL) != 5*time.Second {
		t.Fatalf("a change notice must reset the interval to the floor, got %s", s.TTL(srcURL))
	}
	// The remembered values survive, so the next fetch still reports the change.
	fetched(s, srcURL, page("59", 5))
	if len(*changes) != 1 || (*changes)[0].Facts[0].Old != "49" {
		t.Fatalf("after a notice the next fetch must still report old -> new: %+v", *changes)
	}
	s.Invalidated("http://nobody/") // unknown URLs are ignored
}

func TestScheduleFingerprintCoversEveryFactOnSource(t *testing.T) {
	clock := newClock()
	s, changes := newTestSchedule(t, clock, true)
	fetched(s, srcURL, page("49", 5))
	fetched(s, srcURL, page("49", 5))
	// Only the second fact on the page changes: the interval resets and the
	// change names the seats, not the price.
	fetched(s, srcURL, page("49", 10))
	if s.TTL(srcURL) != 5*time.Second {
		t.Fatalf("TTL = %s, want the floor: a second fact on the same page changed", s.TTL(srcURL))
	}
	if len(*changes) != 1 || len((*changes)[0].Facts) != 1 || (*changes)[0].Facts[0].Name != "Acme Pro seats" || (*changes)[0].Facts[0].New != "10" {
		t.Fatalf("changes = %+v", *changes)
	}
	// Both change at once: one Change with two facts.
	fetched(s, srcURL, page("79", 20))
	if len(*changes) != 2 || len((*changes)[1].Facts) != 2 {
		t.Fatalf("changes = %+v, want a second change with both facts", *changes)
	}
	// Sources are keyed by the exact URL string; the hours page is separate.
	if u, ok := s.SourceForFact("Acme support hours"); !ok || u != hoursURL {
		t.Fatalf("SourceForFact = %q, %v", u, ok)
	}
	if _, ok := s.SourceForFact("nope"); ok {
		t.Fatal("unknown fact must not resolve")
	}
	all := s.Sources()
	if len(all) != 2 || all[0].SourceURL != srcURL || all[1].SourceURL != hoursURL || len(all[0].Facts) != 2 {
		t.Fatalf("Sources() = %+v", all)
	}
}

func TestNewScheduleValidates(t *testing.T) {
	facts := testPol(t).Facts
	if _, err := NewSchedule(Config{Floor: 0}, facts); err == nil {
		t.Fatal("a zero floor must be rejected")
	}
	if _, err := NewSchedule(Config{Floor: time.Minute, Max: time.Second}, facts); err == nil {
		t.Fatal("max below floor must be rejected")
	}
	if _, err := NewSchedule(Config{Floor: time.Second, Max: time.Minute, Growth: 1, Adaptive: true}, facts); err == nil {
		t.Fatal("growth <= 1 with adaptive on must be rejected")
	}
	s, err := NewSchedule(Config{Floor: time.Second, Max: 0, Growth: 1, Adaptive: false}, facts)
	if err != nil || s.Config().Max != time.Second {
		t.Fatalf("fixed schedule: %v, max=%s (want = floor)", err, s.Config().Max)
	}
}
