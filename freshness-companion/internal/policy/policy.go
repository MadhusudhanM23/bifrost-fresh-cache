// Package policy loads the freshness policy: which prompts are
// time-sensitive (categories with keywords), how old a cached answer may be
// without evidence (max_age), what to do when it cannot be verified, and
// which "watched facts" can be checked against a source page.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"freshness-companion/internal/extract"
	"freshness-companion/internal/sensitive"
)

// OnUnverifiable is what to do with a time-sensitive cached answer whose
// freshness could not be verified.
type OnUnverifiable string

const (
	// UnverifiableBypass does not serve the cached answer; the companion
	// refreshes it (clear + re-request) and returns the fresh answer.
	UnverifiableBypass OnUnverifiable = "bypass"
	// UnverifiableServeWithWarning serves the cached answer with an
	// x-freshness-warning header.
	UnverifiableServeWithWarning OnUnverifiable = "serve_with_warning"
)

func (o OnUnverifiable) valid() bool {
	return o == UnverifiableBypass || o == UnverifiableServeWithWarning
}

// OnUnknown is what to do when the companion has no reliable metadata about
// a cached entry (its age is unknown).
type OnUnknown string

const (
	// UnknownAuto revalidates against evidence when a watched fact applies,
	// otherwise serves with a warning. It is the default.
	UnknownAuto OnUnknown = "auto"
	// UnknownRevalidate always tries the evidence check.
	UnknownRevalidate OnUnknown = "revalidate"
	// UnknownServeWithWarning serves without checking, with a warning.
	UnknownServeWithWarning OnUnknown = "serve_with_warning"
	// UnknownBypass refreshes without checking.
	UnknownBypass OnUnknown = "bypass"
)

func (o OnUnknown) valid() bool {
	switch o {
	case UnknownAuto, UnknownRevalidate, UnknownServeWithWarning, UnknownBypass:
		return true
	}
	return false
}

// unknownRank orders OnUnknown values from most lenient to strictest, for
// merging several matched categories.
func unknownRank(o OnUnknown) int {
	switch o {
	case UnknownServeWithWarning:
		return 0
	case UnknownAuto:
		return 1
	case UnknownRevalidate:
		return 2
	case UnknownBypass:
		return 3
	}
	return 1
}

// OnContradicted is what to do with a fresh model answer that states a
// different value than the evidence: a new answer checked on a cache miss
// (verify_new_answers), or the answer a refresh fetched.
type OnContradicted string

const (
	// ContradictedReport returns the answer labelled, and leaves it in the
	// cache, where its next hit is checked again. It is the default.
	ContradictedReport OnContradicted = "report"
	// ContradictedUncache returns the answer labelled and deletes its cache
	// entry, so a wrong answer is never reused for anyone else.
	ContradictedUncache OnContradicted = "uncache"
	// ContradictedCorrect deletes the entry and asks the model once more with
	// the verified current value added as a system message. A corrected
	// answer that matches the evidence is returned and cached; one that still
	// disagrees is handled as uncache.
	ContradictedCorrect OnContradicted = "correct"
)

func (o OnContradicted) valid() bool {
	return o == ContradictedReport || o == ContradictedUncache || o == ContradictedCorrect
}

// contradictedRank orders OnContradicted values from most lenient to most
// active, for merging several matched categories.
func contradictedRank(o OnContradicted) int {
	switch o {
	case ContradictedUncache:
		return 1
	case ContradictedCorrect:
		return 2
	}
	return 0
}

// DefaultMaxAge applies when neither the category nor the defaults set max_age.
const DefaultMaxAge = 10 * time.Minute

// Duration reads "10m"-style strings (Go duration syntax) or a number of
// seconds from JSON.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("invalid duration %q (use Go syntax like \"10m\", \"1h\", \"24h\")", s)
		}
		*d = Duration(v)
		return nil
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return errors.New(`a duration must be a string like "10m" or a number of seconds`)
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Defaults apply to every category that does not override them.
type Defaults struct {
	MaxAge         *Duration      `json:"max_age,omitempty"`
	OnUnverifiable OnUnverifiable `json:"on_unverifiable,omitempty"`
	OnUnknown      OnUnknown      `json:"on_unknown,omitempty"`
	// VerifyNewAnswers checks a fresh answer (a cache miss) against the
	// watched facts before it is reused. Off by default.
	VerifyNewAnswers *bool          `json:"verify_new_answers,omitempty"`
	OnContradicted   OnContradicted `json:"on_contradicted,omitempty"`
}

