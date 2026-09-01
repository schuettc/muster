// Package mcpserver exposes muster's daemon operations as MCP tools over stdio.
package mcpserver

import (
	"encoding/json"
	"fmt"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
)

// rosterRow is the full-fidelity decode of a list_agents row — the fields
// AgentView (the tool-facing shape) deliberately omits but identity guards
// need. Tags match the daemon's snake_case store JSON.
type rosterRow struct {
	Alias      string `json:"alias"`
	ModelType  string `json:"model_type"`
	SocketPath string `json:"socket_path"`
	PaneID     string `json:"pane_id"`
	SessionID  string `json:"session_id"`
	// SessionCreated is the incarnation half of tmux identity (#{session_created},
	// unix seconds — see tmuxenv.Capture.SessionCreated). tmux recycles session
	// IDs from $0 across server restarts, so a (socket_path, session_id, pane_id)
	// tuple match alone cannot tell a live registration from a stale un-reaped
	// row left behind by a dead server incarnation that happened to reuse the
	// same IDs. 0 = unknown (a pre-upgrade row, or one captured outside tmux).
	SessionCreated int64  `json:"session_created"`
	Label          string `json:"label"`
	Departed       bool   `json:"departed"`
	// SupersededBy mirrors store.Agent.SupersededBy — non-empty on a row
	// retired via `become`, naming the alias its identity moved onto.
	// paneRegistration follows this chain by alias when a tuple match lands
	// on a departed row.
	SupersededBy string `json:"superseded_by"`
}

// maxBecomeChainHops bounds paneRegistration's superseded_by walk — a
// pathological or corrupted chain must not loop the daemon call forever.
const maxBecomeChainHops = 8

// paneRegistration returns the calling pane's own live registration: the
// roster row matching this exact (socket_path, session_id, pane_id) tuple
// AND, when both sides know it, the same session incarnation
// (session_created — see rosterRow.SessionCreated). A row whose recorded
// creation time differs from the caller's live one is a ghost left by a
// recycled session ID, not a match.
//
// A tuple match that is DEPARTED with SupersededBy set is not "not
// registered" — `become` retires the old alias in place (its stored tuple is
// never cleared) while cloning the identity onto the successor, and that
// successor may since have re-registered under an entirely different tuple
// (a new pane, a later become of its own), so a second tuple scan over the
// roster would never find it. followBecomeChain walks the alias link
// directly (bounded, cycle-safe) to the first non-departed row in the chain
// and returns THAT — the caller's actual current identity, wherever it now
// lives. A departed tuple match with no successor (an ordinary tombstone) is
// not a match; the outer scan continues in case another row also matches the
// tuple. ok=false outside tmux, on any daemon/decode failure (guards degrade
// open — today's behavior), when no row matches the tuple (chain-followed or
// not), or when the only tuple-matching row is a ghost.
func paneRegistration(socketPath, sessionID, paneID string, sessionCreated int64) (rosterRow, bool) {
	if socketPath == "" || sessionID == "" || paneID == "" {
		return rosterRow{}, false
	}
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		return rosterRow{}, false
	}
	var rows []rosterRow
	if json.Unmarshal(raw, &rows) != nil {
		return rosterRow{}, false
	}
	byAlias := make(map[string]rosterRow, len(rows))
	for _, r := range rows {
		byAlias[r.Alias] = r
	}
	for _, r := range rows {
		if r.SocketPath != socketPath || r.SessionID != sessionID || r.PaneID != paneID {
			continue
		}
		if !r.Departed {
			if r.SessionCreated != 0 && sessionCreated != 0 && r.SessionCreated != sessionCreated {
				continue // ghost: same tuple, different session incarnation
			}
			return r, true
		}
		if r.SupersededBy == "" {
			continue // ordinary tombstone: no successor to follow
		}
		if live, ok := followBecomeChain(byAlias, r.SupersededBy); ok {
			return live, true
		}
	}
	return rosterRow{}, false
}

// followBecomeChain walks a `become` superseded_by link, alias to alias, to
// the first non-departed row — the caller's actual current identity. Bounded
// to maxBecomeChainHops and cycle-safe via visited: a corrupted or
// pathological chain must fail closed (ok=false), never loop.
func followBecomeChain(byAlias map[string]rosterRow, alias string) (rosterRow, bool) {
	visited := make(map[string]bool, maxBecomeChainHops)
	for hops := 0; hops < maxBecomeChainHops; hops++ {
		if visited[alias] {
			return rosterRow{}, false
		}
		visited[alias] = true
		r, found := byAlias[alias]
		if !found {
			return rosterRow{}, false
		}
		if !r.Departed {
			return r, true
		}
		if r.SupersededBy == "" {
			return rosterRow{}, false
		}
		alias = r.SupersededBy
	}
	return rosterRow{}, false
}

