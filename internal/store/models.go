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
	Alias        string `json:"alias"`
	Role         string `json:"role"`
	ModelType    string `json:"model_type"`
	SocketPath   string `json:"socket_path"`
	PaneID       string `json:"pane_id"`
	SessionName  string `json:"session_name"`
	SessionID    string `json:"session_id"`
	Project      string `json:"project"`
	Label        string `json:"label"`
	LabelManual  bool   `json:"label_manual"`
	RegisteredAt int64  `json:"registered_at"`
	LastSeen     int64  `json:"last_seen"`
	// LastReadEntryID is the entry-ID read watermark (see MarkRead/UnreadCount
	// in agents.go): the highest entries.id visible the last time this
	// agent's inbox was read. Supersedes the wall-clock last_read_at for
	// unread math; last_read_at is retained internally for display only.
	LastReadEntryID int64 `json:"last_read_entry_id"`
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
