// Package discover is "Tavily finds the source": when no watched fact covers
// a time-sensitive cached answer, it builds a short search query from the
// policy's template and the product names found in the question, asks Tavily
// Search for a page, and picks one result the caller can check the answer
// against.
//
// What leaves the companion is exactly the derived query (at most 80
// characters, refused when it or the question holds anything the sensitive
// detectors recognise) plus the policy's domain lists. The question, the
// answer, system messages, headers and cache keys are never sent. The query
// still reveals the topic, and proper-noun mode can send a person's name,
// which is why that mode is opt-in.
package discover

import (
	"errors"
	"regexp"
	"strings"
	"unicode"

	"freshness-companion/internal/policy"
	"freshness-companion/internal/sensitive"
)

// MaxQueryLen bounds the derived query.
const MaxQueryLen = 80

// Errors. Reason turns them into the short words shown in headers and logs.
var (
	ErrNoEntities   = errors.New("no allowlisted name appears in the question, so there is nothing to search for")
	ErrSensitive    = errors.New("the question or the query contains text that looks sensitive, so nothing was searched")
	ErrQueryTooLong = errors.New("the derived query is longer than 80 characters, so nothing was searched")
	ErrOverCap      = errors.New("the hourly Tavily Search budget for discovery is used up")
	ErrNoSource     = errors.New("search found no usable page")
	ErrSearchFailed = errors.New("Tavily Search failed")
	ErrSkipped      = errors.New("discovery does not apply")
)

// Reason returns the one-word reason for a Find error: no_entities,
// sensitive, query_too_long, over_cap, no_source, search_failed or skipped.
func Reason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoEntities):
		return "no_entities"
	case errors.Is(err, ErrSensitive):
		return "sensitive"
	case errors.Is(err, ErrQueryTooLong):
		return "query_too_long"
	case errors.Is(err, ErrOverCap):
		return "over_cap"
	case errors.Is(err, ErrNoSource):
		return "no_source"
	case errors.Is(err, ErrSkipped):
		return "skipped"
	}
	return "search_failed"
}

// BuildQuery derives the search query from the policy template and the
// question. The question is only read here; it is never returned or stored.
// entities are the names that went into the query.
func BuildQuery(d *policy.Discover, question string) (q string, entities []string, err error) {
	if d == nil {
		return "", nil, ErrSkipped
	}
	if d.QueryFrom == policy.QueryFromProperNouns {
		entities = properNouns(question)
	} else {
		entities = allowlisted(d.Entities, question)
	}
	if len(entities) == 0 {
		return "", nil, ErrNoEntities
	}
	q = strings.Join(strings.Fields(strings.ReplaceAll(d.Query, "{entities}", strings.Join(entities, " "))), " ")
	if len(q) > MaxQueryLen {
		return "", entities, ErrQueryTooLong
	}
	if sensitive.FindAny([]string{q, question}, nil) != "" {
		return "", entities, ErrSensitive
	}
	return q, entities, nil
}

// NormalizeKey is the store key of a query: category, NUL, the query in lower
// case with single spaces.
func NormalizeKey(category, q string) string {
	return category + "\x00" + strings.ToLower(strings.Join(strings.Fields(q), " "))
}

// wordsPattern matches the words of name, in order, as whole words, any case.
func wordsPattern(name string) *regexp.Regexp {
	words := strings.Fields(name)
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	expr := strings.Join(words, `\s+`)
	if r := []rune(name); len(r) > 0 && isWordRune(r[0]) {
		expr = `\b` + expr
	}
	if r := []rune(name); len(r) > 0 && isWordRune(r[len(r)-1]) {
		expr += `\b`
	}
	return regexp.MustCompile(`(?i)` + expr)
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// allowlisted returns the operator-written names that appear in the question,
// in policy order, spelled as the operator wrote them.
func allowlisted(names []string, question string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		key := strings.ToLower(n)
		if seen[key] {
			continue
		}
		if wordsPattern(n).MatchString(question) {
			seen[key] = true
			out = append(out, n)
		}
	}
	return out
}

// stopwords are capitalised words that are not names: question words and
// common sentence starters.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "i": true, "i'm": true, "im": true, "my": true, "me": true, "we": true, "our": true, "you": true, "your": true,
	"how": true, "what": true, "what's": true, "whats": true, "when": true, "where": true, "which": true, "who": true, "whom": true, "why": true,
	"is": true, "are": true, "was": true, "were": true, "do": true, "does": true, "did": true, "can": true, "could": true, "will": true, "would": true,
	"should": true, "shall": true, "may": true, "might": true, "has": true, "have": true, "had": true, "tell": true, "please": true, "hi": true,
	"hello": true, "hey": true, "thanks": true, "thank": true, "and": true, "or": true, "but": true, "if": true, "in": true, "on": true, "at": true,
	"for": true, "of": true, "to": true, "from": true, "with": true, "about": true, "this": true, "that": true, "these": true, "those": true,
	"it": true, "its": true, "it's": true, "there": true, "any": true, "much": true, "many": true, "price": true, "cost": true, "plan": true,
	"ok": true, "okay": true, "yes": true, "no": true, "also": true, "now": true, "today": true, "currently": true,
}

// properNouns returns at most two runs of capitalised words (at most three
// words each), skipping stopwords and any word that contains a digit.
func properNouns(question string) []string {
	var runs [][]string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			if len(cur) > 3 {
				cur = cur[:3]
			}
			runs = append(runs, cur)
			cur = nil
		}
	}
	for _, field := range strings.Fields(question) {
		// Punctuation at the end of a word ends a run after it; punctuation
		// at the start ends the run before it.
		lead := strings.IndexFunc(field, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
		if lead > 0 {
			flush()
		}
		word := strings.TrimFunc(field, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' })
		word = strings.Trim(word, "'")
		endsRun := field != "" && strings.ContainsRune(",.;:!?()[]\"", rune(field[len(field)-1]))
		r := []rune(word)
		capital := len(r) > 0 && unicode.IsUpper(r[0])
		hasDigit := strings.IndexFunc(word, unicode.IsDigit) >= 0
		if !capital || hasDigit || stopwords[strings.ToLower(word)] {
			flush()
		} else {
			cur = append(cur, word)
		}
		if endsRun {
			flush()
		}
		if len(runs) >= 2 {
			break
		}
	}
	flush()
	if len(runs) > 2 {
		runs = runs[:2]
	}
	out := make([]string, 0, len(runs))
	for _, run := range runs {
		out = append(out, strings.Join(run, " "))
	}
	return out
}
