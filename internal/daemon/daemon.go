// Package daemon serves the muster store over a unix socket.
package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"

	"github.com/schuettc/muster/internal/display"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/wake"
)

// replyPreviewWidth caps how much of a reply's body is journaled into the
// event row's Detail (spec §4) — a display-length preview, not the reply
// itself (the full body lives on the thread's entry).
const replyPreviewWidth = 80

// storeAPI is the slice of *store.Store the daemon depends on. It exists so
// tests can substitute an error-injecting wrapper around a real *store.Store
// (see wake_wiring_test.go) without the store package itself growing a fake —
// *store.Store satisfies this interface as-is.
type storeAPI interface {
	RegisterAgent(store.Agent) error
	ListAgents() ([]store.Agent, error)
	GetAgent(alias string) (store.Agent, bool, error)
	DeleteAgent(alias string) error
	CreateThread(t store.Thread, firstBody string) (int64, error)
	AppendEntry(threadID int64, fromAgent, body, statusChange string) (int64, error)
	ClaimTask(threadID int64, byAgent string) error
	TransitionTask(threadID int64, byAgent, newStatus, note string) error
	GetThread(id int64) (store.Thread, []store.Entry, error)
	Threads(limit int) ([]store.Thread, error)
	Inbox(alias string) ([]store.Thread, error)
	MarkRead(alias string) error
	SessionUnread(socketPath, sessionID string) (total, action int, err error)
	KVSet(key, value, updatedBy string) error
	KVGet(key string) (store.KVPair, bool, error)
	AppendEvent(e store.Event) error
	Events(q store.EventQuery) ([]store.Event, error)
	MaxEventID() (int64, error)
	PruneEvents(olderThanMillis int64) (int64, error)
}

// Daemon owns the listener and the store.
type Daemon struct {
	ln net.Listener
	s  storeAPI
	n  wake.Notifier

	// sessLocks serializes {SessionUnread recompute, tmux option write,
	// journal} per (socket_path, session_id) tuple (spec §3): a concurrent
	// notify and get_inbox drain on the same session must not race, or the
	// smaller post-drain count can be overwritten by a stale in-flight
	// larger one. Keyed by sessionKey; created lazily under sessMu.
	sessMu    sync.Mutex
	sessLocks map[string]*sync.Mutex
}

// Serve binds socketPath (replacing any stale socket) and serves in a
// goroutine. n may be nil, in which case no notifications are delivered.
func Serve(socketPath string, s storeAPI, n wake.Notifier) (*Daemon, error) {
	_ = os.Remove(socketPath) // clear a stale socket from a previous run
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	d := &Daemon{ln: ln, s: s, n: n}
	go d.acceptLoop()
	return d, nil
}

// Close stops accepting connections.
func (d *Daemon) Close() error { return d.ln.Close() }

func (d *Daemon) acceptLoop() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go d.handle(conn)
	}
}

