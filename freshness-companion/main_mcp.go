package main

import (
	"log/slog"
	"strings"

	"freshness-companion/internal/proxy"
)

// enableMCP mounts the MCP server on POST /mcp when -mcp is set. The tools
// reuse the proxy's pieces, so there is nothing else to configure: the
// change-notice token (FRESHNESS_NOTICE_TOKEN) also protects report_change.
func enableMCP(srv *proxy.Server, on bool, logger *slog.Logger) error {
	if !on {
		return nil
	}
	tools, err := srv.EnableMCP(proxy.MCPOptions{ServerName: "freshness-companion", ServerVersion: "0.1"})
	if err != nil {
		return err
	}
	logger.Info("MCP server mounted on POST /mcp", "tools", strings.Join(tools, ","))
	return nil
}
