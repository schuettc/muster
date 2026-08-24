package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/schuettc/muster/internal/channel"
	"github.com/schuettc/muster/internal/channelmcp"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/tmuxenv"
	"github.com/schuettc/muster/internal/version"
)

// channelInstructions is the handshake text: the whole protocol, taught once.
const channelInstructions = `This channel is muster, the local coordination bus. A <channel source="muster-channel"> message means mail arrived on the bus for THIS session.
- It names the intent, sender, thread id and subject. The body is not included.
- action-requested or reply-requested: call get_thread with the thread id, do what it asks, then answer with the muster reply tool.
- fyi: read it with get_thread; no reply is needed.
- A push naming several items, or a "N unread" summary: call get_inbox and work through each thread.
- Act autonomously. Do not ask the user whether to check mail; reading it is the point.
- muster_channel_status reports which aliases this channel pushes for and why it might be idle.`

// runChannel serves a claude/channel MCP server on stdio for the session that
// launched it and runs the carrier beside it. stdout is the protocol.
func runChannel() {
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
	err := srv.Run(os.Stdin, os.Stdout)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "muster: channel:", err)
		os.Exit(1)
	}
}
