package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/schuettc/muster/internal/device"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/wake"
)

// Upstream is the hosted bus as this daemon sees it: one whole proto.Request
// in, one whole proto.Response out. internal/remote's Client implements it.
//
// It is declared HERE, and satisfied elsewhere, on purpose. internal/remote
// already imports this package for IsWriteOp — the single classifier for which
// ops carry an idempotency key — so a daemon that imported remote back would
// be an import cycle. cmd/muster wires the concrete client in, which also
// keeps the transport swappable (a test fake, a future gRPC client) without
// this package knowing anything about HTTP, tokens or retries.
type Upstream interface {
	Call(ctx context.Context, req proto.Request) (proto.Response, error)
}

// ServeRemote binds socketPath exactly as Serve does, but the daemon it
// returns has NO local store: every request arriving on the socket is
// forwarded to up verbatim and its response returned unchanged. Everything
// above the socket — the MCP server, the human CLI, station — is unaware of
// the difference, which is the whole point of the daemon being the API.
//
// What stays on the device is the socket and tmux wake. deviceID identifies
// this machine in the hosted roster: it is stamped onto forwarded
// register_agent calls, and it is how ReconcileLocalSessions tells this
// device's sessions from another's. It is required rather than optional
// because an empty one would match every agent registered before the column
// existed, and the daemon would write tmux badges for sessions on other
// machines.
//
// n may be nil, in which case no badges are written and reconcile is a no-op.
func ServeRemote(socketPath string, up Upstream, n wake.Notifier, deviceID, deviceName string) (*Daemon, error) {
	if up == nil {
		return nil, errors.New("daemon: remote mode requires an upstream")
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil, errors.New("daemon: remote mode requires a device id")
	}
	_ = os.Remove(socketPath) // clear a stale socket from a previous run
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	// s is nil: remote mode has no local store, and serve() always checks
	// d.up first and forwards, so dispatch (and resolveAgentTarget's use of
	// deviceName for expansion) is never reached from this mode — d.s and
	// the expansion deviceName enables are both unused here.
	d := New(nil, n, deviceName)
	d.up = up
	d.deviceID = deviceID
	d.ln = ln
	go d.acceptLoop()
	return d, nil
}

// serve answers one request in whichever mode this daemon was built in. up is
// the mode flag: local mode leaves it nil and reaches Dispatch (and its
// idempotency wrapper) exactly as it did before remote mode existed.
func (d *Daemon) serve(req proto.Request) proto.Response {
	if d.up != nil {
		return d.forward(req)
	}
	return d.Dispatch(req)
}

// forward sends one request upstream and returns what came back.
//
// It adds no idempotency key of its own — the transport mints one key per
// logical Call and holds it across its own retries, which is the only layer
// that knows what a retry is. It does not retry either, for the same reason.
//
// A write that can move a badge triggers a reconcile afterwards, whatever the
// outcome: this is the inline trigger that keeps same-device messaging as fast
// as it is in local mode, and it runs even on a failed or unknown-outcome
// forward precisely because those may still have committed upstream.
//
// The trigger is movesBadge, not IsWriteOp: the two classify different things
// (which ops need an idempotency key vs which ops change what a badge shows),
// and the ops in the gap — kv_set, log_event, set_label, prune_events — would
// each buy a full upstream fan-out for a badge that cannot have changed.
func (d *Daemon) forward(req proto.Request) proto.Response {
	if movesBadge(req.Op) {
		defer d.triggerReconcile()
	}
	if needsDevice(req.Op) {
		req = d.stampDevice(req)
	}
	if needsCallerDevice(req.Op) {
		req = d.stampCallerDevice(req)
	}
	req = d.expandAliasArg(req)

	resp, err := d.up.Call(context.Background(), req)
	if err != nil {
		// A transport failure has to reach the client as a protocol answer:
		// callers above the socket parse a proto.Response and nothing else.
		return proto.Response{Error: "upstream " + req.Op + ": " + err.Error()}
	}
	if resp.OK {
		d.noteRosterChange(req)
	}
	if !resp.OK && IsNewKeyReissueError(resp.Error) {
		return d.unknownOutcome(req.Op, resp)
	}
	return resp
}

