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
//
// # A session is (device, socket_path, session_id) — never the pair alone
//
// Four methods take a session tuple, and every one of them takes a deviceID
// first. The pair on its own is NOT unique in a store shared by more than one
// machine: two macOS laptops both run tmux on /private/tmp/tmux-501/default
// (501 is the default first-user uid) and each numbers its own sessions from
// $1, so the identical pair routinely names a different session per machine.
// A local SQLite store never notices — every row in it is the one device's —
// which is exactly why the collision is invisible until the hosted backend,
// where getting it wrong silently swallows another device's mail
// (SessionUnread), relabels a peer's agent (SetSessionLabel), or tombstones a
// live one (DepartStaleSiblings).
//
// Local mode passes "" and every local row carries "", so the pair still
// behaves as it always did. The device id is matched literally, never as a
// wildcard: a caller that cannot name its device must not be handed another
// device's sessions.
type API interface {
	RegisterAgent(Agent) error
	ListAgents() ([]Agent, error)
	GetAgent(alias string) (Agent, bool, error)
	DepartAgent(alias string) error
	DepartStaleSiblings(deviceID, socketPath, sessionID string, created int64, keepAlias string) ([]string, error)
	SetSessionLabel(deviceID, socketPath, sessionID, label string, manual bool) (int64, error)
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
	SessionUnread(deviceID, socketPath, sessionID string) (total, action int, err error)
	DevicePoll(deviceID string, sinceEntryID int64) (DevicePollResult, error)
	KVSet(key, value, updatedBy string) error
	KVGet(key string) (KVPair, bool, error)
	AppendEvent(e Event) error
	Events(q EventQuery) ([]Event, error)
	MaxEventID() (int64, error)
	PruneEvents(olderThanMillis int64) (int64, error)
	// IdemBegin claims key for a first delivery. found=false means this caller
	// owns execution and must call IdemComplete. found=true with done=true
	// means the op already ran and resp is its recorded response. found=true
	// with done=false means an identical request is in flight.
	IdemBegin(key string) (resp []byte, done bool, found bool, err error)
	IdemComplete(key string, resp []byte) error
}
