package discover

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
)

// Reasons a search result is not picked.
const (
	RejectUnsafeURL   = "unsafe_url"
	RejectLowScore    = "low_score"
	RejectNotIncluded = "outside_include_domains"
	RejectExcluded    = "excluded_domain"
	RejectNoEntity    = "entity_not_in_snippet"
)

// safeURL accepts only https URLs whose host is a name: no IP literal, no
// localhost, no credentials. The chosen URL is later fetched (by Tavily
// Extract, or by the direct provider), so this is also the SSRF guard.
func safeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("%q is not https", raw)
	}
	if u.User != nil {
		return nil, errors.New("URL carries credentials")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return nil, errors.New("URL has no host")
	}
	if net.ParseIP(host) != nil {
		return nil, errors.New("host is an IP address")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") {
		return nil, errors.New("host is local")
	}
	return u, nil
}

// hostMatches reports whether host is domain or a subdomain of it. A domain
// ending in ".*" ("amazon.*") matches that name under any suffix.
func hostMatches(host, domain string) bool {
	host, domain = strings.ToLower(host), strings.ToLower(domain)
	if base, ok := strings.CutSuffix(domain, ".*"); ok {
		return strings.HasPrefix(host, base+".") || strings.Contains(host, "."+base+".")
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func anyHost(host string, domains []string) bool {
	for _, d := range domains {
		if hostMatches(host, d) {
			return true
		}
	}
	return false
}

// entityTokens are the lower-case words of the entities.
func entityTokens(entities []string) []string {
	var out []string
	for _, e := range entities {
		for _, w := range strings.Fields(strings.ToLower(e)) {
			out = append(out, w)
		}
	}
	return out
}

func hasAllWords(text string, words []string) bool {
	for _, w := range words {
		if !regexp.MustCompile(`(?i)(?:^|[^\pL\pN])` + regexp.QuoteMeta(w) + `(?:$|[^\pL\pN])`).MatchString(text) {
			return false
		}
	}
	return true
}

// sameSite reports whether a distinctive entity word (3+ letters) is part of
// the host or the path, e.g. "acme" in acme.com or /acme-pricing/.
func sameSite(u *url.URL, words []string) bool {
	where := strings.ToLower(u.Hostname() + " " + u.Path)
	for _, w := range words {
		if len(w) >= 3 && strings.Contains(where, w) {
			return true
		}
	}
	return false
}

// Rank filters results by the rules below and orders the survivors: same-site
// results first, then Tavily's own rank.
//  1. https, host a name (not an IP or localhost);
//  2. score >= min_score;
//  3. host within include_domains, when set;
//  4. host not in the excluded domains;
//  5. every entity word appears in the title or the snippet.
//
// rejected counts the dropped results per reason.
func Rank(results []evidence.SearchResult, d *policy.Discover, entities []string) (accepted []evidence.SearchResult, rejected map[string]int) {
	rejected = map[string]int{}
	words := entityTokens(entities)
	type cand struct {
		r    evidence.SearchResult
		same bool
	}
	var cands []cand
	for i, r := range results {
		if r.Rank == 0 {
			r.Rank = i + 1
		}
		u, err := safeURL(r.URL)
		switch {
		case err != nil:
			rejected[RejectUnsafeURL]++
		case r.Score < d.Score():
			rejected[RejectLowScore]++
		case len(d.IncludeDomains) > 0 && !anyHost(u.Hostname(), d.IncludeDomains):
			rejected[RejectNotIncluded]++
		case anyHost(u.Hostname(), d.Exclusions()):
			rejected[RejectExcluded]++
		case !hasAllWords(r.Title+" \n "+r.Content, words):
			rejected[RejectNoEntity]++
		default:
			cands = append(cands, cand{r: r, same: sameSite(u, words)})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].same != cands[j].same {
			return cands[i].same
		}
		return cands[i].r.Rank < cands[j].r.Rank
	})
	for _, c := range cands {
		accepted = append(accepted, c.r)
	}
	return accepted, rejected
}

// Pick returns the best acceptable result. When there is none, reason says
// how many results were dropped and why.
func Pick(results []evidence.SearchResult, d *policy.Discover, entities []string) (evidence.SearchResult, string, bool) {
	accepted, rejected := Rank(results, d, entities)
	if len(accepted) > 0 {
		return accepted[0], "", true
	}
	return evidence.SearchResult{}, rejectionSummary(len(results), rejected), false
}

func rejectionSummary(total int, rejected map[string]int) string {
	if total == 0 {
		return "search returned no results"
	}
	keys := make([]string, 0, len(rejected))
	for k := range rejected {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", rejected[k], k))
	}
	return fmt.Sprintf("none of %d results was usable (%s)", total, strings.Join(parts, ", "))
}

// candidateURL returns the first candidate URL that passes the same URL and
// domain rules as a search result ("" = none).
func candidateURL(d *policy.Discover) string {
	for _, raw := range d.CandidateURLs {
		if pageAllowed(raw, d) == nil {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}

// pageAllowed applies the URL rules (safeURL) and, when d is not nil, the
// category's include_domains and excluded domains to one page URL.
func pageAllowed(raw string, d *policy.Discover) error {
	u, err := safeURL(raw)
	if err != nil {
		return err
	}
	if d == nil {
		return nil
	}
	if len(d.IncludeDomains) > 0 && !anyHost(u.Hostname(), d.IncludeDomains) {
		return fmt.Errorf("%s is outside include_domains", u.Hostname())
	}
	if anyHost(u.Hostname(), d.Exclusions()) {
		return fmt.Errorf("%s is an excluded domain", u.Hostname())
	}
	return nil
}

// allowed checks every page of a learned source against pageAllowed. A
// remembered miss names no page and is always allowed.
func (e *Learned) allowed(d *policy.Discover) error {
	if e.NoSource {
		return nil
	}
	if err := pageAllowed(e.URL, d); err != nil {
		return err
	}
	for _, a := range e.Also {
		if err := pageAllowed(a.URL, d); err != nil {
			return err
		}
	}
	return nil
}

// hosts lists the distinct hosts of results, for a "no source" report.
func hosts(results []evidence.SearchResult) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range results {
		u, err := url.Parse(r.URL)
		if err != nil || u.Hostname() == "" || seen[u.Hostname()] {
			continue
		}
		seen[u.Hostname()] = true
		out = append(out, u.Hostname())
	}
	return out
}