// expandableAliasArgs maps an op to the ONE request argument of its that
// names an agent alias a short local name may need expanding in. It is the
// remote-mode mirror of the two local-mode expansion sites:
//
//   - to_target on send_message/task_create — the ADDRESSEE, expanded by
//     resolveAgentTarget (resolve.go). Guarded additionally by to_kind: every
//     other to_kind, e.g. broadcast's project name, is not an alias and must
//     not be touched.
//   - by on task_claim/task_transition and alias on get_inbox — the ACTOR,
//     expanded by requireKnownAlias (resolve.go).
//
// Deliberately absent: `from`. It is gated upstream against the exact string
// (mcpserver.requireRegisteredFrom), and `muster send --from operator` is the
// operator's documented escape hatch for sending as an alias nobody
// registered — expanding it would rewrite a name chosen on purpose.
//
// forward is the ONLY path a remote-mode daemon's requests take (serve checks
// d.up first, see forward's doc), so it is the one place left that must apply
// these rules.
var expandableAliasArgs = map[string]string{
	"send_message":    "to_target",
	"task_create":     "to_target",
	"task_claim":      "by",
	"task_transition": "by",
	"get_inbox":       "alias",
}

// expandAliasArg is remote mode's counterpart to resolveAgentTarget and
// requireKnownAlias: a forwarded request never reaches Dispatch, so neither of
// those runs for it. Without this, a model-supplied short alias (an MCP caller
// passes these fields straight through with no client-side resolution) reaches
// the hosted bus literally — and in a multi-device roster, a bare or another
// device's alias of the same short name is a stranger's agent, not this
// device's.
//
// The rule matches resolve.go exactly: local-first. Try device.Seed against
// THIS device's name, and use the seeded form only if it already exists in
// the roster — an unexpandable name is forwarded untouched so the upstream's
// error names what was actually sent. "" disables expansion (Lambda mode,
// which serves many devices and must never guess one; ServeRemote's deviceName
// is never "" in practice, but the check is kept here too since forward has
// no other guard for it).
//
// Unlike resolveAgentTarget, this cannot do a full resolve.Target pass
// (label/project scoping): that needs the roster narrowed to the sender's
// project, which costs nothing extra when the roster is already a local
// store read, but here it's an upstream network fetch this function must NOT
// pay per request (see aliasesForExpansion). Exact short-alias expansion is
// the whole of what black-hole risk remote mode's single missing backstop
// covers; the fuller label/project resolution remote mode never had.
//
// An already-full alias — the common case, since every model surface reports
// the seeded form — costs nothing: device.Seed returns it unchanged and this
// returns before touching the roster cache. Only a genuinely short name pays a
// lookup, which is what keeps get_inbox (the hottest op here) cheap.
func (d *Daemon) expandAliasArg(req proto.Request) proto.Request {
	if d.deviceName == "" {
		return req
	}
	key, ok := expandableAliasArgs[req.Op]
	if !ok {
		return req
	}
	if key == "to_target" && str(req.Args, "to_kind") != "agent" {
		return req
	}
	given, _ := req.Args[key].(string)
	if given == "" {
		return req
	}
	seeded := device.Seed(d.deviceName, given)
	if seeded == given {
		return req
	}
	aliases, ok := d.aliasesForExpansion()
	if !ok || !aliases[seeded] {
		return req
	}

	// Copied before mutation, same reason as stampDevice: the caller's map
	// came off the wire here, and forward has no license to write into it.
	args := make(map[string]any, len(req.Args))
	for k, v := range req.Args {
		args[k] = v
	}
	args[key] = seeded
	req.Args = args
	return req
}

