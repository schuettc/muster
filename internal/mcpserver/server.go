package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is reported to MCP clients in the server implementation info.
const version = "0.1.0"

const serverInstructions = "Call current_agent before choosing an identity. If registered is false, call register_agent. Use the returned agent.alias as your exact sender identity. Call deregister_agent only when this session should leave the bus; it tombstones this session's live aliases without deleting history."

// Run builds the muster MCP server, registers all tools, and serves over stdio.
// It blocks until the client disconnects or ctx is cancelled.
func Run(ctx context.Context) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: "muster", Version: version}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})
	registerAll(srv)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// registerAll wires every tool onto the server. Each tools_*.go file adds its
// own registration here via this central function.
func registerAll(srv *mcp.Server) {
	registerRegistryTools(srv)
	registerStatusTools(srv)
	registerMessageTools(srv)
	registerStandingTools(srv)
	registerTaskTools(srv)
	registerKVTools(srv)
}
