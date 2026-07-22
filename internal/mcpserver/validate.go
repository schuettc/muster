package mcpserver

import (
	"encoding/json"
	"fmt"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// requireRegisteredFrom gates the MCP thread-minting ops (send_message,
// reply, task_create): from must be an EXACT roster alias. Departed rows
// count — draining a tombstoned alias's leftover mail must still be able to
// reply as it. This closes the address-minting hole where a model invents a
// from alias (e.g. a persona like "timewalk-1998") and the daemon accepts it
// verbatim, materializing an identity nobody registered. The human CLI is
// deliberately NOT behind this gate — `muster send --from operator` is the
// operator's escape hatch and doesn't route through these handlers.
//
// On rejection the error carries the caller's REAL identity when the pane's
// registration resolves, so a confused model self-corrects in one step
// instead of retrying blind. A roster fetch/decode failure degrades open
// (returns nil): a dead daemon already fails the op itself with a clearer
// transport error.
func requireRegisteredFrom(from string) error {
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		return nil // degrade open; the real op will surface the transport error
	}
	var rows []rosterRow
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	for _, r := range rows {
		if r.Alias == from {
			return nil
		}
	}
	c := tmuxenv.CaptureEnv()
	if row, ok := paneRegistration(c.SocketPath, c.SessionID, c.PaneID, c.SessionCreated); ok {
		identity := fmt.Sprintf("'%s'", row.Alias)
		if row.Label != "" {
			identity = fmt.Sprintf("'%s' (label '%s')", row.Alias, row.Label)
		}
		return fmt.Errorf("from alias %q is not registered; this session is registered as %s — send as that alias", from, identity)
	}
	return fmt.Errorf("from alias %q is not registered; call list_agents to find your alias (sessions auto-register as their tmux session name)", from)
}
