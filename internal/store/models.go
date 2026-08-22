package store

// Intent vocabulary for threads. "" (unspecified) is also valid — CreateThread
// accepts it and effectiveIntent (threads.go) derives the operative value.
const (
	IntentFYI    = "fyi"
	IntentReply  = "reply-requested"
	IntentAction = "action-requested"
)

// Agent is a registered participant on the bus.
type Agent struct {
	Alias       string `json:"alias"`
	Role        string `json:"role"`
	ModelType   string `json:"model_type"`
	SocketPath  string `json:"socket_path"`
	PaneID      string `json:"pane_id"`
	SessionName string `json:"session_name"`
	SessionID   string `json:"session_id"`
	// SessionCreated is the tmux session's creation time (#{session_created},
	// unix seconds) captured at register time — the incarnation half of the
	// identity tuple. tmux recycles session IDs from $0 across server
	// restarts, so (SocketPath, SessionID) alone cannot distinguish a
	// registration from a dead server incarnation from one on today's session
	// that reused its ID; creation time is immutable per session (unlike its
	// name) so a mismatch proves the recorded session is gone. 0 = unknown
	// (registered outside tmux, or before this column existed) — liveness
	// then falls back to bare session existence. See tmuxenv.IsSessionAlive
	// and Store.DepartStaleSiblings.
	SessionCreated int64 `json:"session_created"`
	// DeviceID identifies the machine this agent registered from — the
	// wake-routing key once a bus spans devices. SocketPath cannot serve
	// this purpose (two machines can both have /tmp/tmux-501/default).
	// '' = unknown (registered before this column existed).
	DeviceID string `json:"device_id"`
	// DeviceName is the human-meaningful name of that same machine
	// ("work-laptop"), captured at registration. It exists because DeviceID
	// is a UUID and nobody says a UUID out loud: an operator asks for "the
	// ci-cd session on my work laptop", and a model matching that phrase
	// against the roster needs a string shaped like the phrase.
	//
	// It is DISPLAY, never identity. Nothing is keyed, scoped, or compared by
	// it — DeviceID does all of that — which is precisely what lets an
	// operator rename a machine without re-keying a single row or orphaning
	// any mail. '' = unknown (pre-upgrade, or a machine with no hostname).
	DeviceName string `json:"device_name"`
	// HarnessSessionID is the agent-harness session UUID (e.g. Claude Code's
	// session id) when known — the deterministic link between a roster row
	// and the harness session it belongs to. The pane-side launch handshake
	// (`muster register <name> --harness-session <uuid>` before
	// `claude --session-id <uuid>`) sets it on tmux-anchored rows so the
	// session's own hooks — which run with no tmux in their environment on
	// daemon-hosted harnesses — can still find their rows; paneless
	// registrations carry it too. "" = unknown (pre-handshake rows).
	HarnessSessionID string `json:"harness_session_id"`
	// TranscriptPath is the harness conversation's transcript file — the
	// strongest identity key (spec 2026-08-21 §2): Claude Code never changes
	// it for a conversation, while the harness session ID can change under
	// /login. '' when the harness provides none (Codex, paneless).
	TranscriptPath string `json:"transcript_path"`
	Project        string `json:"project"`
	Label          string `json:"label"`
	LabelManual    bool   `json:"label_manual"`
	RegisteredAt   int64  `json:"registered_at"`
	LastSeen       int64  `json:"last_seen"`
	// LastReadEntryID is the entry-ID read watermark (see MarkRead/UnreadCount
	// in agents.go): the highest entries.id visible the last time this
	// agent's inbox was read. Supersedes the wall-clock last_read_at for
	// unread math; last_read_at is retained internally for display only.
	LastReadEntryID int64 `json:"last_read_entry_id"`
	// Departed is true once this agent has been deregistered (see
	// Store.DepartAgent) — a tombstone, not a delete: identity, project,
	// label, and read-state (LastReadEntryID/last_read_at) all survive.
	// RegisterAgent's upsert always resets this to false, so a returning
	// session revives the row cleanly rather than needing a fresh one.
	// Addressing/resolution semantics are UNCHANGED for a departed alias — it
	// remains addressable exactly like a tmux-dead agent (mail waits; they
	// may return); only notifyForThread and station's roster rendering treat
	// Departed specially. muster gc's default reap no longer deletes any
	// row — it sets Departed instead; `gc --purge-agents` hard-deletes
	// departed/dead rows the old way.
	Departed bool `json:"departed"`
	// SupersededBy is non-empty on a departed row that was claimed away via
	// Become: it names the alias that now carries this identity forward.
	// RegisterAgent's upsert always resets it to "" (a revived/re-registered
	// alias is no longer superseded — e.g. the operator purged the successor
	// and re-registered the old name), and Become's clone does NOT copy it
	// onto the new alias (the successor starts unsuperseded). Resume reclaim
	// (hookSessionStartResume) uses this as ground truth to skip resurrecting a
	// retired seed, rather than inferring it from tuple coincidence.
	SupersededBy string `json:"superseded_by"`
}

