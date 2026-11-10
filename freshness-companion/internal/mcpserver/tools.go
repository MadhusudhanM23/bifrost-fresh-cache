package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"freshness-companion/internal/changes"
	"freshness-companion/internal/decision"
	"freshness-companion/internal/discover"
	"freshness-companion/internal/extract"
	"freshness-companion/internal/policy"
)

// Tool names.
const (
	ToolCheckFreshness     = "check_freshness"
	ToolExplainRequest     = "explain_request"
	ToolListWatchedSources = "list_watched_sources"
	ToolReportChange       = "report_change"
)

// checkTimeout bounds the evidence fetches of one check_freshness call.
const checkTimeout = 20 * time.Second

type tool struct {
	name        string
	description string
	schema      map[string]any
	run         func(r *http.Request, args map[string]any) toolResult
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if required == nil {
		required = []string{}
	}
	s["required"] = required
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func (s *Server) toolList() []tool {
	return []tool{
		{
			name: ToolCheckFreshness,
			description: "Check whether an answer about a time-sensitive fact (a price, a policy, an opening time) is still current. " +
				"The question is classified with the companion's policy; for each watched fact that applies, the live source page is read and the current value is extracted. " +
				"If an answer is given, the value it states is compared with the current value: confirmed, stale or unable_to_verify. " +
				"Without an answer, the current values are returned so the caller can compare them itself.",
			schema: objectSchema(map[string]any{
				"question": str("The user's question, used to find the category and the watched facts."),
				"answer":   str("Optional: the answer to check (a cached or proposed answer). Its stated value is compared with the source."),
				"category": str("Optional: force a policy category (for example \"price\") instead of classifying the question by keywords."),
			}, "question"),
			run: s.checkFreshness,
		},
		{
			name: ToolExplainRequest,
			description: "Explain how one request through the freshness companion was answered: from the cache or the model, whether the cached answer was checked against its source, what the gate and guard did. " +
				"Takes the x-freshness-request-id header value of a chat completion response. Returns plain words and the structured record. No prompt or answer text is kept.",
			schema: objectSchema(map[string]any{
				"request_id": str("The id from the x-freshness-request-id response header, like \"req-3fa9c2d1-7\"."),
			}, "request_id"),
			run: s.explainRequest,
		},
		{
			name:        ToolListWatchedSources,
			description: "List the watched facts of the freshness policy (name, category, source URL, how long an answer may be reused) and the change notices recorded so far.",
			schema:      objectSchema(map[string]any{}),
			run:         s.listWatchedSources,
		},
		{
			name: ToolReportChange,
			description: "Report that a watched fact, a whole category or a source page changed, so cached answers about it are checked again before they are reused. " +
				"Send exactly one of fact, category or source_url. Same effect as POST /freshness/changes; if the companion requires a change-notice token, send it as \"Authorization: Bearer <token>\" or in the " + NoticeTokenHeader + " header of the MCP request.",
			schema: objectSchema(map[string]any{
				"fact":       str("Name of a watched fact, as listed by list_watched_sources."),
				"category":   str("Name of a policy category, for example \"policy\"."),
				"source_url": str("A source URL that watched facts read from."),
				"note":       str("Optional free-text note kept with the notice."),
				"clear_cache_keys": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional: Bifrost cache keys whose entries are deleted right away (at most 10).",
				},
			}),
			run: s.reportChange,
		},
	}
}

