package main

import (
	"fmt"
	"math"
	"sort"
)

// AuditRecord mirrors the JSON shape of freshness-companion's
// internal/auditlog.Record, as served by GET /freshness/audits. Like
// DecisionRecord it is copied here, not imported, so the dashboard has no
// build-time dependency on the companion's internals. The companion never
// stores prompts or answers, so neither does this.
type AuditRecord struct {
	ID         string   `json:"id"`
	Time       string   `json:"time"`
	CacheID    string   `json:"cache_id,omitempty"`
	Similarity *float64 `json:"similarity,omitempty"`
	Threshold  *float64 `json:"threshold,omitempty"`
	Near       bool     `json:"near_threshold,omitempty"`
	Verdict    string   `json:"verdict"`
	Compare    string   `json:"compare"`
	Reason     string   `json:"reason,omitempty"`
	Error      string   `json:"error,omitempty"`
	// Gate is true when the check ran BEFORE the answer was served: a
	// "different" verdict then means the wrong answer was blocked and never
	// reached the user. Without it, a "different" verdict means a wrong answer
	// already reached the user.
	Gate      bool    `json:"gate,omitempty"`
	LatencyMS float64 `json:"latency_ms"`
}

// AuditStats mirrors the companion's counters.
type AuditStats struct {
	Scheduled int64 `json:"scheduled"`
	Completed int64 `json:"completed"`
	Same      int64 `json:"same"`
	Different int64 `json:"different"`
	Errors    int64 `json:"errors"`
	Skipped   int64 `json:"skipped"`

	// The check before serving: hits it checked and let through, trusted
	// without a new check, blocked, or could not check.
	GateVerified   int64 `json:"gate_verified"`
	GateTrusted    int64 `json:"gate_trusted"`
	GateBlocked    int64 `json:"gate_blocked"`
	GateUnverified int64 `json:"gate_unverified"`
}

// auditsEnvelope is the wrapper GET /freshness/audits returns (newest first).
type auditsEnvelope struct {
	Enabled bool          `json:"enabled"`
	Compare string        `json:"compare"`
	Stats   AuditStats    `json:"stats"`
	Count   int           `json:"count"`
	Audits  []AuditRecord `json:"audits"`
}

// Tuning constants. They are plain numbers with a stated reason, in the
// spirit of defaultStaleThreshold.
const (
	// inseparableSimilarity: a wrong match scoring this high is, to the
	// embedding model, the same question. No threshold below 1.0 stops it.
	inseparableSimilarity = 0.995
	// earlySignalBelow: under this many judged audits the rate is noise.
	earlySignalBelow = 30
	// driftWindow audits make one window; drift compares the latest window
	// with the one before it, so it needs two full windows.
	driftWindow = 10
	// driftRise is how far the wrong-match rate must climb between the two
	// windows to warn (0.20 = 20 percentage points). With 10 audits a
	// window, one or two extra wrong answers are noise; four are not.
	driftRise = 0.20
	// wrongRateTolerance is the rate above which the dashboard warns even
	// without a rise. 5% is a stated assumption, not a standard.
	wrongRateTolerance = 0.05
	// tolerateMinJudged: do not apply the tolerance on fewer audits.
	tolerateMinJudged = 10
)

// AuditPoint is one audited hit, for the similarity chart (oldest first).
type AuditPoint struct {
	ID         string   `json:"id"`
	Similarity *float64 `json:"similarity,omitempty"`
	Threshold  *float64 `json:"threshold,omitempty"`
	Verdict    string   `json:"verdict"`
	Near       bool     `json:"near_threshold,omitempty"`
	Gate       bool     `json:"gate,omitempty"`
}

// ThresholdAdvice is what the audited hits say about the similarity cutoff.
type ThresholdAdvice struct {
	// Kind: ok | raise | unfixable | raise_partial
	Kind    string `json:"kind"`
	Message string `json:"message"`
	// SuggestedThreshold is the smallest cutoff (to 0.01) that would have
	// blocked every wrong match a cutoff can block.
	SuggestedThreshold *float64 `json:"suggested_threshold,omitempty"`
	WrongBlocked       int      `json:"wrong_blocked"`
	WrongTotal         int      `json:"wrong_total"`
	RightLost          int      `json:"right_lost"`
	RightTotal         int      `json:"right_total"`
	// Inseparable counts wrong matches no cutoff can block.
	Inseparable int `json:"inseparable"`
	// NextRight is the lowest-scoring good hit that stays above the
	// suggested cutoff: a small gap to it means a thin margin.
	NextRight *float64 `json:"next_right,omitempty"`
	// SamplesServed and SamplesGate say where the advice's samples came from:
	// audits made after an answer was served, and checks made before.
	SamplesServed int `json:"samples_served"`
	SamplesGate   int `json:"samples_gate"`
}