// callDaemon sends one op to the daemon (lazily starting it) and returns the
// response Data as JSON, or an error if the transport failed or the daemon
// reported !OK. It is a package-level var so tests can stub it.
var callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
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

// AgentView is the tool-facing shape of a registered agent. Field tags match
// the daemon's JSON case-insensitively.
//
// Project/Label/LabelManual/Departed exist because an alias is not the only
// address on this bus: internal/resolve matches an exact alias FIRST, then a
// qualified "project:label", then a bare label scoped to the sender's own
// project. A roster that showed aliases alone therefore described a smaller
// bus than the daemon actually routes — an agent reading it concluded a live
// label address did not exist and proposed retiring a durable alias to
// "fix" mail that was already being delivered. These four fields are exactly
// what the resolver decides on, so a caller can build any address the daemon
// will accept, and none it won't.
type AgentView struct {
	Alias        string `json:"alias" jsonschema:"the agent's addressable alias"`
	Role         string `json:"role" jsonschema:"the agent's role (producer, consumer, reviewer, ...)"`
	ModelType    string `json:"model_type" jsonschema:"the agent's model (claude, codex, or cursor)"`
	SessionName  string `json:"session_name" jsonschema:"the tmux session the agent runs in"`
	DeviceName   string `json:"device_name" jsonschema:"the machine this agent runs on, named by its operator (e.g. 'work-laptop'). Match this when the human names a machine — 'the ci-cd session on my work laptop' means the agent whose device_name is work-laptop. Empty on a single-machine bus, where it carries no information. It is NOT part of the address: having found the right row, send to its alias."`
	Project      string `json:"project" jsonschema:"the project the agent is registered under; the qualifier in a 'project:label' address"`
	Label        string `json:"label" jsonschema:"what the agent is working on right now; addressable as 'project:label' (or bare within your own project) only when label_manual is true"`
	LabelManual  bool   `json:"label_manual" jsonschema:"true when a human pinned this label, which is what makes it addressable; an auto-generated label is display-only and will not resolve"`
	Departed     bool   `json:"departed" jsonschema:"true for a deregistered agent: its alias still accepts mail (it may return) but its label no longer resolves"`
	RegisteredAt int64  `json:"registered_at" jsonschema:"when the agent first registered (unix ms)"`
	LastSeen     int64  `json:"last_seen" jsonschema:"when the agent was last active (unix ms)"`
}

// ThreadView is the tool-facing shape of a message/task thread. LastFrom,
// LastAt, EntryCount, and Unread are query-time annotations the daemon's
// get_inbox (and get_thread, for its own thread) populate from store.Thread —
// zero-valued when a surface doesn't compute them, so they're additive
// fields, not a breaking change to existing callers.
type ThreadView struct {
	ID         int64  `json:"id" jsonschema:"the thread id"`
	Kind       string `json:"kind" jsonschema:"message or task"`
	FromAgent  string `json:"from_agent" jsonschema:"who created the thread"`
	ToKind     string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget   string `json:"to_target" jsonschema:"the addressed alias or role"`
	Subject    string `json:"subject" jsonschema:"the thread subject"`
	Ref        string `json:"ref" jsonschema:"a pointer to the work (repo/branch/endpoint/file)"`
	Status     string `json:"status" jsonschema:"task status, empty for messages"`
	Standing   bool   `json:"standing,omitempty" jsonschema:"true if this is a standing broadcast (also delivered to sessions that start later, until read)"`
	CreatedAt  int64  `json:"created_at" jsonschema:"creation time (unix ms)"`
	UpdatedAt  int64  `json:"updated_at" jsonschema:"last-update time (unix ms)"`
	LastFrom   string `json:"last_from" jsonschema:"who wrote the thread's most recent entry"`
	LastAt     int64  `json:"last_at" jsonschema:"when the most recent entry was written (unix ms)"`
	EntryCount int    `json:"entry_count" jsonschema:"total entries in the thread"`
	Unread     int    `json:"unread" jsonschema:"entries after your last read that you didn't write yourself; 0 means you've seen everything"`
}

// EntryView is one append-only entry within a thread.
type EntryView struct {
	ID           int64  `json:"id" jsonschema:"the entry id"`
	ThreadID     int64  `json:"thread_id" jsonschema:"the parent thread id"`
	FromAgent    string `json:"from_agent" jsonschema:"who wrote this entry"`
	Body         string `json:"body" jsonschema:"the entry text"`
	StatusChange string `json:"status_change" jsonschema:"the status this entry set, if any"`
	CreatedAt    int64  `json:"created_at" jsonschema:"when the entry was written (unix ms)"`
}