// factCheck is one watched fact in a check_freshness result.
type factCheck struct {
	Name           string     `json:"name"`
	SourceURL      string     `json:"source_url"`
	Compare        string     `json:"compare"`
	CurrentValue   string     `json:"current_value,omitempty"`
	CurrentError   string     `json:"current_error,omitempty"`
	ValueInAnswer  string     `json:"value_in_answer,omitempty"`
	AnswerError    string     `json:"answer_error,omitempty"`
	Verdict        string     `json:"verdict"`
	Provider       string     `json:"provider,omitempty"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
	EvidenceCached bool       `json:"evidence_cached,omitempty"`
	// Discovery: set when no watched fact covered the question and the page
	// was found by discovery. Query is the exact text sent to Tavily Search.
	Discovered bool    `json:"discovered,omitempty"`
	FoundBy    string  `json:"found_by,omitempty"`
	Query      string  `json:"query,omitempty"`
	Rank       int     `json:"rank,omitempty"`
	Score      float64 `json:"score,omitempty"`

	current     extract.Value // the page's value, for the min_sources check
	haveCurrent bool
}

// Per-fact verdicts, next to the decision package's outcomes.
const (
	VerdictNoAnswer = "no_answer_to_compare"
	OutcomeNotTime  = "not_time_sensitive"
	OutcomeNotCmp   = "not_compared"
	ActionNone      = "none"
)

// checkResult is the structured result of check_freshness.
type checkResult struct {
	TimeSensitive bool        `json:"time_sensitive"`
	Categories    []string    `json:"categories"`
	Category      string      `json:"category,omitempty"`
	MaxAge        string      `json:"max_age,omitempty"`
	Facts         []factCheck `json:"facts"`
	// Discovery is the discovery result when it was tried (no_source,
	// over_cap, ...; "found" when a page was found and checked).
	Discovery string `json:"discovery,omitempty"`
	Outcome   string `json:"outcome"`
	Action    string `json:"action"`
	Summary   string `json:"summary"`
}

func (s *Server) checkFreshness(r *http.Request, args map[string]any) toolResult {
	question, err := argString(args, "question")
	if err != nil {
		return errorResult(err.Error())
	}
	answer, err := argString(args, "answer")
	if err != nil {
		return errorResult(err.Error())
	}
	category, err := argString(args, "category")
	if err != nil {
		return errorResult(err.Error())
	}
	if strings.TrimSpace(question) == "" {
		return errorResult("question is required")
	}
	var eff policy.Effective
	var timeSensitive bool
	if strings.TrimSpace(category) != "" {
		eff, timeSensitive = s.deps.Policy.ResolveCategory(category, question)
		if !timeSensitive {
			return errorResult(fmt.Sprintf("no category named %q; list_watched_sources shows the categories", strings.TrimSpace(category)))
		}
	} else {
		eff, timeSensitive = s.deps.Policy.Resolve(question)
	}
	out := checkResult{TimeSensitive: timeSensitive, Categories: []string{}, Facts: []factCheck{}}
	if !timeSensitive {
		out.Outcome, out.Action = OutcomeNotTime, string(decision.Serve)
		out.Summary = "The question matches no time-sensitive category of the policy, so no freshness check is needed: a cached answer may be reused."
		return textResult(out.Summary, out)
	}
	out.Categories = eff.Categories
	out.Category = eff.Categories[0]
	out.MaxAge = eff.MaxAge.String()

	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	hasAnswer := strings.TrimSpace(answer) != ""
	facts := eff.Facts
	var found discover.Found
	discoveryNote := ""
	if len(facts) == 0 && s.deps.Discover != nil && eff.Discover.Active() {
		// Like the proxy: no answer, or an answer that states no single
		// value, leaves nothing to check, so nothing is searched.
		if !hasAnswer {
			out.Discovery = "skipped"
			discoveryNote = " Discovery was not tried: no answer was given to check, so nothing was searched."
		} else if _, err := extract.Extract(eff.Discover.ValueRegexp(), answer, eff.Discover.Mode()); err != nil {
			out.Discovery = string(decision.UnableToVerify)
			discoveryNote = " Discovery was not tried: the value pattern " + describe(err) + " in the answer, so nothing was searched."
		} else if fs, fd, err := s.discoverFacts(ctx, eff, question); err != nil {
			out.Discovery = discover.Reason(err)
			discoveryNote = " Discovery did not find a page to check: " + oneLine(err.Error(), 300) + "."
		} else {
			out.Discovery, facts, found = "found", fs, fd
		}
	}
	page := 0
	for _, f := range facts {
		fc := s.checkFact(ctx, f, answer, hasAnswer)
		if f.Discovered {
			fc.Discovered, fc.FoundBy, fc.Query = true, found.FoundBy, found.Query
			fc.Rank, fc.Score = found.Sources[page].Rank, found.Sources[page].Score
			page++
		}
		out.Facts = append(out.Facts, fc)
	}
	if page > 0 {
		s.settleDiscovered(eff, found, out.Facts)
	}
	worst := decision.Confirmed
	for _, fc := range out.Facts {
		if o := decision.Outcome(fc.Verdict); hasAnswer && severity(o) > severity(worst) {
			worst = o
		}
	}

	switch {
	case len(facts) == 0, worst == decision.Confirmed && page > 0 && !eff.Discover.Trusted():
		// An open-web page (no include_domains) that agrees is not a trusted
		// source: the verdict stays the one without discovery, plus a note.
		if len(facts) > 0 {
			discoveryNote = " Discovery found a page on the open web that agrees (" + agreements(out.Facts) +
				"), but the category has no include_domains, so that page is an unverified source and does not confirm the answer."
		}
		out.Outcome, out.Action = string(decision.Unknown), ActionNone
		out.Summary = fmt.Sprintf("The question is time-sensitive (category %s), but no watched fact covers it, so the source cannot be checked. An answer in this category may be reused for %s after it was written; after that it should be refreshed.",
			joinCats(eff.Categories), out.MaxAge) + discoveryNote
	case !hasAnswer:
		out.Outcome, out.Action = OutcomeNotCmp, ActionNone
		out.Summary = fmt.Sprintf("The question is time-sensitive (category %s, answers may be reused for %s). No answer was given to compare. %s",
			joinCats(eff.Categories), out.MaxAge, currentValues(out.Facts))
	case worst == decision.Stale:
		out.Outcome, out.Action = string(decision.Stale), string(decision.Refresh)
		out.Summary = "The answer is out of date: " + differences(out.Facts) + " It should not be reused; ask the model again."
	case worst == decision.UnableToVerify:
		out.Outcome = string(decision.UnableToVerify)
		if eff.OnUnverifiable == policy.UnverifiableServeWithWarning {
			out.Action = string(decision.ServeWithWarning)
		} else {
			out.Action = string(decision.Refresh)
		}
		out.Summary = fmt.Sprintf("The answer could not be checked against the source: %s The policy for this category says %s.",
			problems(out.Facts), actionWords(out.Action))
	default:
		out.Outcome, out.Action = string(decision.Confirmed), string(decision.Serve)
		out.Summary = "The answer is current: " + agreements(out.Facts) + " It may be reused."
	}
	return textResult(out.Summary, out)
}

// discoverFacts asks discovery for pages and builds one fact per page (two
// with min_sources 2). Only the derived query is sent to Tavily Search.
func (s *Server) discoverFacts(ctx context.Context, eff policy.Effective, question string) ([]*policy.Fact, discover.Found, error) {
	d := eff.Discover
	found, err := s.deps.Discover.Find(ctx, eff.DiscoverCategory, d, question)
	if err != nil {
		return nil, found, err
	}
	if len(found.Sources) == 0 {
		return nil, found, discover.ErrNoSource
	}
	src, err := discover.EntityPattern(found.Entities, d.ValuePatternSource(), d.Near())
	if err != nil {
		return nil, found, err
	}
	facts := make([]*policy.Fact, len(found.Sources))
	for i, page := range found.Sources {
		facts[i] = policy.NewDiscoveredFact(decision.DiscoveredFactPrefix+found.Query, eff.DiscoverCategory, page.URL, src, d.ValueRegexp(), d.Mode())
	}
	return facts, found, nil
}

// settleDiscovered applies the proxy's min_sources rule to the discovered
// pages: they must all agree, or every one of them is unable_to_verify. It
// then records the outcome once for the learned source.
func (s *Server) settleDiscovered(eff policy.Effective, found discover.Found, facts []factCheck) {
	var idx []int
	var pages []decision.PageCheck
	for i := range facts {
		if !facts[i].Discovered {
			continue
		}
		fc := &facts[i]
		idx = append(idx, i)
		reason := fc.CurrentError
		if reason == "" {
			reason = fmt.Sprintf("%s reads %s (%s)", fc.SourceURL, fc.CurrentValue, fc.Verdict)
		}
		pages = append(pages, decision.PageCheck{Outcome: decision.Outcome(fc.Verdict), Current: fc.current, HaveCurrent: fc.haveCurrent, Reason: reason})
	}
	if why, ok := decision.PagesAgree(pages, eff.Discover.Mode()); !ok {
		for n, i := range idx {
			facts[i].Verdict = string(decision.UnableToVerify)
			facts[i].CurrentError = why
			if n > 0 {
				facts[i].CurrentError = "the discovered pages disagree (see " + facts[idx[0]].Name + " above)"
			}
		}
	}
	first := facts[idx[0]]
	s.deps.Discover.Record(eff.DiscoverCategory, found.Query, first.Verdict, first.CurrentValue)
}

func (s *Server) checkFact(ctx context.Context, f *policy.Fact, answer string, hasAnswer bool) factCheck {
	fc := factCheck{Name: f.Name, SourceURL: f.SourceURL, Compare: string(f.Compare), Verdict: string(decision.UnableToVerify)}
	var inAnswer extract.Value
	haveAnswer := false
	if hasAnswer {
		v, err := extract.Extract(f.AnswerRegexp(), answer, f.Compare)
		if err != nil {
			fc.AnswerError = "the answer states no readable value for this fact (answer_pattern " + describe(err) + ")"
		} else {
			fc.ValueInAnswer, inAnswer, haveAnswer = v.Raw, v, true
		}
	}
	if s.deps.Evidence == nil {
		fc.CurrentError = "no evidence provider is configured"
		return fc
	}
	fc.Provider = s.deps.Evidence.ProviderName()
	res, err := s.deps.Evidence.Get(ctx, f.SourceURL)
	if err != nil {
		fc.CurrentError = "the source could not be read: " + oneLine(err.Error(), 200)
		return fc
	}
	if res.Evidence.Provider != "" {
		fc.Provider = res.Evidence.Provider
	}
	if !res.Evidence.FetchedAt.IsZero() {
		t := res.Evidence.FetchedAt.UTC()
		fc.CheckedAt = &t
	}
	fc.EvidenceCached = res.Cached
	current, err := extract.Extract(f.SourceRegexp(), res.Evidence.Content, f.Compare)
	if err != nil {
		fc.CurrentError = "the source page states no readable value (source_pattern " + describe(err) + ")"
		return fc
	}
	fc.CurrentValue, fc.current, fc.haveCurrent = current.Raw, current, true
	switch {
	case !hasAnswer:
		fc.Verdict = VerdictNoAnswer
	case !haveAnswer:
		// unable_to_verify: the answer has no value to compare.
	case extract.Equal(inAnswer, current, f.Compare):
		fc.Verdict = string(decision.Confirmed)
	default:
		fc.Verdict = string(decision.Stale)
	}
	return fc
}

func severity(o decision.Outcome) int {
	switch o {
	case decision.Stale:
		return 2
	case decision.UnableToVerify:
		return 1
	}
	return 0
}

func describe(err error) string {
	var conflict *extract.ConflictError
	switch {
	case errors.Is(err, extract.ErrNoMatch):
		return "matched nothing"
	case errors.As(err, &conflict):
		return "found conflicting values: " + strings.Join(conflict.Values, ", ")
	default:
		return "failed: " + oneLine(err.Error(), 200)
	}
}

func joinCats(cats []string) string { return strings.Join(cats, ", ") }

func currentValues(facts []factCheck) string {
	var parts []string
	for _, f := range facts {
		if f.CurrentValue != "" {
			parts = append(parts, fmt.Sprintf("%s currently reads %s at %s%s.", f.Name, f.CurrentValue, f.SourceURL, checkedWords(f)))
		} else {
			parts = append(parts, fmt.Sprintf("%s: %s.", f.Name, f.CurrentError))
		}
	}
	return strings.Join(parts, " ")
}

func differences(facts []factCheck) string {
	var parts []string
	for _, f := range facts {
		if f.Verdict == string(decision.Stale) {
			parts = append(parts, fmt.Sprintf("the answer says %s but %s now says %s for %s%s.", f.ValueInAnswer, f.SourceURL, f.CurrentValue, f.Name, checkedWords(f)))
		}
	}
	return strings.Join(parts, " ")
}

func agreements(facts []factCheck) string {
	var parts []string
	for _, f := range facts {
		parts = append(parts, fmt.Sprintf("the answer says %s and %s still says %s for %s%s.", f.ValueInAnswer, f.SourceURL, f.CurrentValue, f.Name, checkedWords(f)))
	}
	return strings.Join(parts, " ")
}

func problems(facts []factCheck) string {
	var parts []string
	for _, f := range facts {
		if f.Verdict != string(decision.UnableToVerify) {
			continue
		}
		reason := f.CurrentError
		if reason == "" {
			reason = f.AnswerError
		}
		parts = append(parts, fmt.Sprintf("%s: %s.", f.Name, reason))
	}
	return strings.Join(parts, " ")
}

func checkedWords(f factCheck) string {
	if f.CheckedAt == nil || !f.EvidenceCached {
		return ""
	}
	return " (page read " + f.CheckedAt.Format(time.RFC3339) + ")"
}

func actionWords(action string) string {
	if action == string(decision.ServeWithWarning) {
		return "to serve the answer with a warning"
	}
	return "not to reuse the answer and to ask the model again"
}

func (s *Server) explainRequest(_ *http.Request, args map[string]any) toolResult {
	id, err := argString(args, "request_id")
	if err != nil {
		return errorResult(err.Error())
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errorResult("request_id is required: the value of the x-freshness-request-id response header")
	}
	if s.deps.Explain == nil {
		return errorResult("not found: request explanations are switched off on this companion (-explain-ring is negative)")
	}
	plain, record, ok := s.deps.Explain(id)
	if !ok {
		return errorResult(fmt.Sprintf("not found: no record of request %q. It may be mistyped, older than the last %d requests the companion remembers, or from before the companion restarted. Records are kept in memory only.", id, s.deps.ExplainKept))
	}
	return textResult(plain, record)
}

type watchedFact struct {
	Name      string   `json:"name"`
	Category  string   `json:"category"`
	Keywords  []string `json:"keywords,omitempty"`
	SourceURL string   `json:"source_url"`
	MaxAge    string   `json:"max_age"`
	Compare   string   `json:"compare"`
}

type watchedCategory struct {
	Name   string `json:"name"`
	MaxAge string `json:"max_age"`
}

type sourcesResult struct {
	EvidenceProvider string            `json:"evidence_provider,omitempty"`
	Categories       []watchedCategory `json:"categories"`
	Facts            []watchedFact     `json:"facts"`
	ChangeNotices    []changes.Notice  `json:"change_notices"`
	// LearnedSources are the pages discovery found (-discover); empty when
	// discovery is off.
	LearnedSources []discover.Learned `json:"learned_sources"`
}

func (s *Server) listWatchedSources(_ *http.Request, _ map[string]any) toolResult {
	out := sourcesResult{Categories: []watchedCategory{}, Facts: []watchedFact{}, ChangeNotices: []changes.Notice{}, LearnedSources: []discover.Learned{}}
	if s.deps.Evidence != nil {
		out.EvidenceProvider = s.deps.Evidence.ProviderName()
	}
	maxAge := map[string]string{}
	for _, c := range s.deps.Policy.Categories {
		maxAge[c.Name] = c.EffectiveMaxAge().String()
		out.Categories = append(out.Categories, watchedCategory{Name: c.Name, MaxAge: maxAge[c.Name]})
	}
	var lines []string
	for _, f := range s.deps.Policy.Facts {
		out.Facts = append(out.Facts, watchedFact{Name: f.Name, Category: f.Category, Keywords: f.Keywords, SourceURL: f.SourceURL, MaxAge: maxAge[f.Category], Compare: string(f.Compare)})
		lines = append(lines, fmt.Sprintf("- %s (category %s, reuse up to %s): %s", f.Name, f.Category, maxAge[f.Category], f.SourceURL))
	}
	if s.deps.Changes != nil {
		out.ChangeNotices = s.deps.Changes.Recent(50)
		if out.ChangeNotices == nil {
			out.ChangeNotices = []changes.Notice{}
		}
	}
	text := fmt.Sprintf("%d watched fact(s) across %d categor%s.", len(out.Facts), len(out.Categories), plural(len(out.Categories), "y", "ies"))
	if len(lines) > 0 {
		text += "\n" + strings.Join(lines, "\n")
	}
	if s.deps.Discover != nil {
		out.LearnedSources = s.deps.Discover.Sources()
		var learned []string
		for _, l := range out.LearnedSources {
			if l.NoSource {
				learned = append(learned, fmt.Sprintf("- %q (category %s): no usable page (%s)", l.Query, l.Category, l.Reason))
			} else {
				learned = append(learned, fmt.Sprintf("- %q (category %s): %s (found by %s, used %d time(s))", l.Query, l.Category, l.URL, l.FoundBy, l.Uses))
			}
		}
		text += fmt.Sprintf("\n%d learned source(s) from discovery.", len(learned))
		if len(learned) > 0 {
			text += "\n" + strings.Join(learned, "\n")
		}
	}
	switch n := len(out.ChangeNotices); {
	case n == 0:
		text += "\nNo change notices recorded."
	default:
		latest := out.ChangeNotices[0]
		text += fmt.Sprintf("\n%d change notice(s) recorded; the latest (%s) says %s %q changed at %s.", n, latest.ID, latest.Kind, latest.Target, latest.Time.Format(time.RFC3339))
	}
	return textResult(text, out)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (s *Server) reportChange(r *http.Request, args map[string]any) toolResult {
	if !s.noticeAuthorized(r) {
		return errorResult("unauthorized: a change notice needs the companion's change-notice token, sent as \"Authorization: Bearer <token>\" or in the " + NoticeTokenHeader + " header of the MCP request")
	}
	if s.deps.Report == nil {
		return errorResult("report_change is not available on this companion")
	}
	var req ChangeRequest
	var err error
	if req.Fact, err = argString(args, "fact"); err != nil {
		return errorResult(err.Error())
	}
	if req.Category, err = argString(args, "category"); err != nil {
		return errorResult(err.Error())
	}
	if req.SourceURL, err = argString(args, "source_url"); err != nil {
		return errorResult(err.Error())
	}
	if req.Note, err = argString(args, "note"); err != nil {
		return errorResult(err.Error())
	}
	if req.ClearCacheKeys, err = argStrings(args, "clear_cache_keys"); err != nil {
		return errorResult(err.Error())
	}
	n, err := s.deps.Report(r.Context(), req)
	if err != nil {
		return errorResult(err.Error())
	}
	text := fmt.Sprintf("Recorded change notice %s: %s %q changed. It covers %d fact(s)", n.ID, n.Kind, n.Target, len(n.Facts))
	if len(n.EvidenceDropped) > 0 {
		text += fmt.Sprintf("; the cached evidence of %d source page(s) was dropped, so the next check reads the page again", len(n.EvidenceDropped))
	}
	if len(n.ClearedCacheKeys) > 0 {
		text += fmt.Sprintf("; %d Bifrost cache key(s) were cleared", len(n.ClearedCacheKeys))
	}
	if len(n.ClearErrors) > 0 {
		text += "; some cache clears failed: " + strings.Join(n.ClearErrors, "; ")
	}
	text += ". Cached answers written before this notice are checked again before they are reused."
	return textResult(text, n)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