func (d *Daemon) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		var req proto.Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			_ = enc.Encode(proto.Response{Error: "bad request: " + err.Error()})
			continue
		}
		_ = enc.Encode(d.dispatch(req))
	}
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func i64(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// boolArg reads a bool arg, accepting a JSON bool or the strings "true"/"1"
// (the debug CLI passes all args as strings).
func boolArg(a map[string]any, key string) bool {
	switch v := a[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	default:
		return false
	}
}

func ok(data any) proto.Response    { return proto.Response{OK: true, Data: data} }
func fail(err error) proto.Response { return proto.Response{Error: err.Error()} }

// sessionKey is the sessLocks map key for a (socket_path, session_id) tuple.
// Empty-either-field tuples are never looked up through this path (callers
// guard socket != "" && session != "" first), so the separator collision
// space doesn't matter in practice.
func sessionKey(socketPath, sessionID string) string { return socketPath + "\x00" + sessionID }

// sessionLock returns the mutex guarding {SessionUnread recompute, notifier
// write, journal} for one session tuple, creating it lazily.
func (d *Daemon) sessionLock(socketPath, sessionID string) *sync.Mutex {
	key := sessionKey(socketPath, sessionID)
	d.sessMu.Lock()
	defer d.sessMu.Unlock()
	if d.sessLocks == nil {
		d.sessLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := d.sessLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		d.sessLocks[key] = mu
	}
	return mu
}

// setSessionBadge is the ONE canonical {recompute, push} sequence for a
// session's tmux badge (spec §3): under the session's lock, recompute the
// total unread via store.SessionUnread (never sum per-alias UnreadCount —
// that double-counts threads shared by sibling aliases), then push it to the
// notifier — Notify(total) when total > 0, Clear otherwise. Both notify's
// fan-out and get_inbox's drain funnel through this so a concurrent pair
// always leaves the badge at whichever recompute ran last, never a stale
// interleaved value. Callers journal using the returned total/err; on err,
// callers must journal "error: …" and must NOT treat it as a cleared badge.
func (d *Daemon) setSessionBadge(socketPath, sessionID string) (total int, err error) {
	mu := d.sessionLock(socketPath, sessionID)
	mu.Lock()
	defer mu.Unlock()
	total, _, err = d.s.SessionUnread(socketPath, sessionID)
	if err != nil {
		return 0, err
	}
	if total > 0 {
		err = d.n.Notify(socketPath, sessionID, total)
	} else {
		err = d.n.Clear(socketPath, sessionID)
	}
	return total, err
}

// reconcileBadge is setSessionBadge for identity-change call sites
// (register/deregister) that don't have a thread/journal-row shape to
// produce: best-effort, silent on an empty tuple or a nil notifier (there is
// no tmux badge to reconcile in either case).
func (d *Daemon) reconcileBadge(socketPath, sessionID string) {
	if d.n == nil || socketPath == "" || sessionID == "" {
		return
	}
	_, _ = d.setSessionBadge(socketPath, sessionID)
}

// notifyForThread flags every SESSION affected by activity on threadID — the
// thread's originator plus its recipients (agent/role/broadcast), minus the
// actor's entire session — coalescing sibling aliases of one (socket_path,
// session_id) tuple into a single recompute/notify/journal (spec §3: no
// duplicate lit rows for sibling aliases sharing a session). Agents with no
// tmux identity (empty socket or session) can never carry a tmux badge, so
// they are journaled "skipped" exactly as before, one row per alias. Best-
// effort; never types into a pane.
func (d *Daemon) notifyForThread(threadID int64, actor string) {
	if d.n == nil {
		return
	}
	th, _, err := d.s.GetThread(threadID)
	if err != nil {
		return
	}
	agents, err := d.s.ListAgents()
	if err != nil {
		return
	}
	byAlias := make(map[string]store.Agent, len(agents))
	for _, a := range agents {
		byAlias[a.Alias] = a
	}
	recipients := map[string]struct{}{th.FromAgent: {}}
	switch th.ToKind {
	case "agent":
		recipients[th.ToTarget] = struct{}{}
	case "role":
		for _, a := range agents {
			if a.Role == th.ToTarget && th.ToTarget != "" {
				recipients[a.Alias] = struct{}{}
			}
		}
	case "broadcast":
		for _, a := range agents {
			recipients[a.Alias] = struct{}{}
		}
	}
	// Drop the actor's entire session: the literal alias always goes (an
	// unregistered actor, e.g. "operator", only has this literal exclusion
	// to fall back on), plus any sibling alias sharing its exact tuple.
	delete(recipients, actor)
	if actorAgent, found := byAlias[actor]; found && actorAgent.SocketPath != "" && actorAgent.SessionID != "" {
		for alias := range recipients {
			if peer, ok := byAlias[alias]; ok && peer.SocketPath == actorAgent.SocketPath && peer.SessionID == actorAgent.SessionID {
				delete(recipients, alias)
			}
		}
	}

	// Group the remaining recipients by session tuple, in alias-sorted order
	// so "the alias that put the session in scope" is deterministic
	// (whichever alias of the tuple sorts first) rather than map-order luck.
	aliases := make([]string, 0, len(recipients))
	for alias := range recipients {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	type sessionGroup struct{ socketPath, sessionID, journalAlias string }
	seen := make(map[string]bool, len(aliases))
	var groups []sessionGroup
	for _, alias := range aliases {
		a, found := byAlias[alias]
		if !found || a.SocketPath == "" || a.SessionID == "" {
			d.logEvent(store.Event{Kind: "notify", Agent: alias, ThreadID: threadID, Detail: "skipped: no tmux identity"})
			continue
		}
		key := sessionKey(a.SocketPath, a.SessionID)
		if seen[key] {
			continue // sibling alias of an already-scheduled session
		}
		seen[key] = true
		groups = append(groups, sessionGroup{socketPath: a.SocketPath, sessionID: a.SessionID, journalAlias: alias})
	}

	for _, g := range groups {
		total, err := d.setSessionBadge(g.socketPath, g.sessionID)
		detail := "lit"
		switch {
		case err != nil:
			detail = "error: " + err.Error()
		case total <= 0:
			detail = "cleared"
		}
		d.logEvent(store.Event{Kind: "notify", Agent: g.journalAlias, ThreadID: threadID, Count: total, Detail: detail})
	}
}

// logEvent appends to the observability event log, best-effort: logging must
// never fail or slow the bus operation it describes.
func (d *Daemon) logEvent(e store.Event) { _ = d.s.AppendEvent(e) }

// targetOf renders a thread address as a journal target: 'broadcast' or
// '<to_kind>:<to_target>'.
func targetOf(a map[string]any) string {
	if str(a, "to_kind") == "broadcast" {
		return "broadcast"
	}
	return str(a, "to_kind") + ":" + str(a, "to_target")
}

func (d *Daemon) dispatch(req proto.Request) proto.Response {
	a := req.Args
	switch req.Op {
	case "register_agent":
		alias := str(a, "alias")
		old, hadOld, _ := d.s.GetAgent(alias) // best-effort: capture the pre-mutation tuple for reconciliation
		newAgent := store.Agent{
			Alias: alias, Role: str(a, "role"), ModelType: str(a, "model_type"),
			SocketPath: str(a, "socket_path"), PaneID: str(a, "pane_id"), SessionName: str(a, "session_name"),
			SessionID: str(a, "session_id"),
			Project:   str(a, "project"), Label: str(a, "label"), LabelManual: boolArg(a, "label_manual"),
		}
		if err := d.s.RegisterAgent(newAgent); err != nil {
			return fail(err)
		}
		// Reconciliation (spec §3): rewrite the badge for both the OLD tuple
		// (a re-register that moves an agent to a new session must not leave
		// its previous session's flag stale) and the NEW one.
		if hadOld {
			d.reconcileBadge(old.SocketPath, old.SessionID)
		}
		d.reconcileBadge(newAgent.SocketPath, newAgent.SessionID)
		return ok(nil)
	case "list_agents":
		agents, err := d.s.ListAgents()
		if err != nil {
			return fail(err)
		}
		return ok(agents)
	case "send_message":
		id, err := d.s.CreateThread(store.Thread{
			Kind: "message", FromAgent: str(a, "from"), ToKind: str(a, "to_kind"),
			ToTarget: str(a, "to_target"), Subject: str(a, "subject"), Ref: str(a, "ref"),
			Intent: str(a, "intent"),
		}, str(a, "body"))
		if err != nil {
			return fail(err)
		}
		d.logEvent(store.Event{Kind: "send", Agent: str(a, "from"), Target: targetOf(a), ThreadID: id, Detail: str(a, "subject")})
		d.notifyForThread(id, str(a, "from"))
		return ok(map[string]any{"thread_id": id})
	case "task_create":
		id, err := d.s.CreateThread(store.Thread{
			Kind: "task", FromAgent: str(a, "from"), ToKind: str(a, "to_kind"),
			ToTarget: str(a, "to_target"), Subject: str(a, "subject"), Ref: str(a, "ref"), Status: "open",
			Intent: str(a, "intent"),
		}, str(a, "body"))
		if err != nil {
			return fail(err)
		}
		d.logEvent(store.Event{Kind: "task", Agent: str(a, "from"), Target: targetOf(a), ThreadID: id, Detail: str(a, "subject")})
		d.notifyForThread(id, str(a, "from"))
		return ok(map[string]any{"thread_id": id})
	case "task_claim":
		if err := d.s.ClaimTask(i64(a, "thread_id"), str(a, "by")); err != nil {
			return fail(err)
		}
		d.logEvent(store.Event{Kind: "claim", Agent: str(a, "by"), ThreadID: i64(a, "thread_id")})
		d.notifyForThread(i64(a, "thread_id"), str(a, "by"))
		return ok(nil)
	case "task_transition":
		if err := d.s.TransitionTask(i64(a, "thread_id"), str(a, "by"), str(a, "status"), str(a, "note")); err != nil {
			return fail(err)
		}
		d.logEvent(store.Event{Kind: "transition", Agent: str(a, "by"), ThreadID: i64(a, "thread_id"), Detail: str(a, "status")})
		d.notifyForThread(i64(a, "thread_id"), str(a, "by"))
		return ok(nil)
	case "reply":
		id, err := d.s.AppendEntry(i64(a, "thread_id"), str(a, "from"), str(a, "body"), "")
		if err != nil {
			return fail(err)
		}
		d.logEvent(store.Event{Kind: "reply", Agent: str(a, "from"), ThreadID: i64(a, "thread_id"), Detail: display.Sanitize(str(a, "body"), replyPreviewWidth)})
		d.notifyForThread(i64(a, "thread_id"), str(a, "from"))
		return ok(map[string]any{"entry_id": id})
	case "get_inbox":
		alias := str(a, "alias")
		threads, err := d.s.Inbox(alias)
		if err != nil {
			return fail(err)
		}
		// A read that didn't persist must not report success (spec §3): if
		// MarkRead fails, the op fails outright — no read event, badge
		// untouched.
		if err := d.s.MarkRead(alias); err != nil {
			return fail(err)
		}
		detail := ""
		if d.n != nil {
			if ag, found, _ := d.s.GetAgent(alias); found && ag.SocketPath != "" && ag.SessionID != "" {
				if _, err := d.setSessionBadge(ag.SocketPath, ag.SessionID); err != nil {
					detail = "error: " + err.Error()
				}
			}
		}
		d.logEvent(store.Event{Kind: "read", Agent: alias, Detail: detail})
		return ok(threads)
	case "session_aliases":
		socketPath, sessionID := str(a, "socket_path"), str(a, "session_id")
		if socketPath == "" || sessionID == "" {
			return fail(fmt.Errorf("session_aliases: socket_path and session_id are required"))
		}
		agents, err := d.s.ListAgents()
		if err != nil {
			return fail(err)
		}
		aliases := []string{}
		for _, ag := range agents {
			if ag.SocketPath == socketPath && ag.SessionID == sessionID {
				aliases = append(aliases, ag.Alias)
			}
		}
		sort.Strings(aliases)
		aliases = compactStrings(aliases)
		return ok(map[string]any{"aliases": aliases})
	case "session_unread":
		// Read-only display data (spec §3/§4 hook wiring): no lock needed —
		// unlike setSessionBadge, this neither mutates the tmux badge nor
		// journals anything, so there is nothing for the session lock to
		// serialize against.
		socketPath, sessionID := str(a, "socket_path"), str(a, "session_id")
		if socketPath == "" || sessionID == "" {
			return fail(fmt.Errorf("session_unread: socket_path and session_id are required"))
		}
		total, action, err := d.s.SessionUnread(socketPath, sessionID)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"total": total, "action": action})
	case "get_thread":
		th, entries, err := d.s.GetThread(i64(a, "thread_id"))
		if err != nil {
			return fail(err)
		}
		entries = paginateEntries(entries, i64(a, "offset"), i64(a, "limit"))
		return ok(map[string]any{"thread": th, "entries": entries})
	case "list_threads":
		threads, err := d.s.Threads(int(i64(a, "limit")))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"threads": threads})
	case "kv_set":
		if err := d.s.KVSet(str(a, "key"), str(a, "value"), str(a, "by")); err != nil {
			return fail(err)
		}
		return ok(nil)
	case "kv_get":
		p, found, err := d.s.KVGet(str(a, "key"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"found": found, "pair": p})
	case "log_event":
		target, detail := str(a, "target"), str(a, "detail")
		if detail != "typed" && detail != "submitted" {
			return fail(fmt.Errorf("log_event: detail must be typed|submitted"))
		}
		if _, found, err := d.s.GetAgent(target); err != nil || !found {
			return fail(fmt.Errorf("log_event: unknown target %q", target))
		}
		// The daemon constructs the canonical event; client fields beyond
		// target/detail are ignored so the journal can't be polluted.
		d.logEvent(store.Event{Kind: "nudge", Target: target, Detail: detail})
		return ok(nil)
	case "list_events":
		evs, err := d.s.Events(store.EventQuery{
			Agent: str(a, "agent"), Kind: str(a, "kind"),
			ThreadID: i64(a, "thread_id"), AfterID: i64(a, "after_id"),
			Limit: int(i64(a, "limit")), Backlog: boolArg(a, "backlog"),
		})
		if err != nil {
			return fail(err)
		}
		maxID, err := d.s.MaxEventID()
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"events": evs, "max_id": maxID})
	case "prune_events":
		cutoff := i64(a, "older_than_ms")
		if cutoff <= 0 {
			return fail(fmt.Errorf("older_than_ms must be > 0"))
		}
		n, err := d.s.PruneEvents(cutoff)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"pruned": n})
	case "get_agent":
		ag, found, err := d.s.GetAgent(str(a, "alias"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"found": found, "agent": ag})
	case "deregister_agent":
		alias := str(a, "alias")
		old, hadOld, _ := d.s.GetAgent(alias) // best-effort: capture the tuple to reconcile after deletion
		if err := d.s.DeleteAgent(alias); err != nil {
			return fail(err)
		}
		if hadOld {
			d.reconcileBadge(old.SocketPath, old.SessionID)
		}
		return ok(nil)
	default:
		return proto.Response{Error: "unknown op: " + req.Op}
	}
}

// paginateEntries slices entries (already ordered oldest-first by
// GetThread) by offset and limit. Both are optional; absent/0 for BOTH
// returns entries unchanged (spec: back-compat with every existing MCP/CLI
// caller, none of which pass either arg). offset skips that many entries
// from the start; limit caps how many follow (0 = no cap, i.e. "the rest").
// A negative offset clamps to 0; an offset at or past the end returns an
// empty (non-nil) slice rather than panicking or wrapping.
func paginateEntries(entries []store.Entry, offset, limit int64) []store.Entry {
	if offset <= 0 && limit <= 0 {
		return entries
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= int64(len(entries)) {
		return []store.Entry{}
	}
	end := int64(len(entries))
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return entries[offset:end]
}

// compactStrings removes adjacent duplicates from a sorted slice, in place.
func compactStrings(sorted []string) []string {
	if len(sorted) == 0 {
		return sorted
	}
	out := sorted[:1]
	for _, s := range sorted[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