// Thread is a conversation: a message (no status) or a task (status set).
type Thread struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	FromAgent string `json:"from_agent"`
	ToKind    string `json:"to_kind"`
	ToTarget  string `json:"to_target"`
	Subject   string `json:"subject"`
	Ref       string `json:"ref"`
	Status    string `json:"status"` // "" means NULL (message)
	// Intent is validated by CreateThread against the raw stored vocabulary
	// (""/fyi/reply-requested/action-requested), but every READ surface —
	// Threads, GetThread, Inbox — returns the EFFECTIVE intent (see
	// effectiveIntent in threads.go), never the raw stored value: one
	// vocabulary everywhere a Thread is read, so an old task row (stored
	// intent "") reads as action-requested consistently across all three.
	Intent    string `json:"intent"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// OriginProject is the SENDER's registered project at thread-creation
	// time (iteration-4 orphan-thread fix): the daemon resolves the sender's
	// agent record when it calls CreateThread and stamps its Project here —
	// "" when the sender was unregistered at creation time. Additive and
	// backfilled best-effort for pre-existing rows (see store.migrate); it
	// exists so a thread survives every participant later deregistering —
	// the roster-only project mapping that made ghost-site's threads vanish
	// (spec iteration-4 queue item 4) has this as its durable fallback.
	OriginProject string `json:"origin_project"`
	// LastFrom, LastAt, and EntryCount are query-time only, populated by
	// Threads() and Inbox() from the thread's last entry (by MAX(id), never
	// MAX(created_at) — same-millisecond entries must not tie-break on
	// timestamp) and its total entry count. GetThread/CreateThread leave
	// them zero.
	LastFrom   string `json:"last_from"`
	LastAt     int64  `json:"last_at"`
	EntryCount int    `json:"entry_count"`
	// Unread is query-time only, populated by Inbox(alias): the count of
	// this thread's entries after alias's last_read_entry_id watermark that
	// were NOT written by alias (the same predicate as UnreadCount, scoped
	// to one thread). It answers "for the alias Inbox was called with," not
	// a thread-global property — the defect this fixes was an agent unable
	// to tell "a peer replied on my thread" from "my own last send" without
	// drilling into get_thread. Threads()/GetThread/CreateThread leave it
	// zero.
	Unread int `json:"unread"`
}

// Entry is one append-only message within a thread.
type Entry struct {
	ID           int64  `json:"id"`
	ThreadID     int64  `json:"thread_id"`
	FromAgent    string `json:"from_agent"`
	Body         string `json:"body"`
	StatusChange string `json:"status_change"` // "" means none
	CreatedAt    int64  `json:"created_at"`
}

// Event is one bus journal record: a bus action (send, task, reply, claim,
// transition, nudge) or a wake-layer outcome (mailbox notify, inbox read).
// The daemon appends these so "who did what, and who was lit when" is
// answerable after the fact instead of reconstructed from thread timestamps.
type Event struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"` // 'send' | 'task' | 'reply' | 'claim' | 'transition' | 'nudge' | 'notify' | 'read'
	Agent    string `json:"agent"`
	Target   string `json:"target"`    // 'agent:x' / 'role:r' / 'broadcast' / bare alias (nudge)
	ThreadID int64  `json:"thread_id"` // 0 = no thread
	Count    int    `json:"count"`
	Detail   string `json:"detail"` // 'lit' | 'cleared' | 'skipped: …' | 'error: …'
	// Subject is joined from the event's thread at query time (empty for
	// thread-less events). Never stored on the row.
	Subject string `json:"subject"`
	// Intent is the event's thread's EFFECTIVE intent (effectiveIntent in
	// threads.go), joined at query time exactly like Subject (empty for
	// thread-less events). Never stored on the row.
	Intent string `json:"intent"`
}

// KVPair is a shared blackboard fact.
type KVPair struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedBy string `json:"updated_by"`
	UpdatedAt int64  `json:"updated_at"`
}
