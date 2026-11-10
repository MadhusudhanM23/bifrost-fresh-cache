package discover

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"freshness-companion/internal/extract"
)

// EntityPattern builds the page pattern for a discovered source: one of the
// entity names (whole words, any case), then at most near characters on the
// same line with no digit among them, then the value pattern. For "Acme Pro"
// and a currency value pattern, "Acme Pro plan — $49 per month" yields 49.
// A page that states two different values next to the name (a pricing table)
// makes extract report a conflict, which is unable_to_verify, never confirmed.
func EntityPattern(entities []string, valuePattern string, near int) (*regexp.Regexp, error) {
	if len(entities) == 0 {
		return nil, errors.New("no entity names")
	}
	if near < 0 {
		near = 0
	}
	alts := make([]string, 0, len(entities))
	for _, e := range entities {
		words := strings.Fields(e)
		if len(words) == 0 {
			continue
		}
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		alt := strings.Join(words, `\s+`)
		if r := []rune(e); isWordRune(r[0]) {
			alt = `\b` + alt
		}
		if r := []rune(strings.TrimSpace(e)); isWordRune(r[len(r)-1]) {
			alt += `\b`
		}
		alts = append(alts, alt)
	}
	if len(alts) == 0 {
		return nil, errors.New("no entity names")
	}
	expr := fmt.Sprintf(`(?i)(?:%s)[^0-9\n]{0,%d}(?:%s)`, strings.Join(alts, "|"), near, valuePattern)
	return extract.CompilePattern(expr)
}
