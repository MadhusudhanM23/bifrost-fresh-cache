package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// chatMessage is the subset of an OpenAI chat message the mock reads.
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// messageText flattens an OpenAI message content (a string, or an array of
// content parts) into plain text. Non-text parts are ignored.
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, " ")
	}
	return ""
}

// listMarker matches the bullet or number that starts a list item line:
// "- ", "* ", "• ", "1. ", "1) ", "[1] ".
var listMarker = regexp.MustCompile(`^(?:[-*•]|\d+[.)]|\[\d+\])\s+`)

// contextHeader matches a "Context:" heading line, with optional inline text.
var contextHeader = regexp.MustCompile(`(?i)^context\s*:\s*(.*)$`)

// contextLines extracts the context lines of a system prompt, in order:
//   - every list-item line ("- ...", "* ...", "1. ...", "[1] ..."), marker removed;
//   - every non-empty line after a "Context:" heading (and the heading's inline text).
func contextLines(system string) []string {
	var lines []string
	afterHeader := false
	for _, line := range strings.Split(system, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := contextHeader.FindStringSubmatch(line); m != nil {
			afterHeader = true
			if rest := strings.TrimSpace(m[1]); rest != "" {
				lines = append(lines, stripListMarker(rest))
			}
			continue
		}
		if loc := listMarker.FindStringIndex(line); loc != nil {
			if item := strings.TrimSpace(line[loc[1]:]); item != "" {
				lines = append(lines, item)
			}
			continue
		}
		if afterHeader {
			lines = append(lines, line)
		}
	}
	return lines
}

func stripListMarker(line string) string {
	if loc := listMarker.FindStringIndex(line); loc != nil {
		return strings.TrimSpace(line[loc[1]:])
	}
	return line
}

// splitConversation returns the joined system/developer prompt and the last
// user message of a conversation.
func splitConversation(messages []chatMessage) (system string, question string) {
	var systemParts []string
	for _, m := range messages {
		switch m.Role {
		case "system", "developer":
			if t := messageText(m.Content); t != "" {
				systemParts = append(systemParts, t)
			}
		case "user":
			question = messageText(m.Content)
		}
	}
	return strings.Join(systemParts, "\n"), strings.TrimSpace(question)
}

// Answer is the mock's deterministic reply:
//   - no context lines in the system prompt: "Mock answer to: <question>";
//   - context lines present: "According to the provided information, <first
//     context line sharing a content word with the question>";
//   - context lines present but none shares a content word:
//     "The provided information does not answer: <question>".
func Answer(system, question string) string {
	lines := contextLines(system)
	if len(lines) == 0 {
		return "Mock answer to: " + question
	}
	questionWords := make(map[string]struct{})
	for _, w := range contentWords(question) {
		questionWords[w] = struct{}{}
	}
	for _, line := range lines {
		for _, w := range contentWords(line) {
			if _, ok := questionWords[w]; ok {
				return "According to the provided information, " + line
			}
		}
	}
	return "The provided information does not answer: " + question
}

// answerWithKnowledge is Answer with the rules of a knowledge file: when the
// system prompt has no context lines and a rule matches the question, the reply
// is that rule's answer, verbatim, and rule is its name. Otherwise the reply is
// Answer's and rule is "". With no rules it is exactly Answer.
func answerWithKnowledge(rules []knowledgeRule, system, question string) (answer, rule string) {
	if len(rules) > 0 && len(contextLines(system)) == 0 {
		if r, ok := matchKnowledge(rules, question); ok {
			return r.Answer, r.Name
		}
	}
	return Answer(system, question), ""
}

// knowledgeFile is the JSON shape of a -knowledge file: canned answers that make
// the mock behave like a model that knows a few facts. Questions that mean the
// same thing get the identical answer; look-alike questions that mean different
// things ("turn on" vs "turn off" two-factor login) get different answers.
type knowledgeFile struct {
	Comment string          `json:"_comment"`
	Rules   []knowledgeRule `json:"rules"`
}

