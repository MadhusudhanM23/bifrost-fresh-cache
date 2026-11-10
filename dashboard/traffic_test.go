package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeScriptKeepsItsQuestionsAndAddsOneToolScenario(t *testing.T) {
	tools := 0
	for _, q := range probeScript {
		if q.kind == kindToolResult {
			tools++
		}
	}
	if len(probeScript) != 18 || tools != 1 {
		t.Fatalf("script has %d rows, %d tool scenarios; want the 17 questions plus 1", len(probeScript), tools)
	}
	if probeScript[0].text != "How do I reset my Acme account password from the mobile app?" || probeScript[16].kind != "lookalike" {
		t.Fatalf("the original 17 questions must keep their order: %+v", probeScript[:17])
	}
}

func TestToolResultMessagesCarryAToolRole(t *testing.T) {
	msgs := toolResultMessages("q")
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || msgs[0].(map[string]any)["content"] != "q" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestSendProbeReadsGateSharedAndGuardHeaders(t *testing.T) {
	var gotRoles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotRoles = nil
		for _, m := range body.Messages {
			gotRoles = append(gotRoles, m.Role)
		}
		w.Header().Set("X-Freshness-Gate", "blocked")
		w.Header().Set("X-Freshness-Shared-Refresh", "true")
		w.Header().Set("X-Freshness-Guard", "bypassed: tool_results")
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":5}}`))
	}))
	defer srv.Close()

	ev, _, _, err := sendProbe(context.Background(), srv.Client(), srv.URL, probeQuestion{"q", "lookalike"})
	if err != nil || ev.Gate != "blocked" || !ev.SharedRefresh || ev.Guard != "bypassed: tool_results" || ev.HitType != "miss" {
		t.Fatalf("event = %+v, err %v", ev, err)
	}
	if len(gotRoles) != 1 || gotRoles[0] != "user" {
		t.Fatalf("plain questions send one user message, got %v", gotRoles)
	}
	if _, _, _, err := sendProbe(context.Background(), srv.Client(), srv.URL, probeQuestion{"q", kindToolResult}); err != nil {
		t.Fatal(err)
	}
	if len(gotRoles) != 3 || gotRoles[2] != "tool" {
		t.Fatalf("the tool scenario must send a tool message, got %v", gotRoles)
	}
}
