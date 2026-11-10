// Package mcpserver exposes the companion's freshness checks as a small MCP
// (Model Context Protocol) server, so an MCP client such as Bifrost's MCP
// gateway can call them as tools.
//
// It speaks the plain-HTTP shape of the Streamable HTTP transport and nothing
// more: one JSON-RPC 2.0 request per POST, one JSON response, no server-sent
// events and no session ids. That is enough for a client that only lists and
// calls tools. The transport is the standard library only.
//
// The package does not import the proxy: the proxy hands it the pieces it
// needs through Deps (the policy, the evidence cache, an explain lookup, the
// change-notice store and the function that applies a change notice).
package mcpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freshness-companion/internal/changes"
	"freshness-companion/internal/decision"
	"freshness-companion/internal/discover"
	"freshness-companion/internal/policy"
)

// Discoverer is discovery as the tools use it (normally *discover.Service):
// find a page for a question no watched fact covers, and list what was
// learned.
type Discoverer interface {
	decision.Discoverer
	Sources() []discover.Learned
}

// Protocol versions this server understands, oldest first. The server's
// own cap (Options.MaxProtocolVersion) picks how far up this list it goes.
var knownVersions = []string{"2025-06-18", "2025-11-25", "2026-07-28"}

// DefaultProtocolVersion is the version the server speaks by default. It is
// also the fallback offered to a client that asks for a version the server
// does not know.
const DefaultProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// maxBody bounds one request body.
const maxBody = 1 << 20

// NoticeTokenHeader is the second header the change-notice token is
// accepted in, for clients whose Authorization header is taken.
const NoticeTokenHeader = "X-Freshness-Notice-Token"

// ChangeRequest is a change notice as the report_change tool receives it.
// It mirrors the body of POST /freshness/changes.
type ChangeRequest struct {
	Fact           string
	Category       string
	SourceURL      string
	Note           string
	ClearCacheKeys []string
}

// Deps are the companion pieces the tools work on.
type Deps struct {
	// Policy is required: it classifies questions and lists the watched facts.
	Policy *policy.Policy
	// Evidence returns the current page for a source URL (the evidence cache).
	// nil makes check_freshness report unable_to_verify for every fact.
	Evidence decision.EvidenceSource
	// Explain looks up one request explanation by id: the plain-words text
	// and the full record. nil means explanations are switched off.
	Explain func(id string) (plain string, record any, ok bool)
	// ExplainKept is how many explanations the companion remembers, for the
	// "not found" message.
	ExplainKept int
	// Changes lists recorded change notices. Optional.
	Changes interface {
		Recent(limit int) []changes.Notice
	}
	// Report applies a change notice exactly like POST /freshness/changes
	// does. Optional: nil disables report_change with a clear message.
	Report func(ctx context.Context, req ChangeRequest) (changes.Notice, error)
	// NoticeToken, when set, must accompany a report_change call, either as
	// "Authorization: Bearer <token>" or in the X-Freshness-Notice-Token
	// header. It is the same token POST /freshness/changes requires.
	NoticeToken string
	// Discover, when set, lets check_freshness find a source page with
	// Tavily Search for a question that no watched fact covers, and lets
	// list_watched_sources show the learned sources. Optional.
	Discover Discoverer
}

// Options name the server.
type Options struct {
	Name    string // serverInfo.name (default "freshness-companion")
	Version string // serverInfo.version (default "0.1")
	// MaxProtocolVersion caps negotiation (default DefaultProtocolVersion).
	MaxProtocolVersion string
	Now                func() time.Time
}

// Server is the MCP HTTP handler.
type Server struct {
	deps      Deps
	opts      Options
	supported []string // protocol versions offered, oldest first
	tools     []tool
}

