package main

import "sort"

// DecisionRecord mirrors the JSON shape of freshness-companion's
// internal/decisionlog.Record, as served by GET /freshness/decisions. It is
// defined independently here (not imported from that module) so this
// dashboard has no build-time dependency on freshness-companion's internals;
// the field names and json tags are copied verbatim from
// freshness-companion/internal/decisionlog/log.go.
type DecisionRecord struct {
	Time             string   `json:"time"`
	CacheID          string   `json:"cache_id,omitempty"`
	HitType          string   `json:"hit_type,omitempty"`
	Similarity       string   `json:"similarity,omitempty"`
	Category         string   `json:"category"`
	Fact             string   `json:"fact,omitempty"`
	Outcome          string   `json:"outcome"`
	Action           string   `json:"action"`
	Reason           string   `json:"reason"`
	SourceURL        string   `json:"source_url,omitempty"`
	Provider         string   `json:"provider,omitempty"`
	CachedValue      string   `json:"cached_value,omitempty"`
	CurrentValue     string   `json:"current_value,omitempty"`
	CheckedAt        string   `json:"checked_at,omitempty"`
	EvidenceCached   bool     `json:"evidence_cached,omitempty"`
	AgeSeconds       *float64 `json:"age_seconds,omitempty"`
	RefreshStrategy  string   `json:"refresh_strategy,omitempty"`
	RefreshedCacheID string   `json:"refreshed_cache_id,omitempty"`
	RefreshedCheck   string   `json:"refreshed_check,omitempty"`
	Error            string   `json:"error,omitempty"`
	LatencyMS        float64  `json:"latency_ms"`
	AnswerOf         string   `json:"answer_of,omitempty"`
	ChangeNotice     string   `json:"change_notice,omitempty"`
	Correction       string   `json:"correction,omitempty"`
	CorrectedValue   string   `json:"corrected_value,omitempty"`
	CorrectedCheck   string   `json:"corrected_check,omitempty"`
	// SharedRefresh: this request waited for a refresh another request was
	// already running (a stampede avoided, one model call saved).
	SharedRefresh bool `json:"shared_refresh,omitempty"`
	// Gate: what the check before serving did to a risky semantic hit:
	// verified | trusted | blocked | unverified (empty = it did not apply).
	Gate string `json:"gate,omitempty"`
	// Evicted lists the cache entries a background-watcher record removed
	// (answer_of "watcher", outcome "changed"). Such a record describes a
	// change the watcher saw on a source page, not an answer anyone was given.
	Evicted []string `json:"evicted,omitempty"`
}

// answerOfWatcher marks a decision record written by the background watcher
// when a source page changed (outcome "changed", action "evicted" or "noted").
// It is not a verdict on an answer, so it stays out of the outcome and
// per-fact counts and is counted on its own.
const answerOfWatcher = "watcher"

// decisionsEnvelope is the wrapper GET /freshness/decisions returns.
type decisionsEnvelope struct {
	Count     int              `json:"count"`
	Decisions []DecisionRecord `json:"decisions"`
}

// FactStat is the per-fact outcome breakdown used for the repeat-stale
// warning: the one feature that makes this dashboard diagnostic rather than
// decorative.
type FactStat struct {
	Fact           string `json:"fact"`
	Confirmed      int    `json:"confirmed"`
	Stale          int    `json:"stale"`
	UnableToVerify int    `json:"unable_to_verify"`
	Unknown        int    `json:"unknown"`
	Contradicted   int    `json:"contradicted"`
	Total          int    `json:"total"`
}

// DecisionStats is the computed summary served by GET /api/decisions.
type DecisionStats struct {
	Total          int              `json:"total"`
	Outcomes       map[string]int   `json:"outcomes"`
	Facts          []FactStat       `json:"facts"`
	Warnings       []StaleWarning   `json:"warnings"`
	StaleThreshold int              `json:"stale_threshold"`
	Decisions      []DecisionRecord `json:"decisions"`

	// NewChecked counts fresh answers (cache misses) checked against the
	// source before reuse, and NewContradicted those the source disagreed with.
	NewChecked      int `json:"new_checked"`
	NewContradicted int `json:"new_contradicted"`
	// Corrected counts wrong answers replaced by one that matches the source,
	// Uncached those removed from the cache without a replacement.
	Corrected int `json:"corrected"`
	Uncached  int `json:"uncached"`
	// NoticeDriven counts checks forced by a "this changed" notice.
	NoticeDriven int `json:"notice_driven"`

	// SharedRefreshes counts requests that waited for a refresh already
	// running instead of starting their own.
	SharedRefreshes int `json:"shared_refreshes"`
	// StaleStopped counts stale answers that were refreshed instead of served.
	StaleStopped int `json:"stale_stopped"`
	// WrongNewStopped counts wrong new answers that were corrected or removed
	// from the cache, leaving out any record already counted in StaleStopped.
	WrongNewStopped int `json:"wrong_new_stopped"`
	// WatcherChanges counts source changes the background watcher saw
	// (answer_of "watcher"). Those records are left out of every count above
	// and of Total; they are still listed in Decisions.
	WatcherChanges int `json:"watcher_changes"`
}

