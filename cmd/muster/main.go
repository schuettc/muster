// Command muster is the entrypoint for the muster coordination bus.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/humancli"
	"github.com/schuettc/muster/internal/mcpserver"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/wake"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: muster <serve|debug|mcp|agents|inbox|send|tasks|events|nudge|register|deregister|gc|hook|label> [args]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(runServe())
	case "debug":
		runDebug(os.Args[2:])
	case "mcp":
		runMCP()
	default:
		// humancli.Dispatch owns the CLI subcommand list and errors on an
		// unknown one — routing everything here keeps that list canonical
		// (a second list in this switch once shipped a release whose usage
		// advertised a subcommand main() refused to route).
		if err := humancli.Dispatch(os.Args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "muster:", err)
			os.Exit(1)
		}
	}
}

// runServe runs the daemon until it receives SIGINT/SIGTERM, returning the
// process exit code (0 on a clean shutdown, non-zero on setup failure).
func runServe() int {
	if err := os.MkdirAll(paths.Home(), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		return 1
	}
	s, err := store.Open(paths.DBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		return 1
	}
	defer func() { _ = s.Close() }()
	d, err := daemon.Serve(paths.SocketPath(), s, wake.NewTmuxNotifier("@muster_inbox", 500*time.Millisecond))
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	}
	defer func() { _ = d.Close() }()
	fmt.Fprintln(os.Stderr, "muster daemon listening at", paths.SocketPath())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	return 0
}

// runDebug sends a raw op with key=value string args. Example:
//
//	muster debug register_agent alias=backend role=producer
//	muster debug list_agents
func runDebug(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: muster debug <op> [key=value ...]")
		os.Exit(2)
	}
	req := proto.Request{Op: args[0], Args: map[string]any{}}
	for _, kv := range args[1:] {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				req.Args[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	resp, err := client.Call(paths.SocketPath(), req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "call:", err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Println(string(out))
	if !resp.OK {
		os.Exit(1)
	}
}

// runMCP serves the MCP stdio server. IMPORTANT: stdout is the MCP channel;
// all diagnostics go to stderr.
func runMCP() {
	if err := mcpserver.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		os.Exit(1)
	}
}
