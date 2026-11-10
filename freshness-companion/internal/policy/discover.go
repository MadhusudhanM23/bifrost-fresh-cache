package policy

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"freshness-companion/internal/extract"
)

// Discover is a category's optional "Tavily finds the source" block. When the
// companion runs with -discover and no watched fact covers a time-sensitive
// cache hit, it builds a short search query from Query (the template) and the
// product names found in the question, asks Tavily Search for a page, and
// checks the cached answer against that page as if it were a watched fact.
//
// Only the derived query and the chosen URL ever leave the companion. The
// question, the answer and the system messages never do.
type Discover struct {
	Comment string `json:"_comment,omitempty"`
	Enabled bool   `json:"enabled"`
	// Entities is the allowlist of names (products, plans) that may be put in
	// a query. Only those that appear in the question (whole words, any case)
	// are used; when none appears there is no search.
	Entities []string `json:"entities,omitempty"`
	// QueryFrom is "entities" (default: the allowlist) or "proper_nouns"
	// (opt-in: capitalised word runs from the question, which can include a
	// person's name).
	QueryFrom string `json:"query_from,omitempty"`
	// Query is the template, e.g. "{entities} plan price per month". It must
	// contain {entities} and no other placeholder.
	Query string `json:"query"`
	// IncludeDomains limits search results to these hosts (and their
	// subdomains); the result is rechecked locally.
	IncludeDomains []string `json:"include_domains,omitempty"`
	// ExcludeDomains are added to the built-in list of aggregator and social
	// sites (DefaultExcludeDomains).
	ExcludeDomains []string `json:"exclude_domains,omitempty"`
	// RequireDomains turns discovery off for the category unless
	// IncludeDomains is set.
	RequireDomains bool `json:"require_domains,omitempty"`
	// Compare is exact, numeric (default) or boolean.
	Compare extract.Mode `json:"compare,omitempty"`
	// ValuePattern has one capture group: the value. It reads the cached
	// answer, and the page right after an entity name. Defaults exist for
	// numeric (a currency amount) and boolean (stock words); exact needs one.
	ValuePattern string `json:"value_pattern,omitempty"`
	// NearChars is how many non-digit characters may sit between the entity
	// name and the value on the page (default 40, at most 200).
	NearChars int `json:"near_chars,omitempty"`
	// MinScore is the lowest Tavily relevance score accepted (default 0.5).
	MinScore *float64 `json:"min_score,omitempty"`
	// MinSources 2 reads the two best results; if they disagree the answer is
	// unable_to_verify. Default 1.
	MinSources int `json:"min_sources,omitempty"`
	// CandidateURLs are https pages read when search finds nothing usable.
	// Such a page is labelled found_by=candidate_url, never tavily_search.
	CandidateURLs []string `json:"candidate_urls,omitempty"`

	active  bool
	valueRE *regexp.Regexp
	exclude []string
}

// Query sources.
const (
	QueryFromEntities    = "entities"
	QueryFromProperNouns = "proper_nouns"
)

// Discovery defaults.
const (
	DefaultDiscoverNear     = 40
	MaxDiscoverNear         = 200
	DefaultDiscoverMinScore = 0.5
)

// DefaultExcludeDomains are never picked by discovery: forums, social sites,
// encyclopedias and marketplaces restate prices second hand.
var DefaultExcludeDomains = []string{
	"reddit.com", "quora.com", "youtube.com", "facebook.com", "x.com", "twitter.com",
	"pinterest.com", "medium.com", "wikipedia.org", "amazon.*", "ebay.*",
}

// Default value patterns.
const (
	DefaultNumericValuePattern = `[$€£]\s*([0-9][0-9,]*(?:\.[0-9]+)?)`
	DefaultBooleanValuePattern = `(?i)\b((?:not\s+|un)?available|in\s+stock|out\s+of\s+stock|sold\s+out|back-?ordered)\b`
)

var placeholderRE = regexp.MustCompile(`\{[^{}]*\}`)

// Active reports whether discovery may run for this category.
func (d *Discover) Active() bool { return d != nil && d.active }

// ValueRegexp returns the compiled value pattern.
func (d *Discover) ValueRegexp() *regexp.Regexp { return d.valueRE }

// ValuePatternSource returns the value pattern after defaults.
func (d *Discover) ValuePatternSource() string { return d.valueRE.String() }

// Near returns near_chars after defaults.
func (d *Discover) Near() int {
	if d.NearChars <= 0 {
		return DefaultDiscoverNear
	}
	return d.NearChars
}

// Score returns min_score after defaults.
func (d *Discover) Score() float64 {
	if d.MinScore == nil {
		return DefaultDiscoverMinScore
	}
	return *d.MinScore
}

// Sources returns min_sources after defaults (1 or 2).
func (d *Discover) Sources() int {
	if d.MinSources >= 2 {
		return 2
	}
	return 1
}

// Mode returns the compare mode after defaults.
func (d *Discover) Mode() extract.Mode { return d.Compare }

