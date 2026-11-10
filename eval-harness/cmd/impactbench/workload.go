package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
)

// Phases of one arm's run.
const (
	PhaseWarm  = "warm"  // page says the base price; the cache is populated
	PhaseAfter = "after" // page says the changed price; traffic now meets old entries
	PhaseBurst = "burst" // page says the burst price; N concurrent identical asks
)

// Kinds of request. The kind decides how an answer is judged.
const (
	KindPrice  = "price"  // answer states a price taken from the system context
	KindFAQ    = "faq"    // look-alike pair member or a paraphrase of one
	KindBenign = "benign" // an ordinary question with a canned answer
	KindTool   = "tool"   // the request carries a role "tool" message
)

// Item is one request of the workload. The same list is replayed, unchanged,
// against every arm of a run.
type Item struct {
	ID       string `json:"id"`
	Phase    string `json:"phase"`
	Kind     string `json:"kind"`
	Scenario string `json:"scenario"`
	Question string `json:"question"`
	// Price is the monthly price the app puts into the system message (a RAG
	// app that reads the page at request time). 0 = no system message.
	Price int `json:"price,omitempty"`
	// Tool is the content of a role "tool" message ("" = none).
	Tool string `json:"tool,omitempty"`
}

// Scenario names used in the per-scenario table.
const (
	ScWarm        = "warm (all kinds)"
	ScPriceRepeat = "price: same wording as before the change"
	ScPriceNew    = "price: new wording after the change"
	ScLookPass    = "look-alike: password vs username (second of pair first)"
	ScLookCSV     = "look-alike: export vs import CSV (second of pair first)"
	Sc2FA         = "look-alike: two-factor on vs off (second of pair first)"
	ScFAQPara     = "same-meaning paraphrase of a FAQ"
	ScBenign      = "benign question, same meaning"
	ScTool        = "request carries a tool result"
	ScBurst       = "burst: concurrent identical price asks"
)

// lookAlike is a pair of questions that mean different things.
type lookAlike struct {
	Name        string
	Scenario    string
	A, B        string // A is asked first when warming, B first after the change
	AParaphrase string // a same-meaning rewording of A, asked after the change
}

var lookAlikes = []lookAlike{
	{"password", ScLookPass,
		"How do I reset my Acme account password from the mobile app?",
		"How do I reset my Acme account username from the mobile app?",
		"How can I reset the password for my Acme account on the mobile app?"},
	{"csv", ScLookCSV,
		"Can I export my project to CSV?",
		"Can I import my project from CSV?",
		"Is it possible to export a project as CSV?"},
	{"two-factor", Sc2FA,
		"How do I turn on two-factor login?",
		"How do I turn off two-factor login?",
		""}, // the paraphrase of "turn on" would not teach anything new
}

// Price questions. Warm uses all of priceWarm; after the change the same
// wordings come back (a repeat), together with the new wordings.
var priceWarm = []string{
	"How much does the Acme Pro plan cost per month?",
	"What does the Acme Pro plan cost monthly?",
	"Acme Pro plan price per month?",
	"How much is the Acme Pro plan each month?",
	"What is the price of the Acme Pro plan?",
}

var priceNew = []string{
	"How much is the Acme Pro plan per month?",
	"What's the monthly cost of the Acme Pro plan?",
}

// Benign questions with canned answers (docker/mock-knowledge.json).
var (
	benignWarm  = []string{"At what temperature does water boil?", "What is the chemical symbol for gold?"}
	benignAfter = []string{"What temperature does water boil at?", "What is the boiling point of water?", "Which chemical symbol stands for gold?"}
)

// Tool-using asks: the same question twice, the tool result changed in between.
var toolAsks = []struct{ Question, First, Second string }{
	{"What is the status of my order?", `{"status":"shipped"}`, `{"status":"delivered"}`},
	{"What is my account balance?", `{"balance":120.50}`, `{"balance":95.00}`},
}

// WorkloadConfig sets the three prices and the burst size.
type WorkloadConfig struct {
	BasePrice    int
	ChangedPrice int
	BurstPrice   int
	Burst        int
}

