package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/schuettc/muster/internal/channel"
	"github.com/schuettc/muster/internal/channelmcp"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/tmuxenv"
	"github.com/schuettc/muster/internal/version"
)

// channelInstructions is the durable core, taught once at handshake: what
// the channel is and how a push is shaped. Everything specific to a kind of
// event travels WITH that event, after the separator, so the rule sits next
// to the action it governs instead of thousands of tokens back.
const channelInstructions = `This channel is muster, the local coordination bus. A <channel source="muster-channel"> message means mail arrived on the bus for THIS session.
- Each push is an envelope line (intent, sender, thread id, subject — never the body), then a line containing only "---", then guidance for that push. Follow the guidance; it names the tool to call.
- The body lives in the bus: get_thread <id> reads one thread, get_inbox lists everything unread, reply answers.
- Act autonomously. Do not ask the user whether to check mail; reading it is the point.
- muster_channel_status reports which aliases this channel pushes for and why it might be idle.`

// ChannelMaxListedEnv caps how many threads a batch push still lists on its
// envelope line; above it the push collapses to a count and the strictest
// intent. Non-numeric or non-positive values fall back to the default.
const ChannelMaxListedEnv = "MUSTER_CHANNEL_MAX_LISTED"

// channelMaxListed reads ChannelMaxListedEnv, defaulting to channel.MaxListed.
func channelMaxListed() int {
	raw := os.Getenv(ChannelMaxListedEnv)
	if raw == "" {
		return channel.MaxListed
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "muster: ignoring %s=%q (want a positive integer), using %d\n", ChannelMaxListedEnv, raw, channel.MaxListed)
		return channel.MaxListed
	}
	return n
}

// runChannel serves a claude/channel MCP server on stdio for the session that
// launched it and runs the carrier beside it. stdout is the protocol.
//
// Shutdown has two triggers and both must end the process: the client
// closing stdin (srv.Run returns), or SIGINT/SIGTERM. signal.NotifyContext
// suppresses the signal's default action, so if the server loop were the
// only thing we waited on, a harness that sends SIGTERM while still holding
// our pipes would leave a process that never exits — measured by the
// pi-channels client, which had to SIGKILL after a grace period.
func runChannel() {
	channel.MaxListed = channelMaxListed()
	capture := tmuxenv.CaptureEnv()
	carrier := &channel.Carrier{
		Call:     channel.DaemonClient(paths.SocketPath()),
		Ident:    channel.Identity{SocketPath: capture.SocketPath, SessionID: capture.SessionID, PaneID: capture.PaneID, SessionCreated: capture.SessionCreated},
		Interval: channelInterval(),
	}
	srv := channelmcp.New(channelmcp.Handler{
		Name:         "muster-channel",
		Version:      version.Version(),
		Instructions: channelInstructions,
		Tools: []channelmcp.Tool{{
			Name:        "muster_channel_status",
			Description: "Is the muster channel attached? Reports the tmux pane, the aliases it pushes for, the journal cursor, the last push, and why it is idle if it is.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		}},
		Call: func(name string, _ json.RawMessage) (string, error) {
			if name != "muster_channel_status" {
				return "", fmt.Errorf("unknown tool %q", name)
			}
			return carrier.Status(), nil
		},
	})
	carrier.Notify = srv.Notify

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	go carrier.Run(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Run(os.Stdin, os.Stdout) }()
	select {
	case <-ctx.Done():
		// Signalled while the client still holds our pipes: exit now.
		cancel()
	case err := <-done:
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: channel:", err)
			os.Exit(1)
		}
	}
}
