package openai

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPrependSystemMessage(t *testing.T) {
	in := `{ "model": "m", "temperature": 0.2, "max_tokens": 12345678901234567890, "messages": [ {"role":"system","content":"S"}, {"role":"user","content":"U"} ] }`
	out, err := PrependSystemMessage([]byte(in), `fact "quoted" <b> & more`)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model       string
		Temperature float64
		MaxTokens   json.Number `json:"max_tokens"`
		Messages    []struct{ Role, Content string }
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "m" || got.Temperature != 0.2 || got.MaxTokens.String() != "12345678901234567890" {
		t.Fatalf("other fields must be kept exactly: %s", out)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "system" || got.Messages[0].Content != `fact "quoted" <b> & more` ||
		got.Messages[1].Content != "S" || got.Messages[2].Content != "U" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	for _, bad := range []string{`not json`, `{"model":"m"}`, `{"messages":"x"}`, `[]`} {
		if _, err := PrependSystemMessage([]byte(bad), "x"); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestParseChatRequestLastUserMessage(t *testing.T) {
	body := []byte(`{
		"model": "openai/gpt-4o-mini",
		"messages": [
			{"role": "system", "content": "Mention prices only if asked."},
			{"role": "user", "content": "How much is the Pro plan?"},
			{"role": "assistant", "content": "It is $49 per month."},
			{"role": "user", "content": [
				{"type": "text", "text": "Tell me a joke"},
				{"type": "image_url", "image_url": {"url": "https://example.com/cat.png"}}
			]}
		]
	}`)
	got, err := ParseChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastUserText != "Tell me a joke" {
		t.Fatalf("LastUserText = %q, want the last user message only", got.LastUserText)
	}
	if got.Stream {
		t.Fatal("Stream should be false when absent")
	}
}

func TestParseChatRequestStream(t *testing.T) {
	got, err := ParseChatRequest([]byte(`{"stream": true, "messages": [{"role": "user", "content": "hi"}]}`))
	if err != nil || !got.Stream {
		t.Fatalf("got %+v, %v; want Stream", got, err)
	}
	if _, err := ParseChatRequest([]byte(`not json`)); err == nil {
		t.Fatal("non-JSON body: want an error")
	}
}

func TestAnswerText(t *testing.T) {
	text, err := AnswerText([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"The Acme Pro plan costs $49 per month."}}]}`))
	if err != nil || text != "The Acme Pro plan costs $49 per month." {
		t.Fatalf("got %q, %v", text, err)
	}
	if _, err := AnswerText([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"1"}]}}]}`)); !errors.Is(err, ErrNoText) {
		t.Fatalf("tool-call answer: got %v, want ErrNoText", err)
	}
	if _, err := AnswerText([]byte(`{"choices":[]}`)); err == nil {
		t.Fatal("no choices: want an error")
	}
	if _, err := AnswerText([]byte(`<html>`)); err == nil {
		t.Fatal("non-JSON: want an error")
	}
}

func TestParseChatRequestReadsToolFacts(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"status?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"{\"status\":\"shipped\"}"}],
		"tools":[{"type":"function","function":{"name":"lookup"}},{"type":"function","function":{"name":"Create_Ticket"}}],
		"functions":[{"name":"legacy_fn"}]}`
	req, err := ParseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !req.HasToolResults {
		t.Error("a role=tool message is a tool result")
	}
	if got := strings.Join(req.ToolNames, ","); got != "lookup,Create_Ticket,legacy_fn" {
		t.Errorf("tool names = %q", got)
	}
	plain, _ := ParseChatRequest([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if plain.HasToolResults || len(plain.ToolNames) != 0 {
		t.Errorf("a plain chat has no tool facts: %+v", plain)
	}
	legacy, _ := ParseChatRequest([]byte(`{"messages":[{"role":"function","name":"f","content":"x"}]}`))
	if !legacy.HasToolResults {
		t.Error("a legacy role=function message is a tool result")
	}
}

func TestParseChatRequestModelAndAllMessageTexts(t *testing.T) {
	body := `{"model":"openai/gpt-4o-mini","messages":[
	  {"role":"system","content":"sys text"},
	  {"role":"user","content":[{"type":"text","text":"part one"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"part two"}]},
	  {"role":"tool","content":{"status":"shipped"}},
	  {"role":"assistant","content":null}]}`
	req, err := ParseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "openai/gpt-4o-mini" {
		t.Fatalf("model %q", req.Model)
	}
	if len(req.MessageTexts) != 3 || req.MessageTexts[0] != "sys text" || req.MessageTexts[1] != "part one\npart two" || !strings.Contains(req.MessageTexts[2], "shipped") {
		t.Fatalf("texts = %q", req.MessageTexts)
	}
	if _, err := ParseChatRequest([]byte(`{"model":5,"messages":[]}`)); err != nil {
		t.Fatalf("a non-string model must not make the request unparseable: %v", err)
	}
}
