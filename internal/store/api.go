package store

// API is the store surface the daemon depends on. It lives here rather than in
// daemon so that alternate backends (see internal/dynamostore) can satisfy it
// without importing daemon purely to name the interface. *Store satisfies this
// interface as-is.
//
// UnreadCount is included even though the daemon reaches it only indirectly
// (via SessionUnread): the backend conformance suite asserts on it directly,
// and a backend that got it wrong while getting SessionUnread right would be a
// latent bug nothing else catches.
type API interface {
	RegisterAgent(Agent) error
	ListAgents() ([]Agent, error)
	GetAgent(alias string) (Agent, bool, error)
	DepartAgent(alias string) error
	DepartStaleSiblings(socketPath, sessionID string, created int64, keepAlias string) ([]string, error)
	SetSessionLabel(socketPath, sessionID, label string, manual bool) (int64, error)
	DeleteAgent(alias string) error
	CreateThread(t Thread, firstBody string) (int64, error)
	AppendEntry(threadID int64, fromAgent, body, statusChange string) (int64, error)
	ClaimTask(threadID int64, byAgent string) error
	TransitionTask(threadID int64, byAgent, newStatus, note string) error
	GetThread(id int64) (Thread, []Entry, error)
	Threads(limit int) ([]Thread, error)
	Inbox(alias string) ([]Thread, error)
	MarkRead(alias string) error
	UnreadCount(alias string) (int, error)
	SessionUnread(socketPath, sessionID string) (total, action int, err error)
	KVSet(key, value, updatedBy string) error
	KVGet(key string) (KVPair, bool, error)
	AppendEvent(e Event) error
	Events(q EventQuery) ([]Event, error)
	MaxEventID() (int64, error)
	PruneEvents(olderThanMillis int64) (int64, error)
}
