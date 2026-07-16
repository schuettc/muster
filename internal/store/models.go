package store

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
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
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

// Event is one bus observability record: a mailbox notify outcome or an
// inbox read. The daemon appends these so "who was lit when, and when did it
// clear" is answerable after the fact instead of reconstructed from thread
// timestamps.
type Event struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"` // 'notify' | 'read'
	Agent    string `json:"agent"`
	ThreadID int64  `json:"thread_id"` // 0 = no thread
	Count    int    `json:"count"`
	Detail   string `json:"detail"` // 'lit' | 'cleared' | 'skipped: …' | 'error: …'
}

// KVPair is a shared blackboard fact.
type KVPair struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedBy string `json:"updated_by"`
	UpdatedAt int64  `json:"updated_at"`
}
