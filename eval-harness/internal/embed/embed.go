// Package embed turns text into vectors for the cache-quality harness.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Embedder turns texts into vectors, one per text, in order.
type Embedder interface {
	// Name describes the embedder for the results file.
	Name() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// OpenAI calls an OpenAI-compatible POST {BaseURL}/embeddings endpoint: OpenAI
// itself, or anything that speaks the same API (a local mock, a gateway).
type OpenAI struct {
	BaseURL    string // for example https://api.openai.com/v1
	APIKey     string // sent as "Authorization: Bearer"; may be empty for a local server
	Model      string
	Dimensions int // optional "dimensions" request field; 0 leaves it out
	BatchSize  int // texts per request (default 64)
	Client     *http.Client
}

// Name implements Embedder.
func (o *OpenAI) Name() string {
	name := fmt.Sprintf("openai-compatible model=%s base_url=%s", o.Model, o.BaseURL)
	if o.Dimensions > 0 {
		name += fmt.Sprintf(" dimensions=%d", o.Dimensions)
	}
	return name
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed implements Embedder.
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	batch := o.BatchSize
	if batch <= 0 {
		batch = 64
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batch {
		end := min(start+batch, len(texts))
		vectors, err := o.embedBatch(ctx, client, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("texts %d..%d: %w", start, end-1, err)
		}
		out = append(out, vectors...)
	}
	return out, nil
}

func (o *OpenAI) embedBatch(ctx context.Context, client *http.Client, texts []string) ([][]float32, error) {
	body := map[string]any{"model": o.Model, "input": texts, "encoding_format": "float"}
	if o.Dimensions > 0 {
		body["dimensions"] = o.Dimensions
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(o.BaseURL, "/") + "/embeddings"
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if o.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+o.APIKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, excerpt(raw))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, excerpt(raw))
		}
		var parsed embeddingResponse
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if parsed.Error != nil {
			return nil, fmt.Errorf("API error: %s", parsed.Error.Message)
		}
		if len(parsed.Data) != len(texts) {
			return nil, fmt.Errorf("asked for %d embeddings, got %d", len(texts), len(parsed.Data))
		}
		vectors := make([][]float32, len(texts))
		for _, d := range parsed.Data {
			if d.Index < 0 || d.Index >= len(texts) || vectors[d.Index] != nil || len(d.Embedding) == 0 {
				return nil, fmt.Errorf("malformed embedding at index %d", d.Index)
			}
			vectors[d.Index] = d.Embedding
		}
		return vectors, nil
	}
	return nil, lastErr
}

func excerpt(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// LooksLikeMockServer reports whether baseURL belongs to the local mock OpenAI
// server (tools/mock-openai), which answers GET /mock/stats. A result produced
// with it must never be read as embedding quality.
func LooksLikeMockServer(ctx context.Context, baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return false
	}
	probe := url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/mock/stats"}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.String(), nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Hash is a tiny deterministic in-process embedder: lowercase words hashed into
// Dim signed buckets, L2-normalised. It is NOT a semantic model (texts are close
// only when they share words); it exists so the harness can be exercised with
// no server and no API key.
type Hash struct{ Dim int }

// Name implements Embedder.
func (h Hash) Name() string {
	return fmt.Sprintf("in-process word hashing, dim=%d (NOT a semantic model)", h.Dim)
}

// Embed implements Embedder.
func (h Hash) Embed(_ context.Context, texts []string) ([][]float32, error) {
	dim := h.Dim
	if dim <= 0 {
		dim = 256
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float64, dim)
		words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		for _, w := range words {
			f := fnv.New64a()
			_, _ = f.Write([]byte(w))
			sum := f.Sum64()
			sign := 1.0
			if sum>>63 == 1 {
				sign = -1
			}
			v[sum%uint64(dim)] += sign
		}
		var norm float64
		for _, x := range v {
			norm += x * x
		}
		norm = math.Sqrt(norm)
		vec := make([]float32, dim)
		for j, x := range v {
			if norm > 0 {
				vec[j] = float32(x / norm)
			}
		}
		if norm == 0 {
			vec[0] = 1
		}
		out[i] = vec
	}
	return out, nil
}
