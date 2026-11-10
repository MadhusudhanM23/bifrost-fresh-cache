// Package extract pulls one comparable value out of text with a policy
// regex and compares two values deterministically: no model, no fuzziness.
package extract

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Mode selects how captured values are normalised and compared.
type Mode string

const (
	// Exact compares text after trimming, collapsing internal whitespace and
	// lower-casing, so "Monday 9am" equals " monday  9AM ".
	Exact Mode = "exact"
	// Numeric compares numbers, ignoring currency symbols and thousands
	// separators, so "$1,299.00" equals "1299" and "49.00" equals "49".
	Numeric Mode = "numeric"
	// Boolean reduces the captured text to "true" or "false" by scanning it
	// for a small, fixed set of true-signal and false-signal words/phrases
	// (word-boundary matched, case-insensitive), so "Yes, live chat is
	// available" and "live chat support is currently unavailable" compare
	// deterministically without any NLP model. False signals are checked
	// first, so a negation ("not available", "no longer available",
	// "unavailable") is never mistaken for the positive word it contains.
	Boolean Mode = "boolean"
)

// Valid reports whether m is a supported compare mode.
func (m Mode) Valid() bool { return m == Exact || m == Numeric || m == Boolean }

// maxMatches bounds how many matches are examined when looking for conflicts.
const maxMatches = 64

// ErrNoMatch is returned when the pattern captures nothing in the text.
var ErrNoMatch = errors.New("pattern matched nothing")

// ConflictError is returned when the pattern captures different values in
// the same text, so the text does not state one value unambiguously.
type ConflictError struct {
	Values []string
}

func (e *ConflictError) Error() string {
	return "conflicting values: " + strings.Join(e.Values, ", ")
}

// Value is one extracted value.
type Value struct {
	// Raw is the first captured text, trimmed. Headers and logs show it.
	Raw string
	// Normalized is the canonical form used to detect conflicts.
	Normalized string
	// Number is the parsed number (Numeric mode only); for Boolean mode it is
	// 1 for true and 0 for false, purely informational.
	Number float64
	// Matches counts the pattern matches that agreed on the value.
	Matches int
	// Match is the whole text the first match covered, trimmed, e.g.
	// "Acme Pro plan — $49 per month" for a capture of "49". A correction
	// quotes it to the model, so the value keeps the source's own wording.
	Match string
}

// CompilePattern compiles a policy pattern and requires exactly one capture
// group: the value. Use (?:...) for any other grouping.
func CompilePattern(expr string) (*regexp.Regexp, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, errors.New("pattern is empty")
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	if n := re.NumSubexp(); n != 1 {
		return nil, fmt.Errorf("pattern must have exactly one capture group, it has %d (use (?:...) for other groups)", n)
	}
	return re, nil
}

// Extract applies re to text and returns the single value the text states.
// It returns ErrNoMatch when nothing is captured and a *ConflictError when
// the captures disagree after normalisation.
func Extract(re *regexp.Regexp, text string, mode Mode) (Value, error) {
	if re == nil {
		return Value{}, errors.New("no pattern")
	}
	if !mode.Valid() {
		return Value{}, fmt.Errorf("unknown compare mode %q", mode)
	}
	var out Value
	var distinct []string
	seen := make(map[string]bool)
	for _, m := range re.FindAllStringSubmatch(text, maxMatches) {
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(m[1])
		if raw == "" {
			continue
		}
		norm, num, err := normalize(raw, mode)
		if err != nil {
			return Value{}, fmt.Errorf("captured %q is not a valid %s value: %w", truncate(raw, 64), mode, err)
		}
		if out.Matches == 0 {
			out = Value{Raw: truncate(raw, 200), Normalized: norm, Number: num, Match: truncate(strings.TrimSpace(m[0]), 200)}
		}
		out.Matches++
		if !seen[norm] {
			seen[norm] = true
			distinct = append(distinct, truncate(norm, 64))
		}
	}
	if out.Matches == 0 {
		return Value{}, ErrNoMatch
	}
	if len(distinct) > 1 {
		return Value{}, &ConflictError{Values: distinct}
	}
	return out, nil
}

