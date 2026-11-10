// Package verified holds a table of known answers curated by a person. It is
// a lookup table in front of the cache, not part of it: a question is matched
// exactly (after lower-casing and dropping punctuation and extra spaces), with
// no similarity matching.
package verified

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// Limits that keep a bad file from eating memory.
const (
	MaxFileBytes   = 1 << 20 // 1 MiB
	MaxEntries     = 5000
	MaxAnswerBytes = 8 << 10
)

// Entry is one curated question and answer.
type Entry struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	// Label is a short name shown in a header and in listings.
	Label string `json:"label,omitempty"`
	// Source says where a person got the answer (never sent to callers).
	Source string `json:"source,omitempty"`
	// Scope limits the entry to requests whose scope header has this exact
	// value (for example one customer). Empty = for everyone.
	Scope string `json:"scope,omitempty"`

	// ID is a short hash of the normalised question and scope. It is safe to
	// show: it does not reveal the question.
	ID string `json:"-"`
}

// Normalize lower-cases s and keeps only letters and digits, joined by single
// spaces: "  What's the PRICE?! " -> "what s the price".
func Normalize(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// Set is a validated, immutable group of entries.
type Set struct {
	entries []Entry
	byKey   map[string]int
}

func key(norm, scope string) string { return norm + "\x00" + scope }

func idOf(norm, scope string) string {
	sum := sha256.Sum256([]byte(key(norm, scope)))
	return hex.EncodeToString(sum[:6])
}

// Parse validates a file's bytes. Errors name the entry number and the field,
// never the text.
func Parse(data []byte) (*Set, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("verified answers file is %d bytes; the limit is %d", len(data), MaxFileBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var entries []Entry
	if err := dec.Decode(&entries); err != nil {
		return nil, errors.New("verified answers file must be a JSON array of {question, answer, label, source, scope}: " + firstLine(err.Error()))
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("verified answers file has extra content after the array")
	}
	if len(entries) > MaxEntries {
		return nil, fmt.Errorf("verified answers file has %d entries; the limit is %d", len(entries), MaxEntries)
	}
	s := &Set{entries: make([]Entry, 0, len(entries)), byKey: make(map[string]int, len(entries))}
	for i, e := range entries {
		n := i + 1
		e.Question, e.Answer = strings.TrimSpace(e.Question), strings.TrimSpace(e.Answer)
		e.Label, e.Source, e.Scope = strings.TrimSpace(e.Label), strings.TrimSpace(e.Source), strings.TrimSpace(e.Scope)
		if e.Question == "" {
			return nil, fmt.Errorf("entry %d: question is empty", n)
		}
		if e.Answer == "" {
			return nil, fmt.Errorf("entry %d: answer is empty", n)
		}
		if len(e.Answer) > MaxAnswerBytes {
			return nil, fmt.Errorf("entry %d: answer is %d bytes; the limit is %d", n, len(e.Answer), MaxAnswerBytes)
		}
		norm := Normalize(e.Question)
		if norm == "" {
			return nil, fmt.Errorf("entry %d: question has no letters or digits", n)
		}
		k := key(norm, e.Scope)
		if first, dup := s.byKey[k]; dup {
			return nil, fmt.Errorf("entry %d: same question (after lower-casing and dropping punctuation) and scope as entry %d", n, first+1)
		}
		e.ID = idOf(norm, e.Scope)
		s.byKey[k] = len(s.entries)
		s.entries = append(s.entries, e)
	}
	return s, nil
}

// Len is the number of entries.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.entries)
}

// Entries returns a copy of the entries.
func (s *Set) Entries() []Entry {
	if s == nil {
		return nil
	}
	return append([]Entry(nil), s.entries...)
}

// Lookup finds the entry for a question. An entry with a scope matches only
// that scope; one without matches everyone. A scoped match wins.
func (s *Set) Lookup(question, scope string) (Entry, bool) {
	if s == nil {
		return Entry{}, false
	}
	norm := Normalize(question)
	if norm == "" {
		return Entry{}, false
	}
	if scope != "" {
		if i, ok := s.byKey[key(norm, scope)]; ok {
			return s.entries[i], true
		}
	}
	if i, ok := s.byKey[key(norm, "")]; ok {
		return s.entries[i], true
	}
	return Entry{}, false
}

type state struct {
	set      *Set
	loadedAt time.Time
	mtime    time.Time
	size     int64
	lastErr  string
}

// Store keeps the current Set and reloads it when the file changes.
type Store struct {
	path string
	log  *slog.Logger
	cur  atomic.Pointer[state]
}

// Open loads path. A bad file is an error (the server should not start).
func Open(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	st := &Store{path: path, log: log}
	next, err := st.read()
	if err != nil {
		return nil, fmt.Errorf("-verified %s: %w", path, err)
	}
	st.cur.Store(next)
	return st, nil
}

func (s *Store) read() (*state, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	set, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return &state{set: set, loadedAt: time.Now(), mtime: info.ModTime(), size: info.Size()}, nil
}

// Set returns the current entries.
func (s *Store) Set() *Set { return s.cur.Load().set }

// Info reports when the entries were loaded and the last reload error.
func (s *Store) Info() (loadedAt time.Time, lastErr string) {
	c := s.cur.Load()
	return c.loadedAt, c.lastErr
}

// Lookup finds the entry for a question.
func (s *Store) Lookup(question, scope string) (Entry, bool) {
	return s.Set().Lookup(question, scope)
}

// Reload re-reads the file if its modification time or size changed. A file
// that no longer validates is rejected: the previous entries stay in use and
// the error is shown by Info. It reports whether new entries were loaded.
func (s *Store) Reload() bool {
	cur := s.cur.Load()
	info, err := os.Stat(s.path)
	if err != nil {
		s.fail(cur, "cannot read the file: "+firstLine(err.Error()))
		return false
	}
	if info.ModTime().Equal(cur.mtime) && info.Size() == cur.size {
		return false
	}
	next, err := s.read()
	if err != nil {
		// Remember the version we failed on so the same bad file is not
		// re-read (and re-logged) every tick.
		bad := *cur
		bad.mtime, bad.size = info.ModTime(), info.Size()
		bad.lastErr = firstLine(err.Error())
		s.cur.Store(&bad)
		s.log.Warn("verified answers file rejected; the previous entries stay in use", "error", bad.lastErr)
		return false
	}
	s.cur.Store(next)
	s.log.Info("verified answers reloaded", "entries", next.set.Len())
	return true
}

func (s *Store) fail(cur *state, msg string) {
	if cur.lastErr == msg {
		return
	}
	bad := *cur
	bad.lastErr = msg
	s.cur.Store(&bad)
	s.log.Warn("verified answers file not reloaded", "error", msg)
}

// Watch polls the file every interval until done is closed. (A signal such as
// SIGHUP does not exist on Windows, so a poll is used everywhere.)
func (s *Store) Watch(done <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.Reload()
		}
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