// aliasCacheTTL bounds how long expandAliasArg trusts its cached roster
// before paying a fresh upstream list_agents round trip. Short enough that a
// peer who just registered becomes expandable within one human-perceptible
// pause; long enough that a burst of sends — the case this cache exists for —
// pays that round trip once rather than once per message.
const aliasCacheTTL = 15 * time.Second

// aliasesForExpansion returns the alias set expandAliasArg checks a seeded
// guess against, refreshing it from upstream when the cache is empty or
// older than aliasCacheTTL.
//
// STALENESS POLICY: ok is false whenever there is nothing trustworthy to
// check against — no cache has ever been populated, or the refresh attempt
// just failed — and expandAliasArg's contract for that is to forward the
// input UNEXPANDED, never to fall back to whatever the previous cache
// contents were. This is the deliberately safe direction: an unexpanded
// short alias fails loudly at the upstream resolver as an unknown target,
// while a wrongly-expanded one is delivered silently to the wrong agent's
// inbox. A stale-but-present cache (younger than the TTL) is still trusted —
// that staleness is bounded and accepted on purpose, the same trade every
// TTL cache makes — but a cache old enough to need refreshing is not reused
// merely because refreshing it failed.
func (d *Daemon) aliasesForExpansion() (map[string]bool, bool) {
	d.aliasMu.Lock()
	fresh := d.aliasSet != nil && time.Since(d.aliasAt) < aliasCacheTTL
	set := d.aliasSet
	d.aliasMu.Unlock()
	if fresh {
		return set, true
	}

	agents, err := d.upstreamAgents()
	if err != nil {
		return nil, false
	}
	next := make(map[string]bool, len(agents))
	for _, ag := range agents {
		next[ag.Alias] = true
	}
	d.aliasMu.Lock()
	d.aliasSet, d.aliasAt = next, time.Now()
	d.aliasMu.Unlock()
	return next, true
}

// unknownOutcome translates the two "reissue under a new key" idempotency
// results (see idemNewKeyPrefix) into something a local client can act on.
//
// They ride an HTTP 200 — correctly, since they are not same-key retryable —
// so the transport hands them back as a successful Call with a nil error, and
// forwarding them verbatim would read as an ordinary refusal ("the bus said
// no") when the truth is "the write may have committed."
//
// The daemon does NOT reissue. It cannot: whether a duplicate is harmless
// depends on the op, and for two of them it is not — a second get_inbox
// recomputes the inbox after the first already advanced the read watermark, so
// unread mail silently disappears, and a second task_claim answers a wrong
// not-claimable. Nor is "reissue under a new key" advice the local client can
// follow, since it never sees an idempotency key: the transport mints one per
// Call, so simply making the call again IS the fresh key. So the message says
// what the caller can actually act on — verify, then resend if needed — and
// the original text is kept for diagnosis.
func (d *Daemon) unknownOutcome(op string, resp proto.Response) proto.Response {
	fmt.Fprintln(os.Stderr, "muster: upstream "+op+": unknown outcome:", resp.Error)
	return proto.Response{Error: "upstream " + op +
		": outcome unknown — the write may or may not have committed; check the bus before resending (" +
		resp.Error + ")"}
}

// deviceOps are the ops whose arguments name a SESSION, and so the ops whose
// forwarded request has to carry this device's id.
//
// The membership rule is "the args carry a (socket_path, session_id) tuple, or
// register one". That pair is not device-unique in a shared roster (see
// store.API): two laptops share /private/tmp/tmux-501/default and both number
// sessions from $1. Unstamped, each of these ops would address whichever
// machine's session matched first — session_unread would count a peer's mail,
// session_aliases would advertise a peer's aliases, set_label would readdress
// a peer's agent.
//
// It is deliberately NOT the same set as badgeOps or writeOps: those classify
// what an op DOES, this classifies what its arguments MEAN.
var deviceOps = map[string]bool{
	"register_agent": true, "session_unread": true,
	"session_aliases": true, "set_label": true,
}

