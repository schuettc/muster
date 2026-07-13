package store

// Agent is a registered participant on the bus.
type Agent struct {
	Alias        string
	Role         string
	ModelType    string
	SocketPath   string
	PaneID       string
	SessionName  string
	RegisteredAt int64
	LastSeen     int64
}

// Thread is a conversation: a message (no status) or a task (status set).
type Thread struct {
	ID        int64
	Kind      string
	FromAgent string
	ToKind    string
	ToTarget  string
	Subject   string
	Ref       string
	Status    string // "" means NULL (message)
	CreatedAt int64
	UpdatedAt int64
}

// Entry is one append-only message within a thread.
type Entry struct {
	ID           int64
	ThreadID     int64
	FromAgent    string
	Body         string
	StatusChange string // "" means none
	CreatedAt    int64
}

// KVPair is a shared blackboard fact.
type KVPair struct {
	Key       string
	Value     string
	UpdatedBy string
	UpdatedAt int64
}