// New returns a server. Deps.Policy is required.
func New(deps Deps, opts Options) (*Server, error) {
	if deps.Policy == nil {
		return nil, errors.New("mcpserver: Deps.Policy is required")
	}
	if opts.Name == "" {
		opts.Name = "freshness-companion"
	}
	if opts.Version == "" {
		opts.Version = "0.1"
	}
	if opts.MaxProtocolVersion == "" {
		opts.MaxProtocolVersion = DefaultProtocolVersion
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	var supported []string
	for _, v := range knownVersions {
		supported = append(supported, v)
		if v == opts.MaxProtocolVersion {
			break
		}
	}
	if supported[len(supported)-1] != opts.MaxProtocolVersion {
		return nil, fmt.Errorf("mcpserver: unknown protocol version %q (know %s)", opts.MaxProtocolVersion, strings.Join(knownVersions, ", "))
	}
	s := &Server{deps: deps, opts: opts, supported: supported}
	s.tools = s.toolList()
	return s, nil
}

// ToolNames returns the tool names, in listing order.
func (s *Server) ToolNames() []string {
	out := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.name)
	}
	return out
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ServeHTTP handles POST /mcp. Mount it with a method-qualified pattern so
// GET answers 405.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "the MCP endpoint takes POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeRPC(w, response{JSONRPC: "2.0", ID: null, Error: &rpcError{Code: codeParse, Message: "could not read the request body"}})
		return
	}
	if len(body) > maxBody {
		writeRPC(w, response{JSONRPC: "2.0", ID: null, Error: &rpcError{Code: codeInvalidRequest, Message: "request body is larger than 1 MiB"}})
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		msg := "parse error: the body must be one JSON-RPC 2.0 object"
		if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			msg = "parse error: batches are not supported, send one JSON-RPC object per request"
		}
		writeRPC(w, response{JSONRPC: "2.0", ID: null, Error: &rpcError{Code: codeParse, Message: msg}})
		return
	}
	if req.Method == "" {
		writeRPC(w, response{JSONRPC: "2.0", ID: idOrNull(req.ID), Error: &rpcError{Code: codeInvalidRequest, Message: "missing method"}})
		return
	}
	// A notification has no id and gets no body back (HTTP 202).
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := response{JSONRPC: "2.0", ID: req.ID}
	result, rerr := s.dispatch(r, req)
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	writeRPC(w, resp)
}

var null = json.RawMessage("null")

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return null
	}
	return id
}

func writeRPC(w http.ResponseWriter, resp response) {
	data, err := json.Marshal(resp)
	if err != nil {
		data, _ = json.Marshal(response{JSONRPC: "2.0", ID: idOrNull(resp.ID), Error: &rpcError{Code: codeInternal, Message: "could not encode the response"}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) dispatch(r *http.Request, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.listTools(), nil
	case "tools/call":
		return s.callTool(r, req.Params)
	}
	// server/discover (a Bifrost probe) and anything else we do not serve:
	// resources, prompts, logging, completion.
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
}

func (s *Server) initialize(params json.RawMessage) any {
	var in struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &in)
	version := s.negotiate(in.ProtocolVersion)
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo":   map[string]any{"name": s.opts.Name, "version": s.opts.Version},
		"instructions": "Tools that check whether an answer about a watched fact (a price, a policy, an opening time) is still current, explain how a request through the freshness companion was answered, list the watched sources, and report that a fact changed.",
	}
}

// negotiate echoes a requested version the server supports; otherwise it
// offers the newest one it does.
func (s *Server) negotiate(requested string) string {
	for _, v := range s.supported {
		if v == requested {
			return v
		}
	}
	return s.supported[len(s.supported)-1]
}

func (s *Server) listTools() any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.schema,
		})
	}
	return map[string]any{"tools": out}
}

// toolResult is the result of tools/call.
type toolResult struct {
	Content           []content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textResult(text string, structured any) toolResult {
	return toolResult{Content: []content{{Type: "text", Text: text}}, StructuredContent: structured}
}

// errorResult is a tool-level error: the call was understood but could not
// be carried out (an unknown fact, a missing argument). It is a result with
// isError, so a model-driven client can read it and try again.
func errorResult(text string) toolResult {
	return toolResult{Content: []content{{Type: "text", Text: text}}, IsError: true}
}

func (s *Server) callTool(r *http.Request, params json.RawMessage) (any, *rpcError) {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.Name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs params.name and params.arguments"}
	}
	for _, t := range s.tools {
		if t.name == in.Name {
			args := map[string]any{}
			if len(in.Arguments) > 0 && string(in.Arguments) != "null" {
				if err := json.Unmarshal(in.Arguments, &args); err != nil {
					return nil, &rpcError{Code: codeInvalidParams, Message: "arguments must be a JSON object"}
				}
			}
			return t.run(r, args), nil
		}
	}
	return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + in.Name}
}

// noticeAuthorized reports whether a report_change call carries the
// companion's change-notice token (when one is configured).
func (s *Server) noticeAuthorized(r *http.Request) bool {
	if s.deps.NoticeToken == "" {
		return true
	}
	want := []byte(s.deps.NoticeToken)
	if got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && subtle.ConstantTimeCompare([]byte(got), want) == 1 {
		return true
	}
	got := r.Header.Get(NoticeTokenHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), want) == 1
}

// argString reads an optional string argument; a non-string value is an error.
func argString(args map[string]any, name string) (string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", name)
	}
	return s, nil
}

// argStrings reads an optional array-of-strings argument.
func argStrings(args map[string]any, name string) ([]string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be an array of strings", name)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("argument %q must be an array of strings", name)
		}
		out = append(out, s)
	}
	return out, nil
}
