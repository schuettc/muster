package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is reported to MCP clients in the server implementation info.
const version = "0.1.0"

// Run builds the muster MCP server, registers all tools, and serves over stdio.
// It blocks until the client disconnects or ctx is cancelled.
func Run(ctx context.Context) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: "muster", Version: version}, nil)
	registerAll(srv)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// registerAll wires every tool onto the server. Each tools_*.go file adds its
// own registration here via this central function.
func registerAll(srv *mcp.Server) {
	registerRegistryTools(srv)
	registerMessageTools(srv) // Task 3
	// registerTaskTools(srv)      // Task 4
	// registerKVTools(srv)        // Task 5
}
