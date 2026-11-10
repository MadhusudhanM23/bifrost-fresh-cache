// Package openai reads the few fields of OpenAI-compatible chat completion
// bodies that the companion needs. It never modifies a body.
package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// ChatRequest is what the companion needs from a POST /v1/chat/completions body.
type ChatRequest struct {
	// Stream is true when the request asks for server-sent events.
	Stream bool
	// LastUserText is the text of the last message with role "user".
	LastUserText string
	// HasToolResults is true when a message carries a tool's result (role
	// "tool" or "function"): the answer then depends on live tool data.
	HasToolResults bool
	// ToolNames are the names of the tools the request offers the model.
	ToolNames []string
	// Model is the request's "model" field (echoed by verified answers).
	Model string
	// MessageTexts is the text of every message, all roles, one entry per
	// message (string content or the text parts of a content array).
	MessageTexts []string
}

// ParseChatRequest reads the stream flag and the last user message.
func ParseChatRequest(body []byte) (ChatRequest, error) {
	var req struct {
		Model    json.RawMessage `json:"model"`
		Stream   *bool           `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Functions []struct {
			Name string `json:"name"`
		} `json:"functions"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ChatRequest{}, errors.New("request body is not a JSON chat completion request")
	}
	out := ChatRequest{Stream: req.Stream != nil && *req.Stream}
	_ = json.Unmarshal(req.Model, &out.Model)
	for _, m := range req.Messages {
		t := contentText(m.Content)
		if r := bytes.TrimSpace(m.Content); t == "" && len(r) > 0 && r[0] == '{' {
			t = string(r) // structured content (e.g. a tool result object): scan it as text
		}
		if t != "" {
			out.MessageTexts = append(out.MessageTexts, t)
		}
		if strings.EqualFold(m.Role, "tool") || strings.EqualFold(m.Role, "function") {
			out.HasToolResults = true
		}
	}
	for _, t := range req.Tools {
		if t.Function.Name != "" {
			out.ToolNames = append(out.ToolNames, t.Function.Name)
		}
	}
	for _, f := range req.Functions {
		if f.Name != "" {
			out.ToolNames = append(out.ToolNames, f.Name)
		}
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if strings.EqualFold(req.Messages[i].Role, "user") {
			out.LastUserText = contentText(req.Messages[i].Content)
			break
		}
	}
	return out, nil
}

// PrependSystemMessage returns body with a system message holding text put
// first in "messages". Every other field is kept as it was (values are
// copied as raw JSON, so numbers and nesting are unchanged).
func PrependSystemMessage(body []byte, text string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, errors.New("request body is not a JSON object")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(top["messages"], &messages); err != nil {
		return nil, errors.New("request has no messages array")
	}
	msg, err := json.Marshal(map[string]string{"role": "system", "content": text})
	if err != nil {
		return nil, err
	}
	top["messages"], err = json.Marshal(append([]json.RawMessage{msg}, messages...))
	if err != nil {
		return nil, err
	}
	return json.Marshal(top)
}

// ErrNoText is returned when choices[0].message.content holds no text
// (for example a tool-call-only answer).
var ErrNoText = errors.New("choices[0].message.content has no text")

// AnswerText returns choices[0].message.content from a chat completion
// response body. Errors never quote the body.
func AnswerText(body []byte) (string, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", errors.New("response body is not a JSON chat completion")
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("response has no choices")
	}
	text := contentText(resp.Choices[0].Message.Content)
	if strings.TrimSpace(text) == "" {
		return "", ErrNoText
	}
	return text, nil
}

// contentText accepts a plain string or an array of content parts and
// returns the joined text parts.
func contentText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "", "text", "input_text", "output_text":
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
	}
	return strings.Join(texts, "\n")
}