// Equal reports whether a and b are the same value under mode.
func Equal(a, b Value, mode Mode) bool {
	if mode == Numeric {
		diff := math.Abs(a.Number - b.Number)
		scale := math.Max(1, math.Max(math.Abs(a.Number), math.Abs(b.Number)))
		return diff <= 1e-9*scale
	}
	return a.Normalized == b.Normalized
}

func normalize(raw string, mode Mode) (string, float64, error) {
	switch mode {
	case Numeric:
		n, err := ParseNumber(raw)
		if err != nil {
			return "", 0, err
		}
		return strconv.FormatFloat(n, 'f', -1, 64), n, nil
	case Boolean:
		return ParseBoolean(raw)
	default:
		return strings.ToLower(strings.Join(strings.Fields(raw), " ")), 0, nil
	}
}

// falseSignalRE and trueSignalRE are small, fixed, word-boundary-matched
// word/phrase lists, deliberately not an NLP classifier. falseSignalRE is
// always checked first: most of its phrases are negations of a positive word
// ("not available", "unavailable", "no longer supported"), so checking it
// first means the positive word inside a negation is never matched by
// trueSignalRE. Longer, more specific phrases are listed before the shorter
// words they contain (e.g. "no longer available" before "no").
var (
	falseSignalRE = regexp.MustCompile(`(?i)\b(?:no\s+longer\s+available|not\s+available|no\s+longer\s+supported|not\s+supported|unavailable|unsupported|cannot|can['’]t|does\s+not|doesn['’]t|is\s+not|isn['’]t|was\s+not|wasn['’]t|out\s+of\s+stock|sold\s+out|offline|disabled|inactive|closed|false|no)\b`)
	trueSignalRE  = regexp.MustCompile(`(?i)\b(?:available|supported|online|enabled|active|open|in\s+stock|true|yes|can)\b`)
)

// ErrNoSignal is returned when the captured text matches neither a
// true-signal nor a false-signal word or phrase.
var ErrNoSignal = errors.New("no yes/no signal word found")

// ParseBoolean reduces raw to "true" or "false" (as the Normalized value,
// with Number 1 or 0) by scanning it for the fixed signal words above.
// False signals are checked first (see falseSignalRE).
func ParseBoolean(raw string) (string, float64, error) {
	switch {
	case falseSignalRE.MatchString(raw):
		return "false", 0, nil
	case trueSignalRE.MatchString(raw):
		return "true", 1, nil
	default:
		return "", 0, fmt.Errorf("%w in %q", ErrNoSignal, truncate(raw, 64))
	}
}

var numberToken = regexp.MustCompile(`[-+]?[0-9][0-9.,]*`)

// ParseNumber reads the first number in s, ignoring anything around it
// (currency symbols, units). Separator rules:
//   - both "." and "," present: the later one is the decimal separator
//     ("1,299.50" and "1.299,50" are both 1299.5);
//   - only ",": one comma followed by exactly 3 digits, or several commas,
//     are thousands separators ("1,299", "1,234,567"); otherwise it is a
//     decimal comma ("49,00", "49,5");
//   - only ".": several dots are thousands separators ("1.234.567"); a
//     single dot is always a decimal point ("1.299" is 1.299).
func ParseNumber(s string) (float64, error) {
	tok := strings.TrimRight(numberToken.FindString(s), ".,")
	if tok == "" {
		return 0, errors.New("no number found")
	}
	sign := ""
	if tok[0] == '-' || tok[0] == '+' {
		sign, tok = tok[:1], tok[1:]
	}
	dot, comma := strings.LastIndex(tok, "."), strings.LastIndex(tok, ",")
	switch {
	case dot >= 0 && comma >= 0:
		if dot > comma {
			tok = strings.ReplaceAll(tok, ",", "")
		} else {
			tok = strings.Replace(strings.ReplaceAll(tok, ".", ""), ",", ".", 1)
		}
	case comma >= 0:
		if parts := strings.Split(tok, ","); len(parts) == 2 && len(parts[1]) != 3 {
			tok = parts[0] + "." + parts[1]
		} else {
			tok = strings.ReplaceAll(tok, ",", "")
		}
	case dot >= 0:
		if strings.Count(tok, ".") > 1 {
			tok = strings.ReplaceAll(tok, ".", "")
		}
	}
	n, err := strconv.ParseFloat(sign+tok, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q as a number", truncate(s, 64))
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, errors.New("number out of range")
	}
	return n, nil
}

// truncate shortens s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