// DriftWarning is raised when the wrong-match rate has climbed.
type DriftWarning struct {
	Message  string  `json:"message"`
	Earlier  float64 `json:"earlier_rate"`
	Latest   float64 `json:"latest_rate"`
	Window   int     `json:"window"`
	Tolerate bool    `json:"above_tolerance"`
}

// AuditAnalysis is the computed summary served by GET /api/audits.
type AuditAnalysis struct {
	Enabled bool       `json:"enabled"`
	Compare string     `json:"compare,omitempty"`
	Stats   AuditStats `json:"stats"`
	// Judged, Same, Different, Errors and WrongRate count only audits made
	// AFTER the answer was served, so Different is wrong answers that reached
	// users. Checks made before serving are counted separately below.
	Judged    int `json:"judged"`
	Same      int `json:"same"`
	Different int `json:"different"`
	Errors    int `json:"errors"`
	// GateJudged is checks made before serving that gave a verdict: GateSame
	// were right matches (served), GateDifferent were wrong matches BLOCKED.
	// GateErrors could not be completed (the cached answer was served unchecked).
	GateJudged    int              `json:"gate_judged"`
	GateSame      int              `json:"gate_same"`
	GateDifferent int              `json:"gate_different"`
	GateErrors    int              `json:"gate_errors"`
	WrongRate     float64          `json:"wrong_rate"`
	EarlySignal   bool             `json:"early_signal"`
	Threshold     *float64         `json:"threshold,omitempty"`
	Points        []AuditPoint     `json:"points"`
	Advice        *ThresholdAdvice `json:"advice,omitempty"`
	Drift         *DriftWarning    `json:"drift,omitempty"`
	Audits        []AuditRecord    `json:"audits"`
}

// analyzeAudits turns the companion's audit records (newest first) into the
// wrong-match rate, a threshold suggestion and a drift warning. It is pure and
// deterministic, so it is unit tested without any live service.
func analyzeAudits(env auditsEnvelope) AuditAnalysis {
	out := AuditAnalysis{Enabled: env.Enabled, Compare: env.Compare, Stats: env.Stats, Points: []AuditPoint{}, Audits: env.Audits}
	if out.Audits == nil {
		out.Audits = []AuditRecord{}
	}
	// Oldest first, so charts and the drift windows read in time order.
	ordered := make([]AuditRecord, len(env.Audits))
	for i, r := range env.Audits {
		ordered[len(env.Audits)-1-i] = r
	}
	var served, all []AuditRecord // served: audits after serving; all: both kinds
	for _, r := range ordered {
		if r.Verdict != "same" && r.Verdict != "different" {
			if r.Gate {
				out.GateErrors++
			} else {
				out.Errors++
			}
			continue
		}
		switch {
		case r.Gate && r.Verdict == "same":
			out.GateSame++
		case r.Gate:
			out.GateDifferent++
		case r.Verdict == "same":
			out.Same++
		default:
			out.Different++
		}
		if r.Gate {
			out.GateJudged++
		} else {
			served = append(served, r)
		}
		all = append(all, r)
		out.Points = append(out.Points, AuditPoint{ID: r.ID, Similarity: r.Similarity, Threshold: r.Threshold, Verdict: r.Verdict, Near: r.Near, Gate: r.Gate})
		if r.Threshold != nil {
			t := *r.Threshold
			out.Threshold = &t
		}
	}
	out.Judged = len(served)
	if len(all) == 0 {
		return out
	}
	if out.Judged > 0 {
		out.WrongRate = float64(out.Different) / float64(out.Judged)
		out.Drift = detectDrift(served)
	}
	out.EarlySignal = out.Judged < earlySignalBelow
	out.Advice = adviseThreshold(all, out.Threshold)
	out.Advice.SamplesServed = out.Judged
	out.Advice.SamplesGate = out.GateJudged
	out.Advice.Message += samplesNote(out.Judged, out.GateJudged)
	return out
}

// samplesNote says where the samples behind the threshold advice came from.
func samplesNote(served, gate int) string {
	return fmt.Sprintf(" Based on %d audits of answers already served and %d checks made before serving.", served, gate)
}

