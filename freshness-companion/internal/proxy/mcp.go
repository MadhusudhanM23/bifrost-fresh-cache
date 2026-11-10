package proxy

import (
	"context"
	"net/http"

	"freshness-companion/internal/changes"
	"freshness-companion/internal/mcpserver"
)

// MCPOptions configures the MCP server mounted on POST /mcp.
type MCPOptions struct {
	// ServerName and ServerVersion are reported to MCP clients.
	ServerName    string
	ServerVersion string
	// MaxProtocolVersion caps the MCP protocol version (default 2025-06-18).
	MaxProtocolVersion string
}

// EnableMCP mounts the companion's MCP server (package mcpserver) on
// POST /mcp. Until it is called the route answers 404 and says MCP mode is
// off. The tools reuse the proxy's own pieces: the policy, the evidence
// cache, the explain ring, the change-notice store and the same function
// that serves POST /freshness/changes (so report_change needs the same
// bearer token, when one is configured). It returns the tool names.
func (s *Server) EnableMCP(opts MCPOptions) ([]string, error) {
	deps := mcpserver.Deps{
		Policy:      s.cfg.Policy,
		Evidence:    s.cfg.Engine.Evidence,
		ExplainKept: s.explains.cap,
		Changes:     s.changes,
		NoticeToken: s.cfg.ChangeToken,
		Report: func(ctx context.Context, req mcpserver.ChangeRequest) (changes.Notice, error) {
			return s.applyChangeNotice(ctx, changeRequest{
				Fact: req.Fact, Category: req.Category, SourceURL: req.SourceURL,
				Note: req.Note, ClearCacheKeys: req.ClearCacheKeys,
			})
		},
	}
	if s.explains.cap > 0 {
		deps.Explain = func(id string) (string, any, bool) {
			e, ok := s.explains.get(id)
			if !ok {
				return "", nil, false
			}
			return e.Plain, e, true
		}
	}
	if dd, ok := s.cfg.Engine.Discover.(mcpserver.Discoverer); ok && dd != nil {
		deps.Discover = dd
	}
	srv, err := mcpserver.New(deps, mcpserver.Options{
		Name: opts.ServerName, Version: opts.ServerVersion,
		MaxProtocolVersion: opts.MaxProtocolVersion, Now: s.cfg.Now,
	})
	if err != nil {
		return nil, err
	}
	var h http.Handler = srv
	s.mcp.Store(&h)
	return srv.ToolNames(), nil
}

// handleMCP serves POST /mcp: the MCP server when MCP mode is on, otherwise
// a 404 that says how to turn it on.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	h := s.mcp.Load()
	if h == nil {
		writeError(w, http.StatusNotFound, "MCP mode is off on this companion (start it with -mcp).", "not_found")
		return
	}
	(*h).ServeHTTP(w, r)
}
