package main

import (
	"hash/fnv"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file implements the mock's embedding: a deterministic bag-of-words
// feature-hashing vector. It is NOT a semantic model. Two texts score as
// similar only when they share content words (or most of their letters, via
// character trigrams). Synonyms, word order and meaning are invisible to it.

const (
	// wordWeight is the weight of a whole content word.
	wordWeight = 1.0
	// trigramWeight is the weight of each character trigram of a content word
	// (the word is padded as "<word>"). Trigrams make "capital" and "capitals"
	// close without making unrelated words close.
	trigramWeight = 0.5
)

// stopwords is a small list of English function words. Role labels that
// Bifrost's semantic cache prepends to every message before embedding it
// ("user: ...", "system: ...") are included, so that every chat prompt does
// not share a feature just because it has a user message.
var stopwords = func() map[string]struct{} {
	words := strings.Fields(`
		a about above after again against all am an and any are as at
		be because been before being below between both but by
		can could did do does doing down during each few for from further
		had has have having he her here hers herself him himself his how
		i if in into is it its itself just me more most my myself
		no nor not now of off on once only or other our ours out over own
		same she should so some such than that the their theirs them then
		there these they this those through to too under until up very
		was we were what when where which while who whom why will with would
		you your yours yourself
		user system assistant developer`)
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}()

// tokenize lowercases text and splits it into maximal runs of letters and digits.
func tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func isNumeric(tok string) bool {
	for _, r := range tok {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return tok != ""
}

// isStopword reports whether a token carries no content: a listed function word,
// or a single letter (the "s" of "what's").
func isStopword(tok string) bool {
	if _, ok := stopwords[tok]; ok {
		return true
	}
	return utf8.RuneCountInString(tok) < 2 && !isNumeric(tok)
}

// contentWords returns the tokens of text that are not stopwords, in order.
func contentWords(text string) []string {
	var out []string
	for _, tok := range tokenize(text) {
		if !isStopword(tok) {
			out = append(out, tok)
		}
	}
	return out
}

// countTokens is the mock's token count: the number of word tokens, at least 1.
func countTokens(text string) int {
	if n := len(tokenize(text)); n > 0 {
		return n
	}
	return 1
}

// featureHash maps a feature to an index in [0, dim) and a sign in {-1, +1}.
func featureHash(feature string, dim int) (int, float64) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(feature))
	sum := h.Sum64()
	sign := 1.0
	if sum>>63 == 1 {
		sign = -1.0
	}
	return int(sum % uint64(dim)), sign
}

// Embed returns the L2-normalised hashing embedding of text with dim dimensions.
// The same text and dim always give a bit-identical vector: features are added
// in a fixed order (never by ranging over a map), so float rounding is stable.
func Embed(text string, dim int) []float64 {
	if dim <= 0 {
		dim = 1
	}
	vec := make([]float64, dim)
	add := func(feature string, weight float64) {
		idx, sign := featureHash(feature, dim)
		vec[idx] += sign * weight
	}

	words := contentWords(text)
	if len(words) == 0 {
		// Only stopwords ("what is it?"): use every token so the vector is not zero.
		words = tokenize(text)
	}
	for _, w := range words {
		add("w:"+w, wordWeight)
		padded := []rune("<" + w + ">")
		for i := 0; i+3 <= len(padded); i++ {
			add("c:"+string(padded[i:i+3]), trigramWeight)
		}
	}
	if len(words) == 0 {
		// No letters or digits at all (for example "?!"): use the raw characters.
		raw := []rune(strings.TrimSpace(text))
		for i := range raw {
			end := i + 3
			if end > len(raw) {
				end = len(raw)
			}
			add("r:"+string(raw[i:end]), trigramWeight)
		}
	}

	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		// Every feature cancelled out (or the text was empty): fall back to a fixed
		// unit vector derived from the text, so the result is never a zero vector.
		idx, sign := featureHash("fallback:"+text, dim)
		vec[idx] = sign
		return vec
	}
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

// Cosine returns the cosine similarity of two vectors of equal length.
func Cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}