// Category is a kind of time-sensitive question, recognised by keywords in
// the last user message.
type Category struct {
	Comment          string         `json:"_comment,omitempty"`
	Name             string         `json:"name"`
	Keywords         []string       `json:"keywords"`
	MaxAge           *Duration      `json:"max_age,omitempty"`
	OnUnverifiable   OnUnverifiable `json:"on_unverifiable,omitempty"`
	OnUnknown        OnUnknown      `json:"on_unknown,omitempty"`
	VerifyNewAnswers *bool          `json:"verify_new_answers,omitempty"`
	OnContradicted   OnContradicted `json:"on_contradicted,omitempty"`

	matcher          *regexp.Regexp
	maxAge           time.Duration
	onUnverifiable   OnUnverifiable
	onUnknown        OnUnknown
	verifyNewAnswers bool
	onContradicted   OnContradicted
}

// Fact is a watched fact: a value stated on a source page that a cached
// answer in its category can be checked against.
type Fact struct {
	Comment  string `json:"_comment,omitempty"`
	Name     string `json:"name"`
	Category string `json:"category"`
	// Keywords optionally narrow the fact to prompts that mention it; with
	// no keywords the fact applies to every prompt in its category.
	Keywords []string `json:"keywords,omitempty"`
	// SourceURL is the only thing ever sent to the evidence provider.
	SourceURL string `json:"source_url"`
	// SourcePattern has one capture group: the current value in the source.
	SourcePattern string `json:"source_pattern"`
	// AnswerPattern has one capture group: the value in the cached answer.
	AnswerPattern string `json:"answer_pattern"`
	// Compare is "exact", "numeric" or "boolean".
	Compare extract.Mode `json:"compare"`

	matcher  *regexp.Regexp
	sourceRE *regexp.Regexp
	answerRE *regexp.Regexp
}

// SourceRegexp returns the compiled source_pattern.
func (f *Fact) SourceRegexp() *regexp.Regexp { return f.sourceRE }

// AnswerRegexp returns the compiled answer_pattern.
func (f *Fact) AnswerRegexp() *regexp.Regexp { return f.answerRE }

// Guard keeps requests whose answer must never be reused out of the cache
// (neither read from it nor written to it). Both rules are off by default.
type Guard struct {
	Comment string `json:"_comment,omitempty"`
	// ToolResults "bypass" keeps out requests that carry a tool's result: the
	// answer is built from live tool data, and the same words with different
	// data look alike to a similarity search. "allow" (default) leaves them.
	ToolResults string `json:"tool_results,omitempty"`
	// SideEffectTools lists tool names (shell-style patterns such as
	// "create_*", case-insensitive). A request that offers one of them is
	// kept out: replaying an old text answer would skip the action.
	SideEffectTools []string `json:"side_effect_tools,omitempty"`
	// Sensitive "bypass" keeps out any request whose messages (all roles)
	// contain an email address, card number, API key, SSN, IBAN or phone
	// number, so such text is never stored in a shared cache. "allow"
	// (default) leaves them.
	Sensitive string `json:"sensitive,omitempty"`
	// SensitiveKinds narrows Sensitive to some detectors (default: all):
	// email, card, api_key, jwt, bearer_token, ssn, iban, phone.
	SensitiveKinds []string `json:"sensitive_kinds,omitempty"`
}

// Guard reasons.
const (
	GuardToolResults = "tool_results"
	GuardSideEffect  = "side_effect_tool"
	// GuardSensitivePrefix starts the reason of a sensitive-text bypass.
	GuardSensitivePrefix = "sensitive:"
)

// Check returns why a request must bypass the cache ("" = it may use it).
func (g Guard) Check(hasToolResults bool, toolNames []string) string {
	if hasToolResults && g.ToolResults == "bypass" {
		return GuardToolResults
	}
	for _, name := range toolNames {
		for _, pattern := range g.SideEffectTools {
			if ok, _ := path.Match(strings.ToLower(pattern), strings.ToLower(name)); ok {
				return GuardSideEffect
			}
		}
	}
	return ""
}

// SensitiveOn reports whether the sensitive-text rule is on.
func (g Guard) SensitiveOn() bool { return g.Sensitive == "bypass" }