// needsDevice reports whether op's args name a session and so must be stamped
// with this device's id before being forwarded.
func needsDevice(op string) bool { return deviceOps[op] }

// callerDeviceOps are the ops whose arguments name the CALLER's session via
// caller_* fields (spec 2026-08-21 §3.2) rather than the deviceOps
// convention's bare device_id/session_id pair. get_inbox is the only member
// today: its caller_socket_path/caller_session_id prove which session is
// asking, and on a shared hosted store that proof is incomplete without a
// device id — the roster's rows already carry a real one (register_agent is
// in deviceOps), but neither the MCP server nor the CLI ever learns one to
// send (see stampDevice's doc comment), so an unstamped forward always
// arrives with caller_device_id="" and store.SessionAliasLineage's
// device-scoped base case then matches nothing: every get_inbox on a hosted
// bus would silently degrade to a peek, and no agent could ever clear its own
// badge across devices. This is deliberately its own set, not folded into
// deviceOps: deviceOps's args NAME a session (device_id/session_id, the pair
// RegisterAgent etc. key rows by); get_inbox's args instead PROVE a caller's
// session via a different key (caller_device_id) that would be wrong under
// stampDevice's device_id/device_name keys.
var callerDeviceOps = map[string]bool{"get_inbox": true}

// needsCallerDevice reports whether op's args carry a caller_* proof that
// must be stamped with this device's id before being forwarded.
func needsCallerDevice(op string) bool { return callerDeviceOps[op] }

// stampCallerDevice puts this daemon's device id on a forwarded caller-proof
// op's caller_device_id (see callerDeviceOps) — the same reasoning as
// stampDevice: the device id is known here and nowhere above, so this is the
// only place the hosted bus can acquire a trustworthy one, and a
// caller-supplied caller_device_id is overwritten rather than trusted (a
// client cannot claim to be on another machine). The request is copied before
// mutation, same as stampDevice.
func (d *Daemon) stampCallerDevice(req proto.Request) proto.Request {
	args := make(map[string]any, len(req.Args)+1)
	for k, v := range req.Args {
		args[k] = v
	}
	args["caller_device_id"] = d.deviceID
	req.Args = args
	return req
}

// stampDevice puts this daemon's device id on a forwarded session-scoped op
// (see deviceOps). The device id is known here and nowhere above — the MCP
// server and the CLI never learn one — so this is the only place the hosted
// bus can acquire it, and without it neither reconcile nor the poller can tell
// which agents are local. A caller-supplied device_id is overwritten rather
// than trusted: a client cannot claim to be on another machine.
//
// The request is copied before mutation, args map included: the caller's map
// came off the wire here, but forward has no license to write into a value it
// was handed.
func (d *Daemon) stampDevice(req proto.Request) proto.Request {
	args := make(map[string]any, len(req.Args)+1)
	for k, v := range req.Args {
		args[k] = v
	}
	args["device_id"] = d.deviceID
	// Stamped here for the same reason as the id: no layer above the daemon
	// knows which machine it is on. Empty is fine — the roster just shows the
	// short id instead.
	args["device_name"] = d.deviceName
	req.Args = args
	return req
}

