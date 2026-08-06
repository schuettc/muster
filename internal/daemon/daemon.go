// Package daemon serves the muster store over a unix socket.
package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
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

// Daemon owns the listener and, depending on the mode it was built in, either
// a local store (Serve/New) or an upstream to forward to (ServeRemote).
// Exactly one of s and up is set; up != nil IS the mode flag — see remotemode.go.
//
// The storeAPI interface upstream declares here is deliberately absent: this
// branch promoted it to store.API (internal/store/api.go), the single
// canonical declaration both backends implement. Upstream's additions to it
// land there instead, so a method cannot exist for the daemon and not for
// dynamostore.
type Daemon struct {
	ln net.Listener
	s  store.API
	n  wake.Notifier

	// up, deviceID and the reconcile coalescer are the remote-mode half; all
	// are zero in local mode, which is what keeps local behaviour identical.
	up       Upstream
	deviceID string

	// recClosed is set by Close and is the reconcile loop's stop signal;
	// recWG is how Close waits for an in-flight reconcile to finish, so a
	// closed daemon can no longer call upstream or write a tmux option.
	recMu      sync.Mutex
	recRunning bool
	recPending bool
	recClosed  bool
	recWG      sync.WaitGroup
	// recStop is closed by Close so a goroutine PARKED on a timer (the poller
	// between ticks) wakes immediately instead of holding Close open for a
	// whole poll interval. recClosed is still the authority on "stopped"; this
	// is only how a sleeper hears about it promptly.
	recStop chan struct{}

	// localAgents is which aliases have registered through THIS daemon and
	// which session each sits in — the device-local roster remote mode's
	// poller consults before spending an upstream call. Populated by forward
	// (the only path a registration can take on this device) and seeded once
	// from upstream, so a daemon restarted under live agents still polls.
	localMu     sync.Mutex
	localAgents map[string]store.SessionRef
	localSeeded bool

	// sessLocks serializes {SessionUnread recompute, tmux option write,
	// journal} per (socket_path, session_id) tuple (spec §3): a concurrent
	// notify and get_inbox drain on the same session must not race, or the
	// smaller post-drain count can be overwritten by a stale in-flight
	// larger one. Keyed by sessionKey; created lazily under sessMu.
	sessMu    sync.Mutex
	sessLocks map[string]*sync.Mutex
}

// New builds a Daemon over s with no listener bound. Lambda mode uses this to
// get a Dispatch target without a unix socket; n may be nil, in which case no
// notifications are delivered.
func New(s store.API, n wake.Notifier) *Daemon {
	return &Daemon{s: s, n: n, recStop: make(chan struct{})}
}

// Serve binds socketPath (replacing any stale socket) and serves in a
// goroutine. n may be nil, in which case no notifications are delivered.
func Serve(socketPath string, s store.API, n wake.Notifier) (*Daemon, error) {
	_ = os.Remove(socketPath) // clear a stale socket from a previous run
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	d := New(s, n)
	d.ln = ln
	go d.acceptLoop()
	return d, nil
}

// Close stops accepting connections and shuts down remote mode's background
// reconcile, waiting for an in-flight one to finish. After it returns, the
// daemon makes no further upstream calls and writes no further tmux options —
// including from ReconcileLocalSessions, which the poller may still call from
// outside this package. Safe on a Daemon built by New (no listener), and safe
// to call more than once.
func (d *Daemon) Close() error {
	d.stopReconcile()
	if d.ln == nil {
		return nil
	}
	return d.ln.Close()
}

// stopReconcile latches the reconcile loop shut and waits for whatever is
// already running. The latch is set under recMu — the same lock triggerReconcile
// takes before recWG.Add — so no new reconcile can be added after Wait begins.
func (d *Daemon) stopReconcile() {
	d.recMu.Lock()
	alreadyClosed := d.recClosed
	d.recClosed = true
	if !alreadyClosed && d.recStop != nil {
		close(d.recStop) // wake the poller out of its inter-tick sleep
	}
	d.recMu.Unlock()
	d.recWG.Wait()
}

// reconcileStopped reports whether Close has latched the loop shut.
func (d *Daemon) reconcileStopped() bool {
	d.recMu.Lock()
	defer d.recMu.Unlock()
	return d.recClosed
}

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
		_ = enc.Encode(d.serve(req))
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
	return d.namedLock(sessionKey(socketPath, sessionID))
}