// CheckSensitive returns "sensitive:<kind>" when any text holds a sensitive
// kind ("" = none, or the rule is off). The reason names the kind only,
// never the text.
func (g Guard) CheckSensitive(texts []string) string {
	if !g.SensitiveOn() {
		return ""
	}
	if k := sensitive.FindAny(texts, sensitive.NewSet(g.SensitiveKinds)); k != "" {
		return GuardSensitivePrefix + k
	}
	return ""
}

func (g Guard) validate() error {
	switch g.Sensitive {
	case "", "allow", "bypass":
	default:
		return fmt.Errorf("guard.sensitive %q: want allow or bypass", g.Sensitive)
	}
	for _, k := range g.SensitiveKinds {
		if !sensitive.Valid(k) {
			return fmt.Errorf("guard.sensitive_kinds %q: want one of %s", k, strings.Join(sensitive.AllKinds, ", "))
		}
	}
	switch g.ToolResults {
	case "", "allow", "bypass":
	default:
		return fmt.Errorf("guard.tool_results %q: want allow or bypass", g.ToolResults)
	}
	for _, pattern := range g.SideEffectTools {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("guard.side_effect_tools %q: %w", pattern, err)
		}
	}
	return nil
}

// Policy is the whole policy file.
type Policy struct {
	Comment    string      `json:"_comment,omitempty"`
	Version    int         `json:"version"`
	Guard      Guard       `json:"guard"`
	Defaults   Defaults    `json:"defaults"`
	Categories []*Category `json:"categories"`
	Facts      []*Fact     `json:"facts"`
}

// Load reads and validates a policy file.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	return p, nil
}