// StaleWarning is one "this fact keeps going stale" alert.
type StaleWarning struct {
	Fact       string `json:"fact"`
	StaleCount int    `json:"stale_count"`
	Message    string `json:"message"`
}

// defaultStaleThreshold is how many times a fact must be caught "stale" in
// the available decision log before the dashboard raises a warning. Chosen
// low (2) on purpose: a single stale catch can be a one-off (a source that
// happened to change between the cache write and the check); a second one
// for the same fact means the cache is routinely serving that fact past its
// useful lifetime, which is worth a human looking at the TTL or the
// similarity threshold for that category.
const defaultStaleThreshold = 2

// computeDecisionStats aggregates a batch of decision records into outcome
// counts, a per-fact breakdown, and repeat-stale warnings. It is pure and
// deterministic so it can be (and is, in decisions_test.go) unit tested
// without any live services.
func computeDecisionStats(records []DecisionRecord, staleThreshold int) DecisionStats {
	if staleThreshold <= 0 {
		staleThreshold = defaultStaleThreshold
	}
	outcomes := map[string]int{
		"confirmed":        0,
		"stale":            0,
		"unable_to_verify": 0,
		"unknown":          0,
		"contradicted":     0,
	}
	byFact := map[string]*FactStat{}
	var factOrder []string

	var newChecked, newContradicted, corrected, uncached, noticeDriven, shared, staleStopped, wrongNewStopped, watcherChanges int
	for _, r := range records {
		if r.AnswerOf == answerOfWatcher {
			watcherChanges++
			continue // a page change the watcher saw, not an answer anyone got
		}
		if r.AnswerOf == "new" {
			newChecked++
			if r.Outcome == "contradicted" {
				newContradicted++
			}
		}
		switch r.Correction {
		case "corrected":
			corrected++
		case "uncached":
			uncached++
		}
		if r.SharedRefresh {
			shared++
		}
		if r.Outcome == "stale" {
			staleStopped++
		} else if r.Correction == "corrected" || r.Correction == "uncached" {
			wrongNewStopped++
		}
		if r.ChangeNotice != "" {
			noticeDriven++
		}
		if _, ok := outcomes[r.Outcome]; ok {
			outcomes[r.Outcome]++
		} else if r.Outcome != "" {
			outcomes[r.Outcome]++
		}
		if r.Fact == "" {
			continue // no watched fact applied to this decision
		}
		fs, ok := byFact[r.Fact]
		if !ok {
			fs = &FactStat{Fact: r.Fact}
			byFact[r.Fact] = fs
			factOrder = append(factOrder, r.Fact)
		}
		fs.Total++
		switch r.Outcome {
		case "confirmed":
			fs.Confirmed++
		case "stale":
			fs.Stale++
		case "unable_to_verify":
			fs.UnableToVerify++
		case "unknown":
			fs.Unknown++
		case "contradicted":
			fs.Contradicted++
		}
	}

	sort.Strings(factOrder)
	facts := make([]FactStat, 0, len(factOrder))
	var warnings []StaleWarning
	for _, name := range factOrder {
		fs := *byFact[name]
		facts = append(facts, fs)
		if fs.Stale >= staleThreshold {
			warnings = append(warnings, StaleWarning{
				Fact:       fs.Fact,
				StaleCount: fs.Stale,
				Message: "This fact (" + fs.Fact + ") has gone stale " + itoa(fs.Stale) +
					" times — consider lowering its cache TTL or similarity threshold.",
			})
		}
	}
	// Worst offenders first.
	sort.SliceStable(warnings, func(i, j int) bool {
		return warnings[i].StaleCount > warnings[j].StaleCount
	})
	sort.SliceStable(facts, func(i, j int) bool {
		return facts[i].Stale > facts[j].Stale
	})

	return DecisionStats{
		Total:          len(records) - watcherChanges,
		Outcomes:       outcomes,
		Facts:          facts,
		Warnings:       warnings,
		StaleThreshold: staleThreshold,
		Decisions:      records,

		NewChecked:      newChecked,
		NewContradicted: newContradicted,
		Corrected:       corrected,
		Uncached:        uncached,
		NoticeDriven:    noticeDriven,
		SharedRefreshes: shared,
		StaleStopped:    staleStopped,
		WrongNewStopped: wrongNewStopped,
		WatcherChanges:  watcherChanges,
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