// triggerReconcile runs ReconcileLocalSessions in the background, coalesced:
// at most one runs at a time, and concurrent triggers collapse into a single
// queued follow-up run.
//
// Coalescing is not just thriftiness with upstream calls, though a burst of
// writes fanning out into one list_agents plus one session_unread per session
// each would be that too. It is also what serializes reconciles against each
// other, so a slow one cannot write a stale unread count over a fresh one
// after the fast one already landed.
//
// The loop is owned by Close: recClosed refuses new runs and ends the current
// one, and the WaitGroup — added to under recMu, the same lock Close latches
// under, so nothing can join after Close starts waiting — is what makes
// "Close returned" mean "no goroutine is still calling upstream".
func (d *Daemon) triggerReconcile() {
	d.recMu.Lock()
	if d.recClosed {
		d.recMu.Unlock()
		return
	}
	if d.recRunning {
		d.recPending = true
		d.recMu.Unlock()
		return
	}
	d.recRunning = true
	d.recWG.Add(1)
	d.recMu.Unlock()

	go func() {
		defer d.recWG.Done()
		for {
			d.ReconcileLocalSessions()
			d.recMu.Lock()
			if !d.recPending || d.recClosed {
				d.recRunning = false
				d.recMu.Unlock()
				return
			}
			d.recPending = false
			d.recMu.Unlock()
		}
	}()
}

// ReconcileLocalSessions rewrites this device's tmux badges from upstream
// state. It is the ONE wake operation in remote mode: for each
// (socket_path, session_id) this device has live agents in, take the local
// sessionLock, fetch session_unread upstream, and write the badge.
//
// sessionLock is a local-daemon concern here rather than a server-side one,
// which is where it belonged all along: the lock protects a LOCAL resource —
// this process's tmux option writes for a tuple — and every contender for it
// runs inside this process, so a per-process lock is the whole scope. Holding
// it server-side only ever worked because server and client were the same
// process.
//
// The tuple is NOT a device-unique identity, and nothing here may assume it
// is. Two machines both running tmux as the first user share the socket path
// /private/tmp/tmux-501/default, and each numbers its own sessions from $1, so
// the same (socket_path, session_id) routinely names a different session on a
// different machine. In a shared hosted roster that makes DeviceID the only
// thing separating them: every filter that decides what this device may badge
// — which sessions to reconcile AND which aliases those sessions advertise —
// has to apply it, or one laptop writes the other laptop's aliases into its
// own @muster_agent.
//
// Two triggers drive it: inline after a local write (forward), and the poller.
// Best-effort throughout — a badge is a hint and the inbox is authoritative,
// so an upstream hiccup leaves the previous badge in place rather than
// clearing it.
//
// Exported because the poller lives outside this package.
func (d *Daemon) ReconcileLocalSessions() {
	// No notifier means no badge to write, so there is nothing to learn from
	// upstream — check this BEFORE spending a round trip. A closed daemon is
	// the same answer for a different reason: the poller lives outside this
	// package and can still hold a reference after Close.
	if d.n == nil || d.up == nil || d.reconcileStopped() {
		return
	}
	agents, err := d.upstreamAgents()
	if err != nil {
		fmt.Fprintln(os.Stderr, "muster: reconcile: list agents:", err)
		return
	}

	// Narrow the hosted roster to THIS device before anything reads it. Both
	// consumers below need the same narrowing (see the tuple-collision note on
	// this function), and doing it once here is what keeps them from drifting.
	local := make([]store.Agent, 0, len(agents))
	for _, ag := range agents {
		if ag.DeviceID == d.deviceID {
			local = append(local, ag)
		}
	}

	// Group this device's live agents by session tuple, so sibling aliases
	// sharing a session produce ONE recompute and one badge write rather than
	// one per alias. Sorted so the order is deterministic rather than map luck.
	type tuple struct {
		socketPath, sessionID string
		sessionCreated        int64
	}
	seen := map[string]bool{}
	var sessions []tuple
	for _, ag := range local {
		// A departed agent keeps its last-known tuple on the row, and an agent
		// registered outside tmux has no tuple at all; neither has a badge
		// anyone is watching.
		if ag.Departed || ag.SocketPath == "" || ag.SessionID == "" {
			continue
		}
		key := sessionKey(ag.SocketPath, ag.SessionID)
		if seen[key] {
			continue
		}
		seen[key] = true
		// The tuple's PROVEN incarnation, not this alias's. Sessions are
		// deduplicated on the bare (socket, session) key, so whichever alias
		// this loop reached first would otherwise decide it, and a legacy
		// 0-created row sharing a recycled session ID would drive the whole
		// group's recompute to zero. Same rule the local path applies in
		// notifyForThread — sessionIncarnationOf is that rule, called here
		// over `local` because remote mode's sessionIncarnation cannot read a
		// store (see its doc). Passing `local` AND d.deviceID is belt and
		// braces on purpose: the resolver now filters by device itself, so a
		// colliding tuple on a peer machine cannot contribute its unrelated
		// creation time even if a future caller hands it `agents`.
		sessions = append(sessions, tuple{ag.SocketPath, ag.SessionID,
			sessionIncarnationOf(local, d.deviceID, ag.SocketPath, ag.SessionID, ag.SessionCreated)})
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].socketPath != sessions[j].socketPath {
			return sessions[i].socketPath < sessions[j].socketPath
		}
		return sessions[i].sessionID < sessions[j].sessionID
	})

	for _, s := range sessions {
		if _, err := d.setSessionBadge(s.socketPath, s.sessionID, s.sessionCreated); err != nil {
			fmt.Fprintln(os.Stderr, "muster: reconcile:", s.sessionID, err)
		}
		// The agent badge comes free: the roster is already in hand, so this
		// costs no extra round trip. Without it a remote-mode device would
		// never get the operator's ambient "registered as X" indicator, which
		// in local mode rides on register/deregister's reconcileBadge.
		//
		// It reads `local`, not `agents`: liveAliasesFor matches on the tuple
		// alone — correct for the local store, where every row is this device's
		// — so handing it the whole hosted roster would advertise another
		// machine's aliases on any tuple the two happen to share.
		d.pushAgentBadge(s.socketPath, s.sessionID, liveAliasesFor(local, s.socketPath, s.sessionID, d.deviceName))
	}
}

