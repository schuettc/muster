// Package humancli implements muster's operator subcommands (agents, inbox,
// send, tasks) that read/drive the bus from a plain shell.
package humancli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
)

type agentRow struct {
	Alias       string `json:"alias"`
	Role        string `json:"role"`
	ModelType   string `json:"model_type"`
	SessionName string `json:"session_name"`
	LastSeen    int64  `json:"last_seen"`
}

// threadRow decodes daemon thread responses (get_inbox, get_thread, list_tasks).
type threadRow struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	FromAgent string `json:"from_agent"`
	ToKind    string `json:"to_kind"`
	ToTarget  string `json:"to_target"`
	Subject   string `json:"subject"`
	Status    string `json:"status"`
}

// callData sends one op to the daemon and returns its Data as JSON, or an error
// if the transport failed or the daemon reported !OK.
func callData(op string, args map[string]any) (json.RawMessage, error) {
	resp, err := client.Call(paths.SocketPath(), proto.Request{Op: op, Args: args})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", op, resp.Error)
	}
	b, err := json.Marshal(resp.Data)
	if err != nil {
		return nil, fmt.Errorf("marshal %s result: %w", op, err)
	}
	return b, nil
}

// Dispatch routes an operator subcommand. args[0] is the subcommand name.
func Dispatch(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: muster <agents|inbox|send|tasks> [args]")
	}
	switch args[0] {
	case "agents":
		return cmdAgents(out)
	case "send":
		return cmdSend(args[1:], out)
	case "inbox":
		return cmdInbox(args[1:], out)
	case "tasks":
		return cmdTasks(args[1:], out)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdAgents(out io.Writer) error {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return err
	}
	var agents []agentRow
	if err := json.Unmarshal(raw, &agents); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ALIAS\tROLE\tMODEL\tSESSION"); err != nil {
		return err
	}
	for _, a := range agents {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Alias, a.Role, a.ModelType, a.SessionName); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// cmdSend sends a message to an agent, role, or broadcast target and prints
// the resulting thread ID.
func cmdSend(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "human", "sending agent alias")
	subject := fs.String("subject", "", "message subject")
	ref := fs.String("ref", "", "pointer to the work")
	role := fs.Bool("role", false, "treat target as a role")
	broadcast := fs.Bool("broadcast", false, "send to everyone")
	flagArgs, rest := splitFlagsAndPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	toKind, toTarget := "agent", ""
	switch {
	case *broadcast:
		toKind = "broadcast"
	case *role:
		toKind = "role"
	}
	var body string
	if *broadcast {
		if len(rest) < 1 {
			return fmt.Errorf("usage: muster send --broadcast <body>")
		}
		body = strings.Join(rest, " ")
	} else {
		if len(rest) < 2 {
			return fmt.Errorf("usage: muster send <target> <body> [--from X --subject S --ref R --role --broadcast]")
		}
		toTarget = rest[0]
		body = strings.Join(rest[1:], " ")
	}
	raw, err := callData("send_message", map[string]any{
		"from": *from, "to_kind": toKind, "to_target": toTarget,
		"subject": *subject, "ref": *ref, "body": body,
	})
	if err != nil {
		return err
	}
	var res struct {
		ThreadID int64 `json:"thread_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "sent (thread %d)\n", res.ThreadID)
	return err
}

// sendBoolFlags are cmdSend's flags that take no value, needed so
// splitFlagsAndPositional knows not to consume the following token as a value.
var sendBoolFlags = map[string]bool{"role": true, "broadcast": true}

// splitFlagsAndPositional separates args into flag.FlagSet-parseable tokens
// and positional arguments, regardless of whether flags appear before or
// after the positionals — Go's flag.Parse otherwise stops at the first
// non-flag token, which breaks `send <target> <body> [--from X ...]`.
func splitFlagsAndPositional(args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		name := strings.TrimLeft(a, "-")
		idx := strings.Index(name, "=")
		hasValue := idx >= 0
		if hasValue {
			name = name[:idx]
		}
		if !hasValue && !sendBoolFlags[name] && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positional
}

// cmdInbox prints the given alias's threads.
func cmdInbox(args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: muster inbox <alias>")
	}
	return printThreads(out, args[0], false)
}

// cmdTasks prints the given alias's inbox filtered to kind=task threads.
func cmdTasks(args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: muster tasks <alias>")
	}
	return printThreads(out, args[0], true)
}

// printThreads fetches an alias's inbox and prints it; if tasksOnly, only
// kind=task threads are shown.
func printThreads(out io.Writer, alias string, tasksOnly bool) error {
	raw, err := callData("get_inbox", map[string]any{"alias": alias})
	if err != nil {
		return err
	}
	var threads []threadRow
	if err := json.Unmarshal(raw, &threads); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tKIND\tFROM\tTO\tSTATUS\tSUBJECT"); err != nil {
		return err
	}
	for _, th := range threads {
		if tasksOnly && th.Kind != "task" {
			continue
		}
		to := th.ToKind
		if th.ToTarget != "" {
			to = th.ToKind + ":" + th.ToTarget
		}
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", th.ID, th.Kind, th.FromAgent, to, th.Status, th.Subject); err != nil {
			return err
		}
	}
	return tw.Flush()
}
