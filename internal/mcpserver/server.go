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
//
// No tools are registered yet (Task 1 is the skeleton only), so the server
// parameter is unused until Task 2 adds the first registerXTools call.
func registerAll(_ *mcp.Server) {
	// Tools are added in later tasks:
	//   registerRegistryTools(srv)  // Task 2
	//   registerMessageTools(srv)   // Task 3
	//   registerTaskTools(srv)      // Task 4
	//   registerKVTools(srv)        // Task 5
}
