// Package daemon serves the muster store over a unix socket.
package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/wake"
)

// Daemon owns the listener and the store.
type Daemon struct {
	ln net.Listener
	s  *store.Store
	n  wake.Notifier
}

// Serve binds socketPath (replacing any stale socket) and serves in a
// goroutine. n may be nil, in which case no notifications are delivered.
func Serve(socketPath string, s *store.Store, n wake.Notifier) (*Daemon, error) {
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

// notifyForThread flags every agent affected by activity on threadID — the
// thread's originator plus its recipients (agent/role/broadcast), minus the
// actor — by notifying their tmux SESSION. Best-effort; never types into a pane.
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
	delete(recipients, actor)
	for alias := range recipients {
		a, ok := byAlias[alias]
		if !ok || a.SocketPath == "" || a.SessionID == "" {
			d.logEvent(store.Event{Kind: "notify", Agent: alias, ThreadID: threadID, Detail: "skipped: no tmux identity"})
			continue
		}
		count, err := d.s.UnreadCount(alias)
		if err != nil {
			d.logEvent(store.Event{Kind: "notify", Agent: alias, ThreadID: threadID, Detail: "error: " + err.Error()})
			continue
		}
		detail := "lit"
		if count <= 0 {
			detail = "cleared"
		}
		if err := d.n.Notify(a.SocketPath, a.SessionID, count); err != nil {
			detail = "error: " + err.Error()
		}
		d.logEvent(store.Event{Kind: "notify", Agent: alias, ThreadID: threadID, Count: count, Detail: detail})
	}
}

// logEvent appends to the observability event log, best-effort: logging must
// never fail or slow the bus operation it describes.
func (d *Daemon) logEvent(e store.Event) { _ = d.s.AppendEvent(e) }

func (d *Daemon) dispatch(req proto.Request) proto.Response {
	a := req.Args
	switch req.Op {
	case "register_agent":
		err := d.s.RegisterAgent(store.Agent{
			Alias: str(a, "alias"), Role: str(a, "role"), ModelType: str(a, "model_type"),
			SocketPath: str(a, "socket_path"), PaneID: str(a, "pane_id"), SessionName: str(a, "session_name"),
			SessionID: str(a, "session_id"),
			Project:   str(a, "project"), Label: str(a, "label"), LabelManual: boolArg(a, "label_manual"),
		})
		if err != nil {
			return fail(err)
		}
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
		}, str(a, "body"))
		if err != nil {
			return fail(err)
		}
		d.notifyForThread(id, str(a, "from"))
		return ok(map[string]any{"thread_id": id})
	case "task_create":
		id, err := d.s.CreateThread(store.Thread{
			Kind: "task", FromAgent: str(a, "from"), ToKind: str(a, "to_kind"),
			ToTarget: str(a, "to_target"), Subject: str(a, "subject"), Ref: str(a, "ref"), Status: "open",
		}, str(a, "body"))
		if err != nil {
			return fail(err)
		}
		d.notifyForThread(id, str(a, "from"))
		return ok(map[string]any{"thread_id": id})
	case "task_claim":
		if err := d.s.ClaimTask(i64(a, "thread_id"), str(a, "by")); err != nil {
			return fail(err)
		}
		return ok(nil)
	case "task_transition":
		if err := d.s.TransitionTask(i64(a, "thread_id"), str(a, "by"), str(a, "status"), str(a, "note")); err != nil {
			return fail(err)
		}
		d.notifyForThread(i64(a, "thread_id"), str(a, "by"))
		return ok(nil)
	case "reply":
		id, err := d.s.AppendEntry(i64(a, "thread_id"), str(a, "from"), str(a, "body"), "")
		if err != nil {
			return fail(err)
		}
		d.notifyForThread(i64(a, "thread_id"), str(a, "from"))
		return ok(map[string]any{"entry_id": id})
	case "get_inbox":
		alias := str(a, "alias")
		threads, err := d.s.Inbox(alias)
		if err != nil {
			return fail(err)
		}
		_ = d.s.MarkRead(alias)
		d.logEvent(store.Event{Kind: "read", Agent: alias})
		if d.n != nil {
			if ag, ok, _ := d.s.GetAgent(alias); ok && ag.SocketPath != "" && ag.SessionID != "" {
				_ = d.n.Clear(ag.SocketPath, ag.SessionID)
			}
		}
		return ok(threads)
	case "get_thread":
		th, entries, err := d.s.GetThread(i64(a, "thread_id"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"thread": th, "entries": entries})
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
	case "list_events":
		evs, err := d.s.Events(store.EventQuery{Agent: str(a, "agent"), Backlog: true, Limit: 50})
		if err != nil {
			return fail(err)
		}
		return ok(evs)
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
		if err := d.s.DeleteAgent(str(a, "alias")); err != nil {
			return fail(err)
		}
		return ok(nil)
	default:
		return proto.Response{Error: "unknown op: " + req.Op}
	}
}
