package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIClientOrdersByIndexAndSendsTheKey(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Answer out of order: the client must place vectors by "index".
		data := []map[string]any{}
		for i := len(req.Input) - 1; i >= 0; i-- {
			data = append(data, map[string]any{"index": i, "embedding": []float32{float32(i), 1}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	client := &OpenAI{BaseURL: server.URL + "/v1", APIKey: "k", Model: "m", BatchSize: 2}
	vectors, err := client.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 3 || vectors[0][0] != 0 || vectors[1][0] != 1 || vectors[2][0] != 0 {
		// batches of 2: [a b] -> indexes 0,1; [c] -> index 0
		t.Fatalf("vectors out of order: %v", vectors)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("Authorization header %q", gotAuth)
	}
}

func TestOpenAIClientReportsHTTPErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	_, err := (&OpenAI{BaseURL: server.URL, Model: "m"}).Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected an error for HTTP 401")
	}
}

func TestHashEmbedderIsDeterministicAndUnit(t *testing.T) {
	h := Hash{Dim: 64}
	a, _ := h.Embed(context.Background(), []string{"refund a late order", "refund a late order"})
	var norm float64
	for i := range a[0] {
		if a[0][i] != a[1][i] {
			t.Fatal("same text gave different vectors")
		}
		norm += float64(a[0][i] * a[0][i])
	}
	if norm < 0.999 || norm > 1.001 {
		t.Fatalf("norm %v", norm)
	}
}