// noteRosterChange keeps the device-local roster in step with the roster ops
// that just went upstream successfully. It is called from forward, which is
// the ONLY way an agent on this device can register: the MCP server and the
// CLI are peer clients of this socket, so nothing reaches the hosted roster
// from this machine without passing through here.
//
// What it feeds is the poller's "is there anybody here to wake" check. Keeping
// it locally rather than asking upstream is what makes an idle device free —
// a device with no agents makes no calls at all, rather than one list_agents
// per tick for ever.
//
// Agents with no tmux tuple are not recorded: they can carry no badge, so they
// are not a reason to poll.
func (d *Daemon) noteRosterChange(req proto.Request) {
	alias := str(req.Args, "alias")
	if alias == "" {
		return
	}
	// Every alias this op can mint or remove is exactly the set
	// expandAliasArg's cache exists to check against, so any successful
	// roster op invalidates it here — before the op-specific branches below,
	// which additionally require a tmux tuple and so would miss a
	// tuple-less registration (still a real, expandable alias upstream).
	d.invalidateAliasCache()
	switch req.Op {
	case "register_agent":
		socketPath, sessionID := str(req.Args, "socket_path"), str(req.Args, "session_id")
		if socketPath == "" || sessionID == "" {
			return
		}
		d.localMu.Lock()
		defer d.localMu.Unlock()
		if d.localAgents == nil {
			d.localAgents = map[string]store.SessionRef{}
		}
		d.localAgents[alias] = store.SessionRef{SocketPath: socketPath, SessionID: sessionID}
	case "deregister_agent", "purge_agent":
		d.localMu.Lock()
		defer d.localMu.Unlock()
		delete(d.localAgents, alias)
	}
}

