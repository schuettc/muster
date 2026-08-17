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
//
// # …and one incarnation of it: sessionCreated
//
// The same four methods take a sessionCreated. tmux RECYCLES session IDs, so
// (device, socket, session) still names a sequence of unrelated sessions over
// a machine's lifetime; session_created — the tmux server's creation
// timestamp, immutable for a session's life — picks the one that is running
// now. A caller vouches for it by having just captured it from the pane it is
// acting on. Zero is not a value, it is the ABSENCE of proof (a pre-v0.8.0
// row, or a registrant outside tmux), and it therefore matches NOTHING:
// attribution requires proof (spec §5.1, 2026-08-05).
//
// # Two dimensions, one rule
//
// The two arrived independently — deviceID from the hosted backend, and
// sessionCreated from upstream's conversation-as-identity work — and converged
// on the SAME placement, which is the fact worth recording once here rather
// than re-deriving at each call site:
//
//	BOTH scope the BASE CASE of the supersession walk. NEITHER scopes the
//	recursive step.
//
// The base case is a TUPLE match, and a tuple is a COORDINATE: it names a
// place (which machine, which tmux session) at a time (which incarnation).
// Coordinates collide — the same string names different things on two laptops,
// and the same session ID names different sessions before and after a tmux
// restart — so a base case that does not carry both dimensions will seed the
// walk from a row that is not this session's.
//
// The recursive step is not a tuple match. It follows superseded_by, which
// holds an ALIAS — the global primary key of the roster. A primary key names
// exactly one row everywhere and forever, so there is no coincidence to defend
// against: filtering it by device or by incarnation cannot remove a false
// positive (there are none), it can only drop TRUE ones. And dropping them
// breaks precisely the cases the two features exist for — a name claimed on
// one machine and resumed on another, and a name claimed before a tmux restart
// and still receiving mail after it. Retired seeds sit on old tuples, on old
// incarnations, on other machines, forever.
//
// Said in one line: LINEAGE IS IDENTITY, and identity crosses both machines
// and restarts; THE TUPLE IS LOCATION, and location does not.
//
// One asymmetry between the dimensions: an EMPTY socketPath (the paneless
// harness tuple) is exempt from the incarnation check, because a harness UUID
// is never recycled and so needs no incarnation to disambiguate it. It is NOT
// exempt from the device check — a paneless registration still belongs to
// exactly one machine.
//
// # These methods do not resolve; callers prove
//
// None of the four resolves an incarnation on the caller's behalf: pass a
// created that no row matches and you get an empty answer, not an error. That
// is deliberate — a caller answers for its own incarnation and must prove it,
// and a query that "helpfully" resolved would hand a caller which proved
// nothing another incarnation's mail. The daemon's badge path is the one place
// resolution happens (daemon.sessionIncarnation at the setSessionBadge seam),
// because there the DAEMON, not a caller, is deciding which incarnation a
// (socket, session)-keyed tmux write belongs to.
//
// The cost of that choice, and the reason it is stated here: a caller that
// OMITS sessionCreated gets a plausible zero rather than a complaint. Tests
// against these methods must pass it explicitly or they prove nothing.
type API interface {
	RegisterAgent(Agent) error
	ListAgents() ([]Agent, error)
	GetAgent(alias string) (Agent, bool, error)
	DepartAgent(alias string) error
	DepartStaleSiblings(deviceID, socketPath, sessionID string, created int64, keepAlias string) ([]string, error)
	SetSessionLabel(deviceID, socketPath, sessionID string, sessionCreated int64, label string, manual bool) (int64, error)
	DeleteAgent(alias string) error
	CreateThread(t Thread, firstBody string) (int64, error)
	// SetStandingOrder create-or-replaces the standing order identified by
	// (project, key) idempotently; RetractStandingOrder retracts it
	// (idempotent, reports whether a row changed); ListStandingOrders returns
	// the live keyed orders for a project. See the standing-orders spec.
	SetStandingOrder(project, key, from, body string) (int64, error)
	RetractStandingOrder(project, key string) (bool, error)
	ListStandingOrders(project string) ([]StandingOrder, error)
	AppendEntry(threadID int64, fromAgent, body, statusChange string) (int64, error)
	ClaimTask(threadID int64, byAgent string) error
	TransitionTask(threadID int64, byAgent, newStatus, note string) error
	GetThread(id int64) (Thread, []Entry, error)
	Threads(limit int) ([]Thread, error)
	Inbox(alias string) ([]Thread, error)
	// MarkRead records that alias has read an Inbox snapshot through
	// upToEntryID. Callers must derive the bound from that snapshot, never
	// from a later query, so an entry committed in between remains unread.
	MarkRead(alias string, upToEntryID int64) error
	UnreadCount(alias string) (int, error)
	// StatusCounts returns every alias's side-effect-free (unread,
	// action_required) counts — a pure read for a polling picker; see the
	// method comment on Store.
	StatusCounts() ([]AliasStatus, error)
	SessionUnread(deviceID, socketPath, sessionID string, sessionCreated int64) (total, action int, err error)
	SessionAliasLineage(deviceID, socketPath, sessionID string, sessionCreated int64) ([]string, error)
	// FindConversation answers "which live row IS this conversation" (spec
	// 2026-08-21 §2): by transcript path first when the caller has one
	// (transcriptPath != ""), else by the full live pane tuple
	// (deviceID, socketPath, sessionID, sessionCreated, paneID). Harness
	// session ID is deliberately not a lookup key — it can change under
	// /login, which is the defect this op exists to close. A zero
	// sessionCreated or empty paneID never pane-matches (absence of proof,
	// mirroring every other tuple surface on this interface), and only live
	// (non-departed) rows are ever returned.
	FindConversation(deviceID, transcriptPath, socketPath, sessionID string, sessionCreated int64, paneID string) (Agent, bool, error)
	StampHarness(alias, harnessSessionID, transcriptPath string) error
	// Become claims a new name for an existing identity: it clones from onto
	// to and retires from, stamping from.superseded_by = to. It is a
	// compare-and-swap, not a read-then-write — the to-must-not-exist guard
	// and the clone are one atomic step, so two sessions racing for the same
	// name cannot both win. Backends must return ErrBecomeToExists /
	// ErrBecomeFromMissing for the two guard failures; the daemon maps them to
	// hint-carrying wire errors.
	Become(from, to string) error
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