// knowledgeRule is one canned answer. It matches a question whose tokens include
// every All word, at least one Any word (when Any is set) and no None word.
type knowledgeRule struct {
	Comment string   `json:"_comment"`
	Name    string   `json:"name"`
	All     []string `json:"all"`
	Any     []string `json:"any"`
	None    []string `json:"none"`
	Answer  string   `json:"answer"`
}

// loadKnowledge reads and validates a knowledge file (see parseKnowledge).
func loadKnowledge(path string) ([]knowledgeRule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rules, err := parseKnowledge(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rules, nil
}

// parseKnowledge decodes and validates a knowledge file and returns its rules in
// file order, with their words lowercased and trimmed. Unknown fields are
// rejected so a typo cannot silently disable a rule, and so is a rule that could
// never match as written.
func parseKnowledge(data []byte) ([]knowledgeRule, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var file knowledgeFile
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("invalid knowledge JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid knowledge JSON: unexpected data after the top-level object")
	}
	if len(file.Rules) == 0 {
		return nil, errors.New(`no rules: want {"rules": [{"name": "...", "all": ["..."], "answer": "..."}]}`)
	}
	names := make(map[string]struct{}, len(file.Rules))
	for i := range file.Rules {
		r := &file.Rules[i]
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" {
			return nil, fmt.Errorf("rules[%d]: name is required", i)
		}
		if _, dup := names[r.Name]; dup {
			return nil, fmt.Errorf("rule %q is defined twice", r.Name)
		}
		names[r.Name] = struct{}{}
		if err := r.normalise(); err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.Name, err)
		}
	}
	return file.Rules, nil
}

// normalise lowercases and trims the rule's words in place, and checks that the
// rule has an answer and can match.
func (r *knowledgeRule) normalise() error {
	if strings.TrimSpace(r.Answer) == "" {
		return errors.New("answer is required")
	}
	if len(r.All) == 0 && len(r.Any) == 0 {
		return errors.New(`at least one word in "all" or "any" is required`)
	}
	if err := normaliseWords("all", r.All); err != nil {
		return err
	}
	if err := normaliseWords("any", r.Any); err != nil {
		return err
	}
	if err := normaliseWords("none", r.None); err != nil {
		return err
	}
	for _, w := range r.None {
		if slices.Contains(r.All, w) {
			return fmt.Errorf(`word %q is in both "all" and "none"`, w)
		}
		if slices.Contains(r.Any, w) {
			return fmt.Errorf(`word %q is in both "any" and "none"`, w)
		}
	}
	return nil
}

// normaliseWords lowercases and trims words in place. Each must then be a single
// token, or it could never equal a token of a question.
func normaliseWords(field string, words []string) error {
	for i, w := range words {
		word := strings.ToLower(strings.TrimSpace(w))
		if toks := tokenize(word); len(toks) != 1 || toks[0] != word {
			return fmt.Errorf("word %q in %q is not a single token (use only letters and digits, one word per entry)", w, field)
		}
		words[i] = word
	}
	return nil
}

// matchKnowledge returns the first rule, in file order, that matches question.
// It uses every token of the question, stopwords included, so that "on" and
// "off" (which the embedding ignores) can select different rules.
func matchKnowledge(rules []knowledgeRule, question string) (knowledgeRule, bool) {
	tokens := make(map[string]struct{})
	for _, tok := range tokenize(question) {
		tokens[tok] = struct{}{}
	}
	for i := range rules {
		if rules[i].matches(tokens) {
			return rules[i], true
		}
	}
	return knowledgeRule{}, false
}

// matches reports whether tokens hold every All word, at least one Any word
// (when Any is set) and no None word.
func (r *knowledgeRule) matches(tokens map[string]struct{}) bool {
	has := func(word string) bool {
		_, ok := tokens[word]
		return ok
	}
	for _, w := range r.All {
		if !has(w) {
			return false
		}
	}
	for _, w := range r.None {
		if has(w) {
			return false
		}
	}
	if len(r.Any) == 0 {
		return true
	}
	for _, w := range r.Any {
		if has(w) {
			return true
		}
	}
	return false
}