// Parse decodes and validates a policy. Unknown fields are rejected so a
// typo cannot silently disable a check. When "categories" is absent the
// built-in defaults (price, policy, schedule, availability) are used.
func Parse(data []byte) (*Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("invalid policy JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid policy JSON: unexpected data after the policy object")
	}
	if p.Categories == nil {
		p.Categories = defaultCategories()
	}
	if err := p.compile(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Default returns the built-in policy: the four default categories and no
// watched facts.
func Default() *Policy {
	p := &Policy{Version: 1, Categories: defaultCategories()}
	if err := p.compile(); err != nil {
		panic("built-in policy is invalid: " + err.Error())
	}
	return p
}

func defaultCategories() []*Category {
	return []*Category{
		{Name: "price", Keywords: []string{"price", "prices", "pricing", "cost", "costs", "how much", "fee", "fees", "per month", "per year", "subscription", "plan"}},
		{Name: "policy", Keywords: []string{"policy", "policies", "refund", "refunds", "return policy", "terms", "warranty", "cancellation"}},
		{Name: "schedule", Keywords: []string{"schedule", "opening hours", "open today", "deadline", "timetable", "release date", "when does", "when is"}},
		{Name: "availability", Keywords: []string{"availability", "available", "in stock", "out of stock", "sold out", "backorder"}},
	}
}

func (p *Policy) compile() error {
	if p.Version != 0 && p.Version != 1 {
		return fmt.Errorf("unsupported policy version %d (want 1)", p.Version)
	}
	if err := p.Guard.validate(); err != nil {
		return err
	}
	defMaxAge := DefaultMaxAge
	if p.Defaults.MaxAge != nil {
		if *p.Defaults.MaxAge <= 0 {
			return errors.New("defaults.max_age must be positive")
		}
		defMaxAge = time.Duration(*p.Defaults.MaxAge)
	}
	defUnverifiable := p.Defaults.OnUnverifiable
	if defUnverifiable == "" {
		defUnverifiable = UnverifiableBypass
	}
	if !defUnverifiable.valid() {
		return fmt.Errorf("defaults.on_unverifiable %q: want bypass or serve_with_warning", defUnverifiable)
	}
	defUnknown := p.Defaults.OnUnknown
	if defUnknown == "" {
		defUnknown = UnknownAuto
	}
	if !defUnknown.valid() {
		return fmt.Errorf("defaults.on_unknown %q: want auto, revalidate, serve_with_warning or bypass", defUnknown)
	}
	defVerify := p.Defaults.VerifyNewAnswers != nil && *p.Defaults.VerifyNewAnswers
	defContradicted := p.Defaults.OnContradicted
	if defContradicted == "" {
		defContradicted = ContradictedReport
	}
	if !defContradicted.valid() {
		return fmt.Errorf("defaults.on_contradicted %q: want report, uncache or correct", defContradicted)
	}

	categories := make(map[string]*Category, len(p.Categories))
	for i, c := range p.Categories {
		if c == nil {
			return fmt.Errorf("categories[%d] is null", i)
		}
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" {
			return fmt.Errorf("categories[%d]: name is required", i)
		}
		if categories[c.Name] != nil {
			return fmt.Errorf("category %q is defined twice", c.Name)
		}
		if len(c.Keywords) == 0 {
			return fmt.Errorf("category %q: at least one keyword is required", c.Name)
		}
		m, err := compileKeywords(c.Keywords)
		if err != nil {
			return fmt.Errorf("category %q: %w", c.Name, err)
		}
		c.matcher = m
		c.maxAge = defMaxAge
		if c.MaxAge != nil {
			if *c.MaxAge <= 0 {
				return fmt.Errorf("category %q: max_age must be positive", c.Name)
			}
			c.maxAge = time.Duration(*c.MaxAge)
		}
		c.onUnverifiable = defUnverifiable
		if c.OnUnverifiable != "" {
			if !c.OnUnverifiable.valid() {
				return fmt.Errorf("category %q: on_unverifiable %q: want bypass or serve_with_warning", c.Name, c.OnUnverifiable)
			}
			c.onUnverifiable = c.OnUnverifiable
		}
		c.onUnknown = defUnknown
		if c.OnUnknown != "" {
			if !c.OnUnknown.valid() {
				return fmt.Errorf("category %q: on_unknown %q: want auto, revalidate, serve_with_warning or bypass", c.Name, c.OnUnknown)
			}
			c.onUnknown = c.OnUnknown
		}
		c.verifyNewAnswers = defVerify
		if c.VerifyNewAnswers != nil {
			c.verifyNewAnswers = *c.VerifyNewAnswers
		}
		c.onContradicted = defContradicted
		if c.OnContradicted != "" {
			if !c.OnContradicted.valid() {
				return fmt.Errorf("category %q: on_contradicted %q: want report, uncache or correct", c.Name, c.OnContradicted)
			}
			c.onContradicted = c.OnContradicted
		}
		categories[c.Name] = c
	}

	factNames := make(map[string]bool, len(p.Facts))
	for i, f := range p.Facts {
		if f == nil {
			return fmt.Errorf("facts[%d] is null", i)
		}
		f.Name = strings.TrimSpace(f.Name)
		if f.Name == "" {
			return fmt.Errorf("facts[%d]: name is required", i)
		}
		if factNames[f.Name] {
			return fmt.Errorf("fact %q is defined twice", f.Name)
		}
		factNames[f.Name] = true
		if categories[f.Category] == nil {
			return fmt.Errorf("fact %q: unknown category %q", f.Name, f.Category)
		}
		if err := validateSourceURL(f.SourceURL); err != nil {
			return fmt.Errorf("fact %q: source_url: %w", f.Name, err)
		}
		if f.Compare == "" {
			f.Compare = extract.Exact
		}
		if !f.Compare.Valid() {
			return fmt.Errorf("fact %q: compare %q: want exact, numeric or boolean", f.Name, f.Compare)
		}
		var err error
		if f.sourceRE, err = extract.CompilePattern(f.SourcePattern); err != nil {
			return fmt.Errorf("fact %q: source_pattern: %w", f.Name, err)
		}
		if f.answerRE, err = extract.CompilePattern(f.AnswerPattern); err != nil {
			return fmt.Errorf("fact %q: answer_pattern: %w", f.Name, err)
		}
		if len(f.Keywords) > 0 {
			if f.matcher, err = compileKeywords(f.Keywords); err != nil {
				return fmt.Errorf("fact %q: %w", f.Name, err)
			}
		}
	}
	return nil
}

// validateSourceURL requires an absolute http(s) URL without credentials,
// because the URL is sent verbatim to the evidence provider.
func validateSourceURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must be an absolute http(s) URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	if u.User != nil {
		return errors.New("must not contain credentials (it is sent to the evidence provider)")
	}
	return nil
}

// Effective is the merged policy for one request: the strictest settings of
// every matched category, and the watched facts that apply to the prompt.
type Effective struct {
	Categories       []string
	MaxAge           time.Duration
	OnUnverifiable   OnUnverifiable
	OnUnknown        OnUnknown
	VerifyNewAnswers bool
	OnContradicted   OnContradicted
	Facts            []*Fact
}