// aliasKeyPrefix distinguishes aliasLock's keys from sessionLock's in the
// same underlying map — sessionKey values are socket_path\x00session_id, and
// socket paths are always absolute (start with "/"), so a key starting with
// this prefix can never collide with one sessionLock produces.
const aliasKeyPrefix = "\x01alias\x00"

// aliasLock returns the mutex guarding one alias's register_agent CAS check
// (see handleRegisterAgent's if_absent path), creating it lazily. Distinct
// namespace from sessionLock's tuple-keyed locks (see aliasKeyPrefix).
func (d *Daemon) aliasLock(alias string) *sync.Mutex {
	return d.namedLock(aliasKeyPrefix + alias)
}

// namedLock is the shared lazy-mutex-map machinery behind sessionLock and
// aliasLock — same map, disjoint key namespaces.
func (d *Daemon) namedLock(key string) *sync.Mutex {
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

// handleRegisterAgent implements register_agent. Plain calls (if_absent
// absent/false) upsert exactly as before — every existing caller is
// unaffected. if_absent=true additionally guards the read-then-write gap a
// caller like station's collision-safe probe loop would otherwise have
// between its own get_agent check and this write: under the alias's lock, if
// a record for alias already exists with a DIFFERENT (socket_path,
// session_id) tuple than the one being registered, the op fails instead of
// upserting, rather than silently overwriting whatever raced onto the alias
// in between. A dead-collision "take over" (the caller's own tmux liveness
// check already decided the existing record is stale) intentionally does
// NOT set if_absent — the daemon has no tmux liveness of its own (it stays
// tmux-agnostic per the package boundary), so it cannot distinguish "safe to
// steal, the old tuple is dead" from "a live collision just raced in" on its
// own; that distinction is made client-side before calling in, exactly as it
// is today. The residual TOCTOU — the caller's liveness check and this write
// are not atomic with each other — is accepted for same-machine use: at
// worst the caller's write fails on if_absent and it fails over to the next
// candidate alias, but it can never clobber a tuple it didn't already decide
// (via that liveness check) to overwrite.
func (d *Daemon) handleRegisterAgent(a map[string]any) proto.Response {
	alias := str(a, "alias")
	ifAbsent := boolArg(a, "if_absent")
	newAgent := store.Agent{
		Alias: alias, Role: str(a, "role"), ModelType: str(a, "model_type"),
		SocketPath: str(a, "socket_path"), PaneID: str(a, "pane_id"), SessionName: str(a, "session_name"),
		SessionID: str(a, "session_id"), SessionCreated: i64(a, "session_created"),
		// device_id is stamped by the forwarding daemon in remote mode (see
		// remotemode.go) and absent from every local client's args, so local
		// mode records "" exactly as it did before this column existed.
		DeviceID:         str(a, "device_id"),
		HarnessSessionID: str(a, "harness_session_id"),
		Project:          str(a, "project"), Label: str(a, "label"), LabelManual: boolArg(a, "label_manual"),
	}

	var mu *sync.Mutex
	if ifAbsent {
		mu = d.aliasLock(alias)
		mu.Lock()
		defer mu.Unlock()
	}

	old, hadOld, err := d.s.GetAgent(alias) // pre-mutation tuple, for reconciliation AND the if_absent CAS check
	if err != nil {
		return fail(err)
	}
	// The CAS compares the FULL session identity, device included (see
	// store.API): on a shared roster a colliding tuple on another machine
	// would otherwise read as "the same session", and the guard whose whole
	// job is refusing a silent takeover would wave one through.
	if ifAbsent && hadOld && (old.DeviceID != newAgent.DeviceID ||
		old.SocketPath != newAgent.SocketPath || old.SessionID != newAgent.SessionID) {
		return fail(fmt.Errorf("register_agent: if_absent conflict: alias %q is already registered to a different session", alias))
	}
	if err := d.s.RegisterAgent(newAgent); err != nil {
		return fail(err)
	}
	// Ghost reaping: tombstone sibling aliases claiming this same (socket,
	// session_id) tuple under a DIFFERENT non-zero session_created — leftovers
	// from a previous tmux server incarnation whose session ID was recycled
	// (tmux numbers sessions from $0 again after a server restart). The
	// registrant just captured session_created from the pane it is live in, so
	// any sibling recorded under another creation time is provably dead — a
	// pure stored-data inference, keeping the daemon tmux-agnostic. Must run
	// before the badge reconciliation below so the pushed alias list never
	// includes a ghost.
	//
	// Scoped to the registrant's OWN device: on a shared roster the tuple is
	// not device-unique (see store.API), and another machine's live agent
	// under a colliding tuple has an unrelated session_created — it would look
	// exactly like a ghost and be tombstoned.
	if _, err := d.s.DepartStaleSiblings(newAgent.DeviceID, newAgent.SocketPath, newAgent.SessionID, newAgent.SessionCreated, alias); err != nil {
		return fail(err)
	}
	// Reconciliation (spec §3): rewrite the badge for both the OLD tuple
	// (a re-register that moves an agent to a new session must not leave
	// its previous session's flag stale) and the NEW one.
	if hadOld {
		d.reconcileBadge(old.SocketPath, old.SessionID, old.SessionCreated)
	}
	d.reconcileBadge(newAgent.SocketPath, newAgent.SessionID, newAgent.SessionCreated)
	// Outcome classification (durable-alias spec change 1): the pre-mutation
	// row read above already tells this apart for free — no prior row is a
	// first sight, a live prior row is a tuple refresh, and a tombstone is a
	// returning session (RegisterAgent's upsert just revived it). The unread
	// count rides along so a resuming session learns its backlog in the same
	// call; a count failure degrades to 0 rather than failing a register that
	// already succeeded.
	outcome := "new"
	switch {
	case hadOld && old.Departed:
		outcome = "revived"
		d.logEvent(store.Event{Kind: "register", Agent: alias, Detail: "revived"})
	case hadOld:
		outcome = "refreshed"
	}
	unread, err := d.s.UnreadCount(alias)
	if err != nil {
		unread = 0
	}
	return ok(map[string]any{"outcome": outcome, "unread": unread})
}

// setSessionBadge is the ONE canonical {recompute, push} sequence for a
// session's tmux badge (spec §3): under the session's lock, recompute the
// total unread via d.sessionUnread — the local store's SessionUnread in local
// mode, the same op upstream in remote mode (never sum per-alias UnreadCount —
// that double-counts threads shared by sibling aliases), then push it to the
// notifier — Notify(total) when total > 0, Clear otherwise. Both notify's
// fan-out and get_inbox's drain funnel through this so a concurrent pair
// always leaves the badge at whichever recompute ran last, never a stale
// interleaved value. Callers journal using the returned total/err; on err,
// callers must journal "error: …" and must NOT treat it as a cleared badge.
//
// sessionCreated is the incarnation being badged (spec §5.1): the caller
// passes the SessionCreated of whatever row put this tuple in scope, so the
// recompute counts only mail belonging to one tmux-session incarnation — a
// legacy or ghost row sitting on a recycled session ID can no longer inflate
// a live session's badge.
//
// The caller's value is a FALLBACK, not the last word: sessionIncarnation
// re-resolves the tuple from stored rows first. The count is keyed
// (socket, session, created) but the badge itself is keyed (socket, session)
// — only one incarnation of a session ID can exist in tmux at a time — so a
// recompute driven by the wrong row's incarnation would write an
// authoritative zero over a live session's real mail (the ghost-sorts-first
// notify grouping, and reconcile driven by a departing ghost's own row).
// Resolving here, inside the ONE canonical {recompute, push} sequence, means
// no call site can reintroduce that skew.
func (d *Daemon) setSessionBadge(socketPath, sessionID string, sessionCreated int64) (total int, err error) {
	mu := d.sessionLock(socketPath, sessionID)
	mu.Lock()
	defer mu.Unlock()
	total, err = d.sessionUnread(socketPath, sessionID, d.sessionIncarnation(socketPath, sessionID, sessionCreated))
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

// sessionIncarnation resolves which incarnation currently OCCUPIES a tmux
// tuple, from stored rows only (the daemon stays tmux-agnostic — it never
// queries tmux). The answer is the highest non-zero session_created among
// the tuple's non-departed rows: a live registrant captured that value from
// the pane it is running in, so a row carrying it is proof the session
// exists under that incarnation, while a 0-created row (pre-v0.8.0, or a
// registrant outside tmux) proves nothing and never wins. Departed rows are
// excluded — a tombstone is exactly the row whose incarnation is over.
//
// fallback is returned when no row proves an incarnation for the tuple: with
// nothing to correct to, the caller's own value stands (and for a tuple
// holding only 0-created rows that value is itself 0, which seeds nothing —
// attribution requires proof, spec §5.1).
//
// REMOTE MODE resolves ELSEWHERE, and this returns fallback untouched. There
// is no local store to read, and the roster upstream is the whole bus's — a
// tuple colliding on a peer machine would contribute its own unrelated
// creation times, so resolving here against an un-narrowed roster would be
// worse than not resolving at all. Both remote callers of setSessionBadge
// therefore resolve first, against a roster narrowed to THIS DEVICE, and pass
// the proven value in: ReconcileLocalSessions from the roster it already
// holds, and reconcileSessions from one it fetches once per batch. The
// asymmetry is real and is why this is a seam rather than a bare read; the
// property it protects — a badge write is driven by the incarnation that
// OCCUPIES the tuple, never by whichever row happened to name it — holds in
// both modes.
func (d *Daemon) sessionIncarnation(socketPath, sessionID string, fallback int64) int64 {
	if d.up != nil {
		return fallback
	}
	agents, err := d.s.ListAgents()
	if err != nil {
		return fallback // best-effort: a roster read failure must not change badge behavior
	}
	return sessionIncarnationOf(agents, socketPath, sessionID, fallback)
}

// sessionIncarnationOf is sessionIncarnation over an already-loaded roster,
// for callers that hold one — notifyForThread in local mode, and both remote
// reconcile paths (which MUST narrow that roster to this device first; see
// sessionIncarnation). Same rule, no second read.
func sessionIncarnationOf(agents []store.Agent, socketPath, sessionID string, fallback int64) int64 {
	var proven int64
	for _, a := range agents {
		if a.Departed || a.SocketPath != socketPath || a.SessionID != sessionID {
			continue
		}
		if a.SessionCreated > proven {
			proven = a.SessionCreated
		}
	}
	if proven == 0 {
		return fallback
	}
	return proven
}

// reconcileBadge is setSessionBadge for identity-change call sites
// (register/deregister/purge) that don't have a thread/journal-row shape to
// produce: best-effort, silent on an empty tuple or a nil notifier (there is
// no tmux badge to reconcile in either case). Identity changes are also the
// only moments a session's alias list can change, so this additionally
// pushes the agent badge (@muster_agent) — the operator's ambient
// "registered as X" indicator.
func (d *Daemon) reconcileBadge(socketPath, sessionID string, sessionCreated int64) {
	if d.n == nil || socketPath == "" || sessionID == "" {
		return
	}
	_, _ = d.setSessionBadge(socketPath, sessionID, sessionCreated)
	d.pushSessionAgents(socketPath, sessionID)
}

// sessionAliasesFor returns the sorted, deduplicated LIVE alias list for a
// session tuple — departed (tombstoned) agents excluded, since the agent
// badge advertises who is currently addressable there. Distinct from the
// session_aliases op, which includes departed aliases on purpose (their
// unread mail still needs draining).
func (d *Daemon) sessionAliasesFor(socketPath, sessionID string) ([]string, error) {
	agents, err := d.s.ListAgents()
	if err != nil {
		return nil, err
	}
	return liveAliasesFor(agents, socketPath, sessionID), nil
}

// liveAliasesFor is sessionAliasesFor's filter over a roster already in hand —
// the one place the "live aliases of this session tuple" rule is written, so
// the local path (which reads the roster from the store) and the remote one
// (which reads it upstream) cannot disagree about who the badge advertises.
func liveAliasesFor(agents []store.Agent, socketPath, sessionID string) []string {
	aliases := []string{}
	for _, ag := range agents {
		if ag.SocketPath == socketPath && ag.SessionID == sessionID && !ag.Departed {
			aliases = append(aliases, ag.Alias)
		}
	}
	sort.Strings(aliases)
	return compactStrings(aliases)
}

// pushSessionAgents recomputes and pushes the agent badge under the session's
// lock (same serialization contract as setSessionBadge: last recompute wins,
// no stale interleaved write). Best-effort: a store or tmux error leaves the
// previous badge in place — the roster stays authoritative.
func (d *Daemon) pushSessionAgents(socketPath, sessionID string) {
	mu := d.sessionLock(socketPath, sessionID)
	mu.Lock()
	defer mu.Unlock()
	aliases, err := d.sessionAliasesFor(socketPath, sessionID)
	if err != nil {
		return
	}
	_ = d.n.SetAgents(socketPath, sessionID, aliases)
}

// pushAgentBadge writes an already-computed alias list to the agent badge
// under the session's lock — remote mode's counterpart to pushSessionAgents,
// which recomputes inside the lock. It reads its roster once per reconcile
// rather than once per session, so the recompute cannot sit inside the
// per-session lock; ReconcileLocalSessions' coalescer is what keeps two
// reconciles from interleaving a stale roster over a fresh one (see
// triggerReconcile).
func (d *Daemon) pushAgentBadge(socketPath, sessionID string, aliases []string) {
	mu := d.sessionLock(socketPath, sessionID)
	mu.Lock()
	defer mu.Unlock()
	_ = d.n.SetAgents(socketPath, sessionID, aliases)
}

// notifyForThread flags every SESSION affected by activity on threadID — the
// thread's originator plus its recipients (agent/role/broadcast), minus the
// actor's entire session — coalescing sibling aliases of one (socket_path,
// session_id) tuple into a single recompute/notify/journal (spec §3: no
// duplicate lit rows for sibling aliases sharing a session). Agents with no
// tmux identity (empty socket or session) can never carry a tmux badge, so
// they are journaled "skipped" exactly as before, one row per alias. Best-
// effort; never types into a pane.
//
// The tuple grouping below is device-BLIND, and may only stay that way while
// this runs against a roster of one device's agents. It does: local mode's
// store holds nothing else, and in hosted mode this returns at the nil
// notifier above (a Lambda has no tmux to badge — see internal/lambdamode), so
// remote devices reconcile from device_poll instead. Give the hosted daemon a
// notifier and this needs the device dimension every other session-scoped
// surface now carries (store.API), or two machines' sessions coalesce into one
// group and one of them silently never gets its badge.
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
			if th.ToTarget == "" || a.Project == th.ToTarget {
				recipients[a.Alias] = struct{}{}
			}
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

	type sessionGroup struct {
		socketPath, sessionID, journalAlias string
		sessionCreated                      int64
	}
	seen := make(map[string]bool, len(aliases))
	var groups []sessionGroup
	for _, alias := range aliases {
		a, found := byAlias[alias]
		if !found || a.SocketPath == "" || a.SessionID == "" {
			d.logEvent(store.Event{Kind: "notify", Agent: alias, ThreadID: threadID, Detail: "skipped: no tmux identity"})
			continue
		}
		// A departed (tombstoned) agent keeps its last-known tuple on the row
		// (DepartAgent never clears it), so it would otherwise pass the tmux-
		// identity check above and get grouped for a badge write into a
		// session nobody is watching anymore. Skipped exactly like the
		// no-tmux-identity case, with its own journaled reason — mail still
		// waits for them (addressing is unchanged), it's only the tmux badge
		// push that's pointless.
		if a.Departed {
			d.logEvent(store.Event{Kind: "notify", Agent: alias, ThreadID: threadID, Detail: "skipped: departed"})
			continue
		}
		key := sessionKey(a.SocketPath, a.SessionID)
		if seen[key] {
			continue // sibling alias of an already-scheduled session
		}
		seen[key] = true
		// The group's incarnation is the tuple's PROVEN one, not this
		// alias's: recipients are grouped by the bare (socket, session) key,
		// so whichever alias sorts first would otherwise decide it — and a
		// legacy 0-created ghost sharing a recycled session ID would drive
		// the whole group's recompute to zero (see
		// TestNotifyBadgeIgnoresGhostIncarnationOrdering). a.SessionCreated
		// stays the fallback for a tuple no row proves.
		groups = append(groups, sessionGroup{
			socketPath: a.SocketPath, sessionID: a.SessionID,
			sessionCreated: sessionIncarnationOf(agents, a.SocketPath, a.SessionID, a.SessionCreated),
			journalAlias:   alias,
		})
	}

	for _, g := range groups {
		total, err := d.setSessionBadge(g.socketPath, g.sessionID, g.sessionCreated)
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

// senderProject resolves alias's CURRENTLY registered project for stamping
// onto a new thread's origin_project (iteration-4 orphan-thread fix, spec
// queue item 4b): "" when alias isn't a registered agent (never registered,
// or a lookup error) — an unregistered sender's thread simply carries no
// origin project, exactly like every other best-effort roster lookup in this
// file (e.g. register/deregister's own discarded GetAgent error).
func (d *Daemon) senderProject(alias string) string {
	ag, found, err := d.s.GetAgent(alias)
	if err != nil || !found {
		return ""
	}
	return ag.Project
}

// validateBroadcastTarget is the send-time backstop for scoped broadcasts
// (to_kind=broadcast, to_target=<project>): reject unless some non-departed
// agent is registered under exactly that project, so a typo'd project never
// creates a thread that concerns nobody — the same black-hole principle as
// resolveAgentTarget for mistyped aliases. A global broadcast (empty
// target) is never validated. Exact match only; project strings come from
// tmux capture and are stable, so no fuzzy or prefix matching.
func (d *Daemon) validateBroadcastTarget(project string) error {
	if project == "" {
		return nil
	}
	agents, err := d.s.ListAgents()
	if err != nil {
		return err
	}
	known := make(map[string]bool)
	for _, ag := range agents {
		if !ag.Departed && ag.Project != "" {
			known[ag.Project] = true
		}
	}
	if known[project] {
		return nil
	}
	names := make([]string, 0, len(known))
	for p := range known {
		names = append(names, p)
	}
	sort.Strings(names)
	return fmt.Errorf("no registered agents in project %q (known projects: %s)", project, strings.Join(names, ", "))
}

// targetOf renders a thread address as a journal target: 'broadcast' or
// '<to_kind>:<to_target>'. toTarget is the (possibly daemon-resolved) target
// actually stored on the thread, not necessarily the raw request arg — so a
// send_message/task_create addressed to a label journals the ALIAS it
// resolved to, not the label a caller typed. A scoped broadcast journals
// 'broadcast:<project>'.
func targetOf(toKind, toTarget string) string {
	if toKind == "broadcast" {
		if toTarget == "" {
			return "broadcast"
		}
		return "broadcast:" + toTarget
	}
	return toKind + ":" + toTarget
}

// dispatch runs one request against the store/notifier and returns its
// response, with no socket or connection involved — the seam lambda mode
// uses to route a request through the same op logic the socket-bound daemon
// uses. Callers go through Dispatch, which wraps this with idempotency.
func (d *Daemon) dispatch(req proto.Request) proto.Response {
	a := req.Args
	switch req.Op {
	case "register_agent":
		return d.handleRegisterAgent(a)
	case "list_agents":
		agents, err := d.s.ListAgents()
		if err != nil {
			return fail(err)
		}
		return ok(agents)
	case "send_message":
		from := str(a, "from")
		toKind, toTarget := str(a, "to_kind"), str(a, "to_target")
		if toKind == "agent" {
			resolved, err := d.resolveAgentTarget(from, toTarget)
			if err != nil {
				return fail(err)
			}
			toTarget = resolved
		}
		if toKind == "broadcast" {
			if err := d.validateBroadcastTarget(toTarget); err != nil {
				return fail(err)
			}
		}
		id, err := d.s.CreateThread(store.Thread{
			Kind: "message", FromAgent: from, ToKind: toKind,
			ToTarget: toTarget, Subject: str(a, "subject"), Ref: str(a, "ref"),
			Intent: str(a, "intent"), OriginProject: d.senderProject(from),
		}, str(a, "body"))
		if err != nil {
			return fail(err)
		}
		d.logEvent(store.Event{Kind: "send", Agent: from, Target: targetOf(toKind, toTarget), ThreadID: id, Detail: str(a, "subject")})
		d.notifyForThread(id, from)
		return ok(map[string]any{"thread_id": id})
	case "task_create":
		from := str(a, "from")
		toKind, toTarget := str(a, "to_kind"), str(a, "to_target")
		if toKind == "agent" {
			resolved, err := d.resolveAgentTarget(from, toTarget)
			if err != nil {
				return fail(err)
			}
			toTarget = resolved
		}
		if toKind == "broadcast" {
			if err := d.validateBroadcastTarget(toTarget); err != nil {
				return fail(err)
			}
		}
		id, err := d.s.CreateThread(store.Thread{
			Kind: "task", FromAgent: from, ToKind: toKind,
			ToTarget: toTarget, Subject: str(a, "subject"), Ref: str(a, "ref"), Status: "open",
			Intent: str(a, "intent"), OriginProject: d.senderProject(from),
		}, str(a, "body"))
		if err != nil {
			return fail(err)
		}
		d.logEvent(store.Event{Kind: "task", Agent: from, Target: targetOf(toKind, toTarget), ThreadID: id, Detail: str(a, "subject")})
		d.notifyForThread(id, from)
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
		// An fyi reply is a closer: the entry lands on the thread and shows
		// as unread on the recipients' next natural inbox check, but nobody's
		// badge lights — no wake means no reflexive ack back, which is what
		// breaks the ack-loop (each closing ack waking the peer into one more
		// closing ack).
		if !boolArg(a, "fyi") {
			d.notifyForThread(i64(a, "thread_id"), str(a, "from"))
		}
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
				if _, err := d.setSessionBadge(ag.SocketPath, ag.SessionID, ag.SessionCreated); err != nil {
					detail = "error: " + err.Error()
				}
			}
		}
		d.logEvent(store.Event{Kind: "read", Agent: alias, Detail: detail})
		return ok(threads)
	case "session_aliases":
		// socket_path may be empty: a paneless session's tuple is ("",
		// harness session UUID) — see internal/harnessenv. Only a missing
		// session_id leaves no tuple to key on.
		//
		// SessionAliasLineage (not a flat tuple filter over ListAgents) walks
		// supersession lineage the same way SessionUnread does: a
		// become-retired seed's row sits on its OLD tuple forever (reclaim
		// correctly never resurrects it there), so a flat filter would drop
		// it from the very session its identity moved to. Lineage rows are
		// additive to this op's existing include-departed-on-purpose
		// behavior — nothing here narrows what used to come back, it only
		// adds the aliases a flat tuple match was missing.
		//
		// session_created scopes the lineage walk's base case to one tmux
		// incarnation (spec §5.1). An absent arg reads 0: a tmux caller that
		// supplied no proof of incarnation gets an empty list (correct — it
		// cannot claim any row on a recyclable session ID), while a paneless
		// caller (empty socket_path) is unaffected.
		socketPath, sessionID := str(a, "socket_path"), str(a, "session_id")
		if sessionID == "" {
			return fail(fmt.Errorf("session_aliases: session_id is required"))
		}
		// Device-scoped like every other session-tuple surface (see
		// store.API): on a shared roster the pair alone would advertise a
		// colliding session on ANOTHER machine as one of this session's own
		// aliases — and this op's answer is what the hook drains and the
		// nudge path addresses. Local mode sends no device_id and every local
		// row carries "", so it matches exactly as before. Both scopings live
		// in SessionAliasLineage's base case, not in a post-filter here: the
		// supersession walk must stay free to leave the tuple AND the device
		// (see its doc).
		aliases, err := d.s.SessionAliasLineage(str(a, "device_id"), socketPath, sessionID, i64(a, "session_created"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"aliases": aliases})
	case "session_unread":
		// Read-only display data (spec §3/§4 hook wiring): no lock needed —
		// unlike setSessionBadge, this neither mutates the tmux badge nor
		// journals anything, so there is nothing for the session lock to
		// serialize against.
		// socket_path may be empty — the paneless tuple, as in session_aliases.
		// session_created carries the caller's live incarnation (spec §5.1);
		// absent reads 0, so a tmux caller with no proof gets zeros and a
		// paneless caller is unaffected — same contract as session_aliases.
		// Deliberately NOT run through sessionIncarnation: this op answers
		// the caller's own question, and resolving server-side would hand a
		// caller that proved nothing another incarnation's mail. The badge
		// path resolves precisely because there the DAEMON, not a caller, is
		// deciding which incarnation a (socket, session)-keyed tmux write
		// belongs to.
		socketPath, sessionID := str(a, "socket_path"), str(a, "session_id")
		if sessionID == "" {
			return fail(fmt.Errorf("session_unread: session_id is required"))
		}
		total, action, err := d.s.SessionUnread(str(a, "device_id"), socketPath, sessionID, i64(a, "session_created"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"total": total, "action": action})
	case "device_poll":
		// Remote mode's wake path for traffic originating on ANOTHER device:
		// the server holds both the roster and the entries, so it answers with
		// exactly the sessions this device must reconcile plus the watermark
		// to resume from — one round trip, no entries shipped to the laptop.
		// device_id is stamped by the polling daemon, never taken on trust
		// from a client (see stampDevice).
		res, err := d.s.DevicePoll(str(a, "device_id"), i64(a, "since_entry_id"))
		if err != nil {
			return fail(err)
		}
		return ok(res)
	case "get_thread":
		th, entries, err := d.s.GetThread(i64(a, "thread_id"))
		if err != nil {
			return fail(err)
		}
		total := len(entries) // live count BEFORE pagination — additive, back-compat (spec §5 carried-over fix: the newest-entries gap)
		entries = paginateEntries(entries, i64(a, "offset"), i64(a, "limit"))
		return ok(map[string]any{"thread": th, "entries": entries, "total": total})
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
		old, hadOld, _ := d.s.GetAgent(alias) // best-effort: capture the tuple to reconcile after tombstoning
		if err := d.s.DepartAgent(alias); err != nil {
			return fail(err)
		}
		if hadOld {
			d.reconcileBadge(old.SocketPath, old.SessionID, old.SessionCreated)
		}
		return ok(nil)
	case "set_label":
		// The bus-side half of `muster label` (see store.SetSessionLabel):
		// the CLI has already written the live tmux option; this lands the
		// same value in the store so the daemon's resolver (which never
		// re-reads tmux) agrees with the CLI's live-label resolution
		// immediately, not at the next register_agent upsert.
		// session_created scopes the write to the caller's proven incarnation
		// (spec §5.1): an old caller omitting it now updates 0 rows and the
		// CLI's existing "bus label sync failed / refreshes on next
		// register" warning path already communicates a 0-update as
		// silence — acceptable single-binary skew.
		n, err := d.s.SetSessionLabel(str(a, "device_id"), str(a, "socket_path"), str(a, "session_id"), i64(a, "session_created"), str(a, "label"), boolArg(a, "label_manual"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"updated": n})
	case "stamp_harness_session":
		if err := d.s.SetHarnessSessionID(str(a, "alias"), str(a, "harness_session_id")); err != nil {
			return fail(err)
		}
		return ok(nil)
	case "purge_agent":
		// The explicit, irreversible hard-delete: `muster gc --purge-agents`'s
		// own op, distinct from deregister_agent's tombstone. Identity,
		// project, label, and read-state are all gone once this returns.
		alias := str(a, "alias")
		old, hadOld, _ := d.s.GetAgent(alias) // best-effort: capture the tuple to reconcile after deletion
		if err := d.s.DeleteAgent(alias); err != nil {
			return fail(err)
		}
		if hadOld {
			d.reconcileBadge(old.SocketPath, old.SessionID, old.SessionCreated)
		}
		return ok(nil)
	case "become":
		// Claim a durable name: the seed's identity moves to `to` and the
		// seed retires (store.Become). unread is computed via SessionUnread
		// on the claimed alias's (socket_path, session_id) tuple, not
		// UnreadCount(to) — mail addressed to the pre-claim seed alias
		// concerns that alias's *text*, not the destination alias's, so
		// UnreadCount(to) would miss it. SessionUnread groups by the
		// session tuple, which now belongs to `to` post-claim, so it
		// correctly picks up mail that arrived before the claim. This
		// matches the spec's intent: "how much mail is waiting for this
		// session", reported so a caller can say "you are now X — N
		// unread" in one round trip.
		from, to := str(a, "from"), str(a, "to")
		if err := d.s.Become(from, to); err != nil {
			switch {
			case errors.Is(err, store.ErrBecomeFromMissing):
				// No self-prefix (finding F3): callData already renders
				// "<op>: <resp.Error>" for every op — send_message's resolve
				// errors follow the same convention (no "send_message: "
				// baked in here). A daemon error that also opened with
				// "become: " doubled into "become: become: ..." on the CLI.
				return fail(fmt.Errorf("no such alias %q to become from; register first", from))
			case errors.Is(err, store.ErrBecomeToExists):
				return fail(fmt.Errorf("alias %q already has history; pick another name, or purge it with `muster gc --purge-agents`", to))
			}
			return fail(err)
		}
		d.logEvent(store.Event{Kind: "become", Agent: to, Detail: from + " → " + to})
		ag, _, err := d.s.GetAgent(to)
		if err != nil {
			// store.Become above already committed: the claim itself succeeded,
			// so a GetAgent read failure here must not surface as an op failure
			// (finding F3) — a caller retrying on error would then hit
			// ErrBecomeToExists for a claim that already went through. Degrade
			// best-effort exactly like the unread lookup below already does:
			// skip the badge reconcile and report unread:0 instead of failing.
			return ok(map[string]any{"from": from, "to": to, "unread": 0})
		}
		d.reconcileBadge(ag.SocketPath, ag.SessionID, ag.SessionCreated)
		// The device comes from the CLAIMED ROW, not from d.deviceID: a
		// daemon fronting a shared store (lambda mode) serves many devices and
		// carries none of its own, and store.Become clones device_id from the
		// seed, so ag.DeviceID is the tuple this session actually sits on.
		// Local mode reads "" off the row and matches as it always did.
		//
		// The incarnation is the same one the badge just used
		// (sessionIncarnation), so the number reported back to the claimer and
		// the number on its tmux badge can never disagree.
		unread, _, err := d.s.SessionUnread(ag.DeviceID, ag.SocketPath, ag.SessionID,
			d.sessionIncarnation(ag.SocketPath, ag.SessionID, ag.SessionCreated))
		if err != nil {
			unread = 0 // best-effort: the claim already succeeded
		}
		return ok(map[string]any{"from": from, "to": to, "unread": unread})
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