func adviseThreshold(judged []AuditRecord, current *float64) *ThresholdAdvice {
	var sepWrong, right []float64
	adv := &ThresholdAdvice{}
	for _, r := range judged {
		switch {
		case r.Verdict == "same":
			adv.RightTotal++
			if r.Similarity != nil {
				right = append(right, *r.Similarity)
			}
		case r.Similarity != nil && *r.Similarity >= inseparableSimilarity:
			adv.WrongTotal++
			adv.Inseparable++
		default:
			adv.WrongTotal++
			if r.Similarity != nil {
				sepWrong = append(sepWrong, *r.Similarity)
			}
		}
	}
	sort.Float64s(right)

	if adv.WrongTotal == 0 {
		adv.Kind = "ok"
		adv.Message = fmt.Sprintf("All %d audited hits were right.", adv.RightTotal)
		if len(right) > 0 {
			adv.Message += fmt.Sprintf(" The lowest scored %.2f.", right[0])
		}
		return adv
	}

	if len(sepWrong) > 0 {
		m := sepWrong[0]
		for _, s := range sepWrong {
			m = math.Max(m, s)
		}
		t := math.Round((math.Floor(m*100)/100+0.01)*100) / 100
		for t <= m {
			t = math.Round((t+0.01)*100) / 100
		}
		if t <= 1 {
			adv.SuggestedThreshold = &t
			adv.WrongBlocked = len(sepWrong)
			for _, s := range right {
				if s < t {
					adv.RightLost++
				} else if adv.NextRight == nil {
					v := s
					adv.NextRight = &v
				}
			}
		}
	}

	switch {
	case adv.SuggestedThreshold != nil && adv.Inseparable == 0:
		adv.Kind = "raise"
	case adv.SuggestedThreshold != nil:
		adv.Kind = "raise_partial"
	default:
		adv.Kind = "unfixable"
	}

	from := ""
	if current != nil {
		from = fmt.Sprintf(" from %.2f", *current)
	}
	switch adv.Kind {
	case "raise", "raise_partial":
		adv.Message = fmt.Sprintf("Raising the threshold%s to %.2f would have blocked %d of %d wrong matches and lost %d of %d good hits.",
			from, *adv.SuggestedThreshold, adv.WrongBlocked, adv.WrongTotal, adv.RightLost, adv.RightTotal)
		if adv.NextRight != nil && *adv.NextRight-*adv.SuggestedThreshold < 0.01 {
			adv.Message += fmt.Sprintf(" The closest good hit scored %.4f, a thin margin: audit more before changing it.", *adv.NextRight)
		}
		if adv.Inseparable > 0 {
			adv.Message += fmt.Sprintf(" %d more scored %.2f or higher: the embedding cannot tell those questions apart, so no threshold fixes them.", adv.Inseparable, inseparableSimilarity)
		}
	default:
		adv.Message = fmt.Sprintf("%d of %d wrong matches scored %.2f or higher: the embedding cannot tell those questions apart, so no threshold fixes them. Cache those question types by exact match only, or use a better embedding model.",
			adv.Inseparable, adv.WrongTotal, inseparableSimilarity)
	}
	return adv
}

// detectDrift compares the wrong-match rate of the latest window of audits
// with the window before it. judged is oldest first.
func detectDrift(judged []AuditRecord) *DriftWarning {
	rate := func(rs []AuditRecord) float64 {
		bad := 0
		for _, r := range rs {
			if r.Verdict == "different" {
				bad++
			}
		}
		return float64(bad) / float64(len(rs))
	}
	if len(judged) < driftWindow {
		return nil
	}
	latest := rate(judged[len(judged)-driftWindow:])
	w := &DriftWarning{Window: driftWindow, Latest: latest, Tolerate: len(judged) >= tolerateMinJudged && latest > wrongRateTolerance}
	if len(judged) >= 2*driftWindow {
		w.Earlier = rate(judged[len(judged)-2*driftWindow : len(judged)-driftWindow])
		if latest-w.Earlier >= driftRise {
			w.Message = fmt.Sprintf("The wrong-match rate rose from %.0f%% to %.0f%% between the last two sets of %d audits. The questions people ask may have changed: review the threshold.",
				w.Earlier*100, latest*100, driftWindow)
			return w
		}
	}
	if w.Tolerate {
		w.Message = fmt.Sprintf("%.0f%% of the latest %d audited hits were wrong matches, above the %.0f%% tolerance.", latest*100, driftWindow, wrongRateTolerance*100)
		return w
	}
	return nil
}