// Classify returns the categories whose keywords appear in text, in policy order.
func (p *Policy) Classify(text string) []*Category {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []*Category
	for _, c := range p.Categories {
		if c.matcher.MatchString(text) {
			out = append(out, c)
		}
	}
	return out
}

// Resolve classifies text and merges the matched categories. ok is false
// when the prompt is not time-sensitive. When several categories match, the
// smallest max_age, the strictest on_unverifiable (bypass wins), the
// strictest on_unknown and the most active on_contradicted apply, new answers
// are verified if any category asks for it, and facts of every matched
// category are checked.
func (p *Policy) Resolve(text string) (eff Effective, ok bool) {
	cats := p.Classify(text)
	if len(cats) == 0 {
		return Effective{}, false
	}
	return p.effective(cats, text), true
}

// ResolveCategory is Resolve for one named category, whether or not its
// keywords appear in text: the category's own settings apply, and its facts
// are kept when their keywords (if any) match text. ok is false when no
// category has that name.
func (p *Policy) ResolveCategory(name, text string) (eff Effective, ok bool) {
	name = strings.TrimSpace(name)
	for _, c := range p.Categories {
		if c.Name == name {
			return p.effective([]*Category{c}, text), true
		}
	}
	return Effective{}, false
}

// EffectiveMaxAge is the category's max_age after the defaults are applied.
func (c *Category) EffectiveMaxAge() time.Duration { return c.maxAge }

func (p *Policy) effective(cats []*Category, text string) Effective {
	eff := Effective{
		MaxAge:         cats[0].maxAge,
		OnUnverifiable: UnverifiableServeWithWarning,
		OnUnknown:      cats[0].onUnknown,
		OnContradicted: cats[0].onContradicted,
	}
	matched := make(map[string]bool, len(cats))
	for _, c := range cats {
		eff.Categories = append(eff.Categories, c.Name)
		matched[c.Name] = true
		eff.MaxAge = min(eff.MaxAge, c.maxAge)
		if c.onUnverifiable == UnverifiableBypass {
			eff.OnUnverifiable = UnverifiableBypass
		}
		if unknownRank(c.onUnknown) > unknownRank(eff.OnUnknown) {
			eff.OnUnknown = c.onUnknown
		}
		if contradictedRank(c.onContradicted) > contradictedRank(eff.OnContradicted) {
			eff.OnContradicted = c.onContradicted
		}
		eff.VerifyNewAnswers = eff.VerifyNewAnswers || c.verifyNewAnswers
	}
	for _, f := range p.Facts {
		if matched[f.Category] && (f.matcher == nil || f.matcher.MatchString(text)) {
			eff.Facts = append(eff.Facts, f)
		}
	}
	return eff
}

// FactNames returns the names of the facts in eff, in policy order.
func (e Effective) FactNames() []string {
	out := make([]string, 0, len(e.Facts))
	for _, f := range e.Facts {
		out = append(out, f.Name)
	}
	return out
}

// Fact returns the watched fact with that name, or nil.
func (p *Policy) Fact(name string) *Fact {
	name = strings.TrimSpace(name)
	for _, f := range p.Facts {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// HasCategory reports whether a category with that name exists.
func (p *Policy) HasCategory(name string) bool {
	name = strings.TrimSpace(name)
	for _, c := range p.Categories {
		if c.Name == name {
			return true
		}
	}
	return false
}

// FactsInCategory returns the watched facts of one category, in policy order.
func (p *Policy) FactsInCategory(category string) []*Fact {
	var out []*Fact
	for _, f := range p.Facts {
		if f.Category == category {
			out = append(out, f)
		}
	}
	return out
}

// FactsWithSource returns the watched facts whose source_url is rawURL,
// ignoring scheme and host case, a trailing slash and the fragment.
func (p *Policy) FactsWithSource(rawURL string) []*Fact {
	want, ok := comparableURL(rawURL)
	if !ok {
		return nil
	}
	var out []*Fact
	for _, f := range p.Facts {
		if got, ok := comparableURL(f.SourceURL); ok && got == want {
			out = append(out, f)
		}
	}
	return out
}

func comparableURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/") + "?" + u.RawQuery, true
}
