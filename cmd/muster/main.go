// Command muster is the entrypoint for the muster coordination bus.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/device"
	"github.com/schuettc/muster/internal/humancli"
	"github.com/schuettc/muster/internal/mcpserver"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/remote"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/wake"
)

func main() {
	if len(os.Args) < 2 {
		// The provided.al2023 runtime execs the zip's `bootstrap` with NO
		// arguments, so the Lambda never gets to say `muster lambda`. Without
		// this the function would print usage to a log nobody reads and exit
		// 2 on every cold start. AWS_LAMBDA_FUNCTION_NAME is set by the
		// runtime itself and by nothing else, so it cannot fire on a device.
		if os.Getenv(LambdaRuntimeEnv) != "" {
			os.Exit(runLambda())
		}
		humancli.Usage(os.Stdout)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if wantsHelp(os.Args[2:]) {
			_ = humancli.HelpFor("serve", os.Stdout)
			return
		}
		os.Exit(runServe())
	case "debug":
		if wantsHelp(os.Args[2:]) {
			_ = humancli.HelpFor("debug", os.Stdout)
			return
		}
		runDebug(os.Args[2:])
	case "mcp":
		if wantsHelp(os.Args[2:]) {
			_ = humancli.HelpFor("mcp", os.Stdout)
			return
		}
		runMCP()
	case "lambda":
		if wantsHelp(os.Args[2:]) {
			_ = humancli.HelpFor("lambda", os.Stdout)
			return
		}
		os.Exit(runLambda())
	default:
		// humancli.Dispatch owns the CLI subcommand list (including
		// help/version) and errors on an unknown one — routing everything
		// here keeps that list canonical (a second list in this switch once
		// shipped a release whose usage advertised a subcommand main()
		// refused to route).
		if err := humancli.Dispatch(os.Args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "muster:", err)
			code := 1
			var usageErr *humancli.UsageError
			if errors.As(err, &usageErr) {
				code = 2
			}
			os.Exit(code)
		}
	}
}

// wantsHelp reports whether the first token after a subcommand name is a
// help flag. serve/mcp/debug/lambda are owned by main() (not humancli.Dispatch),
// so their -h/--help handling lives here rather than behind flag.ErrHelp
// interception the way the humancli-dispatched commands do it.
func wantsHelp(args []string) bool {
	return len(args) > 0 && humancli.IsHelpArg(args[0])
}

// LambdaRuntimeEnv is set by the AWS Lambda runtime in every execution
// environment. main() reads it only to decide what an ARGUMENT-LESS
// invocation means: on a device that is a usage error, inside Lambda it is
// the runtime starting `bootstrap`. An explicit `muster lambda` needs none of
// this — the switch above routes it regardless of environment.
const LambdaRuntimeEnv = "AWS_LAMBDA_FUNCTION_NAME"

// BackendEnv selects which backend `muster serve` fronts: "local" (the
// default) or "remote". An unrecognised value is an error rather than a
// fallback to local — a typo'd backend must not silently strand a device on a
// bus nobody else is on.
const BackendEnv = "MUSTER_BACKEND"

// RemoteURLEnv is the hosted bus endpoint, required when BackendEnv is
// "remote". The token is deliberately NOT an environment variable: it lives in
// a 0600 file (see remote.ReadToken).
const RemoteURLEnv = "MUSTER_REMOTE_URL"

// PollIntervalEnv tunes how often a remote-mode daemon asks the bus whether
// another device has sent mail to an agent on this one (a Go duration, e.g.
// "3s"). It bounds only CROSS-device wake latency — a write from this device
// reconciles inline — and the poller widens the interval by itself while the
// bus is quiet, so this is the floor rather than the fixed cadence.
// Unparseable or non-positive values fall back to the default with a warning,
// since a mistyped knob must not leave a device silently unwoken.
const PollIntervalEnv = "MUSTER_POLL_INTERVAL"

// pollInterval reads PollIntervalEnv, defaulting to daemon.DefaultPollInterval.
func pollInterval() time.Duration {
	raw := os.Getenv(PollIntervalEnv)
	if raw == "" {
		return daemon.DefaultPollInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "muster: ignoring %s=%q (want a positive Go duration like 10s), using %s\n",
			PollIntervalEnv, raw, daemon.DefaultPollInterval)
		return daemon.DefaultPollInterval
	}
	return d
}

// runServe runs the daemon until it receives SIGINT/SIGTERM, returning the
// process exit code (0 on a clean shutdown, non-zero on setup failure).
//
// Both backends bind the same unix socket and use the same tmux notifier —
// every client above the daemon is identical either way. Local mode touches no
// remote code at all, and neither mode links the AWS SDK.
func runServe() int {
	if err := os.MkdirAll(paths.Home(), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "muster: mkdir:", err)
		return 1
	}
	notifier := wake.NewTmuxNotifier("@muster_inbox", 500*time.Millisecond)

	var d *daemon.Daemon
	switch backend := os.Getenv(BackendEnv); backend {
	case "", "local":
		s, err := store.Open(paths.DBPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: open store:", err)
			return 1
		}
		defer func() { _ = s.Close() }()
		d, err = daemon.Serve(paths.SocketPath(), s, notifier, device.Name())
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: serve:", err)
			return 1
		}
	case "remote":
		var err error
		if d, err = serveRemote(notifier); err != nil {
			fmt.Fprintln(os.Stderr, "muster:", err)
			return 1
		}
		// The poller is remote mode's ONLY wake path for traffic that
		// originated on another device: a local write reconciles inline, but
		// nothing on this machine hears about a peer's message otherwise.
		d.StartPoller(pollInterval())
	default:
		fmt.Fprintf(os.Stderr, "muster: unknown %s %q (want local or remote)\n", BackendEnv, backend)
		return 1
	}
	defer func() { _ = d.Close() }()
	fmt.Fprintln(os.Stderr, "muster daemon listening at", paths.SocketPath())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	return 0
}

// serveRemote builds the remote-mode daemon: it keeps the unix socket and the
// tmux notifier on this device and forwards every request to the hosted bus.
//
// Every input is resolved BEFORE the socket is bound, so a device missing its
// URL, token or identity fails at startup with the reason rather than on
// whichever op a client happens to send first.
func serveRemote(n wake.Notifier) (*daemon.Daemon, error) {
	url := os.Getenv(RemoteURLEnv)
	if url == "" {
		return nil, fmt.Errorf("%s=remote requires %s", BackendEnv, RemoteURLEnv)
	}
	token, err := remote.ReadToken()
	if err != nil {
		return nil, err
	}
	up, err := remote.New(url, token)
	if err != nil {
		return nil, err
	}
	id, err := device.ID()
	if err != nil {
		return nil, fmt.Errorf("device id: %w", err)
	}
	return daemon.ServeRemote(paths.SocketPath(), up, n, id, device.Name())
}

// runDebug sends a raw op with key=value string args. Example:
//
//	muster debug register_agent alias=backend role=producer
//	muster debug list_agents
func runDebug(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "muster: usage: muster debug <op> [key=value ...]")
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
		fmt.Fprintln(os.Stderr, "muster: call:", err)
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
		fmt.Fprintln(os.Stderr, "muster: mcp:", err)
		os.Exit(1)
	}
}