// Exclusions returns the default and the configured excluded domains.
func (d *Discover) Exclusions() []string { return d.exclude }

func (d *Discover) compile(category string) error {
	where := fmt.Sprintf("category %q: discover", category)
	if d.Compare == "" {
		d.Compare = extract.Numeric
	}
	if !d.Compare.Valid() {
		return fmt.Errorf("%s: compare %q: want exact, numeric or boolean", where, d.Compare)
	}
	if !strings.Contains(d.Query, "{entities}") {
		return fmt.Errorf("%s: query %q must contain {entities}", where, d.Query)
	}
	for _, ph := range placeholderRE.FindAllString(d.Query, -1) {
		if ph != "{entities}" {
			return fmt.Errorf("%s: query %q: unknown placeholder %s (only {entities} is allowed)", where, d.Query, ph)
		}
	}
	switch d.QueryFrom {
	case "", QueryFromEntities:
		if len(d.Entities) == 0 {
			return fmt.Errorf("%s: entities is required (or set query_from to %q)", where, QueryFromProperNouns)
		}
	case QueryFromProperNouns:
	default:
		return fmt.Errorf("%s: query_from %q: want %s or %s", where, d.QueryFrom, QueryFromEntities, QueryFromProperNouns)
	}
	for i, e := range d.Entities {
		e = strings.Join(strings.Fields(e), " ")
		if e == "" {
			return fmt.Errorf("%s: entities[%d] is empty", where, i)
		}
		d.Entities[i] = e
	}
	pattern := d.ValuePattern
	if pattern == "" {
		switch d.Compare {
		case extract.Numeric:
			pattern = DefaultNumericValuePattern
		case extract.Boolean:
			pattern = DefaultBooleanValuePattern
		default:
			return fmt.Errorf("%s: compare exact needs a value_pattern", where)
		}
	}
	re, err := extract.CompilePattern(pattern)
	if err != nil {
		return fmt.Errorf("%s: value_pattern: %w", where, err)
	}
	d.valueRE = re
	if d.NearChars < 0 || d.NearChars > MaxDiscoverNear {
		return fmt.Errorf("%s: near_chars must be between 0 and %d", where, MaxDiscoverNear)
	}
	if d.MinScore != nil && (*d.MinScore < 0 || *d.MinScore > 1) {
		return fmt.Errorf("%s: min_score must be between 0 and 1", where)
	}
	if d.MinSources < 0 || d.MinSources > 2 {
		return fmt.Errorf("%s: min_sources must be 1 or 2", where)
	}
	for i, h := range d.IncludeDomains {
		if d.IncludeDomains[i], err = bareHost(h); err != nil {
			return fmt.Errorf("%s: include_domains: %w", where, err)
		}
	}
	d.exclude = append([]string(nil), DefaultExcludeDomains...)
	for _, h := range d.ExcludeDomains {
		host, err := bareHost(h)
		if err != nil {
			return fmt.Errorf("%s: exclude_domains: %w", where, err)
		}
		d.exclude = append(d.exclude, host)
	}
	for _, raw := range d.CandidateURLs {
		if err := validateSourceURL(raw); err != nil {
			return fmt.Errorf("%s: candidate_urls: %w", where, err)
		}
		if u, _ := url.Parse(strings.TrimSpace(raw)); u.Scheme != "https" {
			return fmt.Errorf("%s: candidate_urls: %q must be https", where, raw)
		}
	}
	d.active = d.Enabled && !(d.RequireDomains && len(d.IncludeDomains) == 0)
	return nil
}

// bareHost accepts "example.com" or "*.example.com"-free plain hosts, and the
// "amazon.*" style used by the defaults. No scheme, path, port or user.
func bareHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return "", errors.New("empty host")
	}
	if strings.ContainsAny(h, "/:@?# ") {
		return "", fmt.Errorf("%q must be a bare host like example.com (no scheme, path or port)", h)
	}
	if net.ParseIP(h) != nil {
		return "", fmt.Errorf("%q: an IP address is not a domain", h)
	}
	if !strings.Contains(h, ".") {
		return "", fmt.Errorf("%q is not a domain", h)
	}
	return h, nil
}

// NewDiscoveredFact builds a fact for a source that discovery found. It is
// never added to the policy's facts. Its patterns are already compiled.
func NewDiscoveredFact(name, category, sourceURL string, src, ans *regexp.Regexp, mode extract.Mode) *Fact {
	return &Fact{
		Name: name, Category: category, SourceURL: sourceURL,
		SourcePattern: src.String(), AnswerPattern: ans.String(), Compare: mode,
		Discovered: true, sourceRE: src, answerRE: ans,
	}
}

// Trusted reports whether search is limited to include_domains. Only then
// may a discovered page that agrees with an answer serve it; a page found on
// the open web can only send an answer to refresh.
func (d *Discover) Trusted() bool { return d != nil && len(d.IncludeDomains) > 0 }
