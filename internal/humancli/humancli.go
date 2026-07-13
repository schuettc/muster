// Package humancli implements muster's operator subcommands (agents, inbox,
// send, tasks) that read/drive the bus from a plain shell.
package humancli

import (
	"encoding/json"
	"fmt"
	"io"
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

// threadRow decodes daemon thread responses (get_thread, list_tasks). It is
// unused by Task 1's agents command; Milestone D's inbox/send/tasks commands
// (added in later tasks) decode into it.
//
//nolint:unused // reserved for humancli commands landing later in Milestone D
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