// invalidateAliasCache drops expandAliasArg's cached roster so the next
// expansion re-fetches from upstream rather than trusting a snapshot that
// predates a registration change this daemon just forwarded. Called from
// noteRosterChange, which forward invokes synchronously and without holding
// aliasMu (expandAliasArg, the cache's only other user, always releases
// aliasMu before forward's upstream call happens) — so this cannot deadlock
// against the refresh path in aliasesForExpansion, and the two never nest.
//
// It clears the field rather than deleting keys from the existing map,
// matching aliasesForExpansion's contract: a published map is never mutated
// in place (a concurrent reader could be mid-range over it), only replaced
// or, here, cleared to nil so the next read is forced through a real
// refresh.
func (d *Daemon) invalidateAliasCache() {
	d.aliasMu.Lock()
	d.aliasSet, d.aliasAt = nil, time.Time{}
	d.aliasMu.Unlock()
}

// hasLocalAgents reports whether this device has an agent whose pane could be
// woken, seeding from the hosted roster ONCE if it has never seen one.
//
// The seed exists for the restart case: a daemon that came up under
// already-registered agents has an empty local roster and would otherwise
// never poll, so cross-device mail would stop arriving silently until each
// agent happened to re-register. It costs exactly one list_agents over the
// daemon's lifetime, and only while the local roster is empty.
func (d *Daemon) hasLocalAgents() bool {
	d.localMu.Lock()
	n, seeded := len(d.localAgents), d.localSeeded
	d.localMu.Unlock()
	if n > 0 {
		return true
	}
	if seeded {
		return false
	}

	agents, err := d.upstreamAgents()
	d.localMu.Lock()
	defer d.localMu.Unlock()
	if err != nil {
		// Not marked seeded: an upstream hiccup must not permanently decide
		// this device has nobody on it.
		fmt.Fprintln(os.Stderr, "muster: poll: seed local roster:", err)
		return false
	}
	d.localSeeded = true
	if d.localAgents == nil {
		d.localAgents = map[string]store.SessionRef{}
	}
	for _, ag := range agents {
		if ag.DeviceID != d.deviceID || ag.Departed || ag.SocketPath == "" || ag.SessionID == "" {
			continue
		}
		d.localAgents[ag.Alias] = store.SessionRef{SocketPath: ag.SocketPath, SessionID: ag.SessionID}
	}
	return len(d.localAgents) > 0
}

// reconcileSessions rewrites the badges of exactly the sessions named — the
// poller's counterpart to ReconcileLocalSessions, which discovers its own list
// from the hosted roster.
//
// It takes the server's answer as the list rather than re-deriving one:
// device_poll already filtered by device id, and asking the roster again would
// buy a round trip to reproduce a decision the server just made from fresher
// state than this device has.
//
// The agent badge is deliberately not touched. It advertises WHO is registered
// here, which only an identity change can move.
//
// It DOES read the roster once — not for the agent badge, but because a badge
// write must name an incarnation (spec §5.1) and device_poll answers with
// tuples only. Remote mode's sessionIncarnation cannot resolve one for itself
// (no local store, and the hosted roster is the whole bus's), so the
// resolution has to happen here, over a roster narrowed to this device. Once
// per batch, not once per session: a device with five lit sessions still costs
// one list_agents.
//
// A roster read FAILURE skips the whole batch rather than badging with a zero
// incarnation. Zero seeds nothing, so it would not recompute the badge — it
// would clear it, telling every named session it is caught up on the one tick
// the server just said it has mail. Leaving the previous badge in place is the
// safe direction, and the next tick retries (the watermark has advanced, but
// device_poll re-reports any session with mail still unread).
func (d *Daemon) reconcileSessions(sessions []store.SessionRef) {
	if d.n == nil || d.up == nil || len(sessions) == 0 {
		return
	}
	local, err := d.deviceRoster()
	if err != nil {
		fmt.Fprintln(os.Stderr, "muster: poll reconcile: roster:", err)
		return
	}
	for _, s := range sessions {
		if d.reconcileStopped() {
			return
		}
		if s.SocketPath == "" || s.SessionID == "" {
			continue // no badge anyone is watching
		}
		// fallback 0: an unprovable tuple (no live row carries a non-zero
		// created) genuinely has no incarnation to badge, and 0 correctly
		// resolves to an empty answer rather than another incarnation's mail.
		created := sessionIncarnationOf(local, d.deviceID, s.SocketPath, s.SessionID, 0)
		if _, err := d.setSessionBadge(s.SocketPath, s.SessionID, created); err != nil {
			fmt.Fprintln(os.Stderr, "muster: poll reconcile:", s.SessionID, err)
		}
	}
}