// BuildWorkload returns the request list for one seed. Everything random is a
// shuffle of independent items drawn from a generator seeded with seed, so the
// same seed always gives the same list and the same list is sent to every arm.
func BuildWorkload(seed int64, cfg WorkloadConfig) []Item {
	rng := rand.New(rand.NewSource(seed))
	var items []Item
	seq := map[string]int{}
	add := func(it Item) {
		seq[it.Phase]++
		it.ID = fmt.Sprintf("%s-%02d", it.Phase, seq[it.Phase])
		items = append(items, it)
	}
	shuffled := func(in []string) []string {
		out := append([]string(nil), in...)
		rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	pairOrder := func() []lookAlike {
		out := append([]lookAlike(nil), lookAlikes...)
		rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}

	// 1. WARM: price questions, then each look-alike pair (A then B), then benign.
	for _, q := range shuffled(priceWarm) {
		add(Item{Phase: PhaseWarm, Kind: KindPrice, Scenario: ScWarm, Question: q, Price: cfg.BasePrice})
	}
	for _, p := range pairOrder() {
		add(Item{Phase: PhaseWarm, Kind: KindFAQ, Scenario: ScWarm, Question: p.A})
		add(Item{Phase: PhaseWarm, Kind: KindFAQ, Scenario: ScWarm, Question: p.B})
	}
	for _, q := range benignWarm {
		add(Item{Phase: PhaseWarm, Kind: KindBenign, Scenario: ScWarm, Question: q})
	}

	// 3. AFTER-CHANGE (the app now sends the changed price).
	//    Round 1: the warm wordings again plus new wordings, shuffled together.
	round1 := shuffled(append(append([]string(nil), priceWarm...), priceNew...))
	isNew := map[string]bool{}
	for _, q := range priceNew {
		isNew[q] = true
	}
	for _, q := range round1 {
		sc := ScPriceRepeat
		if isNew[q] {
			sc = ScPriceNew
		}
		add(Item{Phase: PhaseAfter, Kind: KindPrice, Scenario: sc, Question: q, Price: cfg.ChangedPrice})
	}
	//    Look-alikes in the opposite order: B first, then A.
	for _, p := range pairOrder() {
		add(Item{Phase: PhaseAfter, Kind: KindFAQ, Scenario: p.Scenario, Question: p.B})
		add(Item{Phase: PhaseAfter, Kind: KindFAQ, Scenario: p.Scenario, Question: p.A})
	}
	//    Same-meaning rewordings of FAQ answers and of benign questions.
	var paras []string
	for _, p := range lookAlikes {
		if p.AParaphrase != "" {
			paras = append(paras, p.AParaphrase)
		}
	}
	for _, q := range shuffled(paras) {
		add(Item{Phase: PhaseAfter, Kind: KindFAQ, Scenario: ScFAQPara, Question: q})
	}
	for _, q := range shuffled(benignAfter) {
		add(Item{Phase: PhaseAfter, Kind: KindBenign, Scenario: ScBenign, Question: q})
	}
	//    Round 2 of the price questions (the warm wordings, shuffled).
	for _, q := range shuffled(priceWarm) {
		add(Item{Phase: PhaseAfter, Kind: KindPrice, Scenario: ScPriceRepeat, Question: q, Price: cfg.ChangedPrice})
	}
	//    Tool-result asks: the same question twice, the tool result changed.
	askOrder := rng.Perm(len(toolAsks))
	for _, i := range askOrder {
		a := toolAsks[i]
		add(Item{Phase: PhaseAfter, Kind: KindTool, Scenario: ScTool, Question: a.Question, Tool: a.First})
		add(Item{Phase: PhaseAfter, Kind: KindTool, Scenario: ScTool, Question: a.Question, Tool: a.Second})
	}

	// 4. BURST: N identical asks (the first warm wording) at the burst price.
	for i := 0; i < cfg.Burst; i++ {
		add(Item{Phase: PhaseBurst, Kind: KindPrice, Scenario: ScBurst, Question: priceWarm[0], Price: cfg.BurstPrice})
	}
	return items
}

type chatMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// BodyOptions change the request body for a real model. The zero value gives
// exactly the body the mock run used.
type BodyOptions struct {
	// MaxTokens is sent as max_tokens when > 0. Reasoning models (such as
	// gpt-oss) need room to think or they return an empty answer.
	MaxTokens int
	// ToolCallMessage puts an assistant message with a tool_calls entry before
	// the role "tool" message. Real providers reject a tool result that does
	// not answer a tool call; the mock accepts it either way.
	ToolCallMessage bool
}

// systemFor is the system message of a price request, in the Context: list
// format that tools/mock-openai needs to state the price (see DOCKER.md).
func systemFor(price int) string {
	return fmt.Sprintf("You answer questions about Acme.\nContext:\n- The Acme Pro plan costs $%d per month.\nAnswer in one sentence and state the monthly price.", price)
}

// Body is the JSON request body of the item, as the mock run sent it.
func (it Item) Body(model string) []byte { return it.BodyWith(model, BodyOptions{}) }

// BodyWith is the JSON request body of the item with the given options.
func (it Item) BodyWith(model string, opt BodyOptions) []byte {
	msgs := []chatMsg{}
	if it.Price > 0 {
		msgs = append(msgs, chatMsg{Role: "system", Content: systemFor(it.Price)})
	}
	msgs = append(msgs, chatMsg{Role: "user", Content: it.Question})
	if it.Tool != "" {
		if opt.ToolCallMessage {
			tc := toolCall{ID: "call_1", Type: "function"}
			tc.Function.Name, tc.Function.Arguments = "lookup", "{}"
			msgs = append(msgs, chatMsg{Role: "assistant", ToolCalls: []toolCall{tc}})
		}
		msgs = append(msgs, chatMsg{Role: "tool", ToolCallID: "call_1", Content: it.Tool})
	}
	b, _ := json.Marshal(struct {
		Model     string    `json:"model"`
		Messages  []chatMsg `json:"messages"`
		MaxTokens int       `json:"max_tokens,omitempty"`
	}{model, msgs, opt.MaxTokens})
	return b
}

// MeaningGroup names what a question asks: two questions in the same group
// want the same answer (a FAQ question and its same-meaning rewording, or two
// wordings of the same ordinary question). The two questions of a look-alike
// pair are in different groups. Unknown questions are their own group.
func MeaningGroup(q string) string {
	for _, p := range lookAlikes {
		if q == p.A || (p.AParaphrase != "" && q == p.AParaphrase) {
			return p.Name + ":A"
		}
		if q == p.B {
			return p.Name + ":B"
		}
	}
	for _, w := range append(append([]string(nil), priceWarm...), priceNew...) {
		if q == w {
			return "price"
		}
	}
	switch q {
	case benignWarm[0], benignAfter[0], benignAfter[1]:
		return "benign:boiling-water"
	case benignWarm[1], benignAfter[2]:
		return "benign:gold-symbol"
	}
	return "question:" + q
}

// PhaseCounts counts items per phase.
func PhaseCounts(items []Item) map[string]int {
	out := map[string]int{}
	for _, it := range items {
		out[it.Phase]++
	}
	return out
}
