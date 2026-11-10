package policy

import (
	"errors"
	"regexp"
	"strings"
)

// compileKeywords builds one case-insensitive matcher for a keyword list.
//
// Rules (deterministic, no stemming or model):
//   - matching ignores case;
//   - a keyword that starts or ends with a letter, digit or underscore must
//     sit on a word boundary there, so "pro" matches "Pro plan" but not
//     "product" (boundaries are ASCII-based);
//   - spaces inside a keyword match any run of whitespace ("how much");
//   - a trailing "*" matches any word suffix ("refund*" matches "refunds").
func compileKeywords(keywords []string) (*regexp.Regexp, error) {
	parts := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		kw = strings.TrimSpace(kw)
		wildcard := strings.HasSuffix(kw, "*")
		kw = strings.TrimSpace(strings.TrimSuffix(kw, "*"))
		if kw == "" {
			return nil, errors.New("keywords must not be empty")
		}
		words := strings.Fields(kw)
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		expr := strings.Join(words, `\s+`)
		if isWordByte(kw[0]) {
			expr = `\b` + expr
		}
		if wildcard {
			expr += `\w*`
		}
		if isWordByte(kw[len(kw)-1]) {
			expr += `\b`
		}
		parts = append(parts, expr)
	}
	return regexp.Compile(`(?i)(?:` + strings.Join(parts, "|") + `)`)
}

func isWordByte(b byte) bool {
	return b == '_' || ('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}