// deviceRoster is the hosted roster narrowed to THIS device — the only roster
// an incarnation may be resolved against in remote mode. The narrowing is the
// point: (socket, session) collides across machines, so a peer's row on the
// same tuple would otherwise contribute its own unrelated creation time and
// win the "highest non-zero created" rule outright.
func (d *Daemon) deviceRoster() ([]store.Agent, error) {
	agents, err := d.upstreamAgents()
	if err != nil {
		return nil, err
	}
	local := make([]store.Agent, 0, len(agents))
	for _, ag := range agents {
		if ag.DeviceID == d.deviceID {
			local = append(local, ag)
		}
	}
	return local, nil
}

// sessionUnread returns a session's total unread from whichever backend this
// daemon fronts. It is the seam that lets setSessionBadge stay the ONE
// {recompute, push} sequence in both modes rather than growing a remote twin.
//
// sessionCreated is the incarnation the badge is being written for, already
// resolved by setSessionBadge (see daemon.sessionIncarnation). It is sent
// upstream as an ARGUMENT rather than left to the server: the hosted
// session_unread op deliberately does not resolve (see store.API), so a
// remote-mode badge that omitted it would ask for incarnation 0 — which seeds
// nothing — and get an authoritative zero back for a session with mail.
func (d *Daemon) sessionUnread(socketPath, sessionID string, sessionCreated int64) (int, error) {
	if d.up == nil {
		total, _, err := d.s.SessionUnread(d.deviceID, socketPath, sessionID, sessionCreated)
		return total, err
	}
	resp, err := d.callUpstream(proto.Request{Op: "session_unread", Args: map[string]any{
		"device_id": d.deviceID, "socket_path": socketPath, "session_id": sessionID,
		"session_created": sessionCreated,
	}})
	if err != nil {
		return 0, err
	}
	var out struct {
		Total int `json:"total"`
	}
	if err := decodeData(resp.Data, &out); err != nil {
		return 0, fmt.Errorf("session_unread: %w", err)
	}
	return out.Total, nil
}

// upstreamAgents is the hosted roster — remote mode's answer to
// store.ListAgents.
func (d *Daemon) upstreamAgents() ([]store.Agent, error) {
	resp, err := d.callUpstream(proto.Request{Op: "list_agents"})
	if err != nil {
		return nil, err
	}
	var agents []store.Agent
	if err := decodeData(resp.Data, &agents); err != nil {
		return nil, fmt.Errorf("list_agents: %w", err)
	}
	return agents, nil
}

// callUpstream makes one upstream call for the daemon's OWN account (not a
// forwarded client request) and collapses both failure channels into an error,
// since these callers have no client to hand a proto.Response to.
//
// It passes context.Background(): the transport owns its own deadlines (a
// bounded per-attempt timeout times a bounded attempt count), and a second
// timeout here would either duplicate that or silently truncate its retry
// ladder.
func (d *Daemon) callUpstream(req proto.Request) (proto.Response, error) {
	resp, err := d.up.Call(context.Background(), req)
	if err != nil {
		return proto.Response{}, err
	}
	if !resp.OK {
		return proto.Response{}, errors.New(resp.Error)
	}
	return resp, nil
}

// decodeData re-marshals a Response.Data into a typed value. Data crosses the
// wire as generic JSON, so it arrives as map[string]any/[]any rather than the
// struct the server encoded; a round trip through JSON is the same approach
// every other typed reader of a daemon response uses.
func decodeData(data any, out any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
