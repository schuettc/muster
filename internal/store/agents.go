package store

import (
	"database/sql"
	"errors"

	"github.com/schuettc/muster/internal/clock"
)

// RegisterAgent upserts by Alias: inserts on first sight (stamping RegisteredAt),
// and on conflict refreshes the tuple + LastSeen while preserving RegisteredAt.
// departed is always reset to 0 by both the insert and the conflict update, so
// re-registering a previously-departed alias (a returning session) revives it
// cleanly — read-state (last_read_entry_id/last_read_at) is untouched by
// either branch, so it survives the roundtrip intact. superseded_by is left
// alone on conflict (only the INSERT branch defaults it to ”): a re-register
// of a claimed-away alias — the seed's session resuming on its old tuple — is
// exactly the case become-lineage exists to route mail through, not a signal
// that the claim never happened, so re-registering must not forget the
// successor. See Store.Become and SessionAliasLineage.
func (s *Store) RegisterAgent(a Agent) error {
	now := clock.NowMillis()
	_, err := s.db.Exec(`
INSERT INTO agents (alias, role, model_type, socket_path, pane_id, session_name, session_id, session_created, device_id, device_name, harness_session_id, transcript_path, project, label, label_manual, departed, superseded_by, registered_at, last_seen, last_read_entry_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?, (SELECT COALESCE(MAX(id), 0) FROM entries))
ON CONFLICT(alias) DO UPDATE SET
    role=excluded.role,
    model_type=excluded.model_type,
    socket_path=excluded.socket_path,
    pane_id=excluded.pane_id,
    session_name=excluded.session_name,
    session_id=excluded.session_id,
    session_created=excluded.session_created,
    device_id=excluded.device_id,
    device_name=excluded.device_name,
    harness_session_id=excluded.harness_session_id,
    transcript_path=excluded.transcript_path,
    project=excluded.project,
    label=excluded.label,
    label_manual=excluded.label_manual,
    departed=0,
    last_seen=excluded.last_seen`,
		a.Alias, a.Role, a.ModelType, a.SocketPath, a.PaneID, a.SessionName, a.SessionID, a.SessionCreated, a.DeviceID,
		a.DeviceName, a.HarnessSessionID, a.TranscriptPath, a.Project, a.Label, a.LabelManual, now, now)
	return err
}

// ListAgents returns all agents ordered by alias — departed (tombstoned)
// agents included: their rows are history, not gone (see DepartAgent).
func (s *Store) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(`
SELECT alias, role, model_type, socket_path, pane_id, session_name, session_id, session_created, device_id, device_name, harness_session_id, transcript_path, project, label, label_manual, registered_at, last_seen, last_read_entry_id, last_read_standing_entry_id, departed, superseded_by
FROM agents ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.SessionID, &a.SessionCreated, &a.DeviceID, &a.DeviceName, &a.HarnessSessionID, &a.TranscriptPath, &a.Project, &a.Label, &a.LabelManual, &a.RegisteredAt, &a.LastSeen, &a.LastReadEntryID, &a.LastReadStandingEntryID, &a.Departed, &a.SupersededBy); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAgent looks up a single agent by alias. ok is false if no such agent is
// registered at all — a departed (tombstoned) agent still reports ok=true,
// with Departed set (see DepartAgent).
func (s *Store) GetAgent(alias string) (Agent, bool, error) {
	return s.scanOneAgent(`WHERE alias=?`, alias)
}

// agentColumns is the exact column list every single-row agent scan uses —
// GetAgent and FindConversation alike — so the two can never drift apart.
const agentColumns = `alias, role, model_type, socket_path, pane_id, session_name, session_id, session_created, device_id, device_name, harness_session_id, transcript_path, project, label, label_manual, registered_at, last_seen, last_read_entry_id, last_read_standing_entry_id, departed, superseded_by`

// scanOneAgent runs a `SELECT <agentColumns> FROM agents ` + where query with
// args and scans the single resulting row. ok is false (not an error) when
// the where clause matches no row — mirroring GetAgent's own contract, since
// GetAgent is now just this with `WHERE alias=?`.
func (s *Store) scanOneAgent(where string, args ...any) (Agent, bool, error) {
	var a Agent
	err := s.db.QueryRow(`SELECT `+agentColumns+` FROM agents `+where, args...).
		Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.SessionID, &a.SessionCreated, &a.DeviceID, &a.DeviceName, &a.HarnessSessionID, &a.TranscriptPath, &a.Project, &a.Label, &a.LabelManual, &a.RegisteredAt, &a.LastSeen, &a.LastReadEntryID, &a.LastReadStandingEntryID, &a.Departed, &a.SupersededBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, false, nil
	}
	if err != nil {
		return Agent{}, false, err
	}
	return a, true, nil
}

// FindConversation answers "which live row IS this conversation" (spec
// 2026-08-21 §2): by transcript path when the caller has one, else by the
// full live pane tuple. Harness session ID is deliberately not a key here —
// it can change under /login, which is the defect this op exists to close.
// A zero sessionCreated or empty paneID never pane-matches (absence of
// proof), mirroring every other tuple surface.
func (s *Store) FindConversation(deviceID, transcriptPath, socketPath, sessionID string, sessionCreated int64, paneID string) (Agent, bool, error) {
	if transcriptPath != "" {
		a, ok, err := s.scanOneAgent(`WHERE device_id=? AND transcript_path=? AND departed=0 ORDER BY last_seen DESC LIMIT 1`, deviceID, transcriptPath)
		if err != nil || ok {
			return a, ok, err
		}
	}
	if socketPath == "" || sessionID == "" || sessionCreated == 0 || paneID == "" {
		return Agent{}, false, nil
	}
	return s.scanOneAgent(`WHERE device_id=? AND socket_path=? AND session_id=? AND session_created=? AND pane_id=? AND departed=0 ORDER BY last_seen DESC LIMIT 1`,
		deviceID, socketPath, sessionID, sessionCreated, paneID)
}

// TouchAgent bumps last_seen. No error if the agent is unknown.
func (s *Store) TouchAgent(alias string) error {
	_, err := s.db.Exec(`UPDATE agents SET last_seen=? WHERE alias=?`, clock.NowMillis(), alias)
	return err
}

// StampHarness attaches the harness link to an existing row: the session
// UUID and the transcript path, each only when non-empty — a hook that
// knows one but not the other must not erase what another hook stamped.
// Identity, tuple, and read-state are untouched; unknown alias is a no-op.
func (s *Store) StampHarness(alias, harnessSessionID, transcriptPath string) error {
	_, err := s.db.Exec(`UPDATE agents SET
    harness_session_id = CASE WHEN ?2 = '' THEN harness_session_id ELSE ?2 END,
    transcript_path    = CASE WHEN ?3 = '' THEN transcript_path    ELSE ?3 END
WHERE alias = ?1`, alias, harnessSessionID, transcriptPath)
	return err
}

// DepartAgent tombstones alias (spec: deregistration must survive so
// departed history stays drillable): sets departed=1 in place. Identity,
// project, label, and read-state (last_read_entry_id/last_read_at) are all
// preserved — this is the deregister_agent op's normal path now, replacing
// the old hard DELETE. Unknown alias is a no-op (no error), mirroring
// DeleteAgent's own contract. RegisterAgent's upsert is the only way back to
// departed=0 (a returning session revives the row).
func (s *Store) DepartAgent(alias string) error {
	_, err := s.db.Exec(`UPDATE agents SET departed=1 WHERE alias=?`, alias)
	return err
}

// SetSessionLabel updates the STORED label for every non-departed alias
// registered to the (socketPath, sessionID) tuple — a label is a
// session-level property, so all sibling aliases move together. This is the
// daemon-side half of `muster label` (the set_label op): the CLI writes the
// live tmux option and pushes the same value here in the same command, so
// the stored copy the daemon's own resolver reads (resolveAgentTarget —
// tmux-agnostic by rule, it never re-reads tmux) never drifts from what a
// CLI caller resolving against live tmux sees. Clearing is label="",
// manual=false. Returns how many rows changed; 0 with an empty tuple
// component (nothing addressable to update — matches SessionUnread's
// never-group-on-empty rule).
// deviceID scopes the session to one machine (see store.API's note on the
// tuple): a label is an ADDRESS, so relabelling a colliding tuple on another
// device would readdress a peer's agent out from under it.
//
// sessionCreated scopes the write to one tmux-session incarnation (spec
// §5.1): only rows whose session_created equals it — and is non-zero — are
// eligible, so a label write can land on the CURRENT session only, never on
// a recycled-ID ghost from a dead server incarnation (created mismatch) or
// an unprovable legacy row (created 0, indistinguishable from a ghost). The
// caller vouches that sessionCreated is the live session's own value, just
// captured from the pane it is labeling.
//
// The two scopes are independent and both required: deviceID says WHICH
// MACHINE's tuple, sessionCreated says WHICH INCARNATION of it. Neither
// implies the other — a colliding tuple on a peer machine has its own
// unrelated creation times.
func (s *Store) SetSessionLabel(deviceID, socketPath, sessionID string, sessionCreated int64, label string, manual bool) (int64, error) {
	if socketPath == "" || sessionID == "" {
		return 0, nil
	}
	res, err := s.db.Exec(`
UPDATE agents SET label=?, label_manual=?
WHERE device_id=? AND socket_path=? AND session_id=? AND departed=0
  AND session_created=? AND session_created != 0`,
		label, manual, deviceID, socketPath, sessionID, sessionCreated)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DepartStaleSiblings tombstones every OTHER non-departed alias registered to
// the same (socketPath, sessionID) tuple whose session_created differs from
// created — ghosts from a previous tmux server incarnation whose session ID
// was recycled. The inference needs no tmux access (the daemon stays
// tmux-agnostic): creation time is immutable for a session's lifetime, so two
// rows claiming one session ID with different non-zero creation times cannot
// both be live, and the caller vouches that created is the CURRENT session's
// (it just captured it from the live pane it is registering from). Rows with
// session_created 0 are spared — a pre-upgrade registration on the same
// still-running session is indistinguishable from a ghost, and it self-heals
// to a real value the next time that agent re-registers. No-op (0, nil) when
// any tuple component is empty/zero. Returns the tombstoned aliases so the
// caller can reconcile their badges.
// deviceID scopes the reaping to one machine (see store.API's note on the
// tuple). The evidence this infers from — "another row claims my session id
// under a different creation time" — is only evidence among rows from the SAME
// machine: two laptops' sessions have unrelated creation times, so without the
// device dimension registering on one would tombstone the other's LIVE agent.
func (s *Store) DepartStaleSiblings(deviceID, socketPath, sessionID string, created int64, keepAlias string) ([]string, error) {
	if socketPath == "" || sessionID == "" || created == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
SELECT alias FROM agents
WHERE device_id=? AND socket_path=? AND session_id=? AND departed=0
  AND session_created != 0 AND session_created != ? AND alias != ?`,
		deviceID, socketPath, sessionID, created, keepAlias)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var stale []string
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		stale = append(stale, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, alias := range stale {
		if err := s.DepartAgent(alias); err != nil {
			return nil, err
		}
	}
	return stale, nil
}

// DeleteAgent hard-deletes an agent's registration by alias — irreversible:
// identity, project, label, and read-state are all gone, not just flagged.
// Unknown alias is a no-op (no error). Message/task history is unaffected —
// threads store the alias as text, not a foreign key. This is now reserved
// for `muster gc --purge-agents` (the daemon's purge_agent op); plain
// deregistration goes through DepartAgent instead.
func (s *Store) DeleteAgent(alias string) error {
	_, err := s.db.Exec(`DELETE FROM agents WHERE alias=?`, alias)
	return err
}

// StatusCounts returns every registered alias's side-effect-free inbox counts
// (unread + action_required), for a picker/attention column that polls on a
// cadence. It moves NO watermark and journals NO peek — a pure read, which is
// exactly why a poller uses it instead of get_inbox. Departed aliases are
// included (their mail still waits), matching ListAgents. The per-alias count
// reuses the canonical unread predicate (standing/retracted-aware), so it can
// never disagree with UnreadCount/get_inbox about what is unread.
func (s *Store) StatusCounts() ([]AliasStatus, error) {
	agents, err := s.ListAgents()
	if err != nil {
		return nil, err
	}
	out := make([]AliasStatus, 0, len(agents))
	for _, a := range agents {
		unread, action, err := s.unreadAndAction(a.Alias)
		if err != nil {
			return nil, err
		}
		out = append(out, AliasStatus{Alias: a.Alias, Unread: unread, ActionRequired: action})
	}
	return out, nil
}

// unreadAndAction is UnreadCount plus the action-requested subset in one query:
// the count of threads concerning alias with an unread entry, and how many of
// those have effective intent action-requested. Same predicate as UnreadCount
// (they must never drift), read-only.
func (s *Store) unreadAndAction(alias string) (unread, action int, err error) {
	err = s.db.QueryRow(`
SELECT COUNT(*),
       COUNT(CASE WHEN `+effectiveIntent+` = 'action-requested' THEN 1 END)
FROM threads
WHERE `+threadConcerns+`
  AND EXISTS (SELECT 1 FROM entries e
              WHERE e.thread_id = threads.id
                AND ((threads.standing = 1 AND threads.standing_retracted = 0 AND e.id > COALESCE((SELECT last_read_standing_entry_id FROM agents WHERE alias=?), 0))
                  OR (threads.standing = 0 AND e.id > COALESCE((SELECT last_read_entry_id FROM agents WHERE alias=?), 0)))
                AND e.from_agent != ?)`,
		alias, alias, alias, alias, alias, alias, alias).Scan(&unread, &action)
	return unread, action, err
}

// UnreadCount returns how many threads concerning alias (threadConcerns —
// matching Inbox exactly) contain an entry with id greater than the agent's
// entry-ID read watermark (last_read_entry_id) that was written by someone
// else. Judging entries rather than the thread's updated_at means an agent's
// own reply never re-flags its own inbox, and a peer's reply on a thread the
// agent originated does. The watermark is an entry ID, not a wall-clock
// timestamp, so two entries landing in the same millisecond never race a
// strict "after last read" comparison (see MarkRead).
func (s *Store) UnreadCount(alias string) (int, error) {
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(*) FROM threads
WHERE `+threadConcerns+`
  AND EXISTS (SELECT 1 FROM entries e
              WHERE e.thread_id = threads.id
                AND ((threads.standing = 1 AND threads.standing_retracted = 0 AND e.id > COALESCE((SELECT last_read_standing_entry_id FROM agents WHERE alias=?), 0))
                  OR (threads.standing = 0 AND e.id > COALESCE((SELECT last_read_entry_id FROM agents WHERE alias=?), 0)))
                AND e.from_agent != ?)`,
		alias, alias, alias, alias, alias, alias, alias).Scan(&n)
	return n, err
}

// MarkRead records that alias has read its Inbox snapshot through upToEntryID.
// The caller supplies the snapshot bound; querying entries here would create a
// race where a newer entry could be marked read without ever being returned.
// last_read_at is stamped for display only and does not affect unread math.
func (s *Store) MarkRead(alias string, upToEntryID int64) error {
	now := clock.NowMillis()
	_, err := s.db.Exec(`
UPDATE agents
SET last_read_entry_id = MAX(last_read_entry_id, ?),
    last_read_standing_entry_id = MAX(last_read_standing_entry_id, ?),
    last_read_at = ?
WHERE alias = ?`, upToEntryID, upToEntryID, now, alias)
	return err
}

// ErrBecomeFromMissing / ErrBecomeToExists are become's guard sentinels —
// the daemon maps them to loud, hint-carrying wire errors.
var (
	ErrBecomeFromMissing = errors.New("become: from alias not found")
	ErrBecomeToExists    = errors.New("become: to alias already exists")
)

// Become claims a new name for an existing identity (spec:
// become-claim-your-name): inserts to as a CLONE of from — tuple, DEVICE,
// harness link, project, label, role, model, and the READ WATERMARK, without which
// the claimed identity would see all of history as unread — then retires
// from as a tombstone AND stamps from.superseded_by = to. to must not be a
// LIVE row: a live row is someone else's identity, and merging into it is
// exactly the confusion this feature exists to kill. A DEPARTED to, by
// contrast, is nobody's live identity any more (spec 2026-08-21 §4:
// reclaimable names) — the claim may reclaim it, hard-deleting the tombstone
// and replacing it with the clone in the same transaction; nothing in
// SessionAliasLineage's walk can reach a name that changed ownership out from
// under it (Become's SEED always points forward via superseded_by, never a
// TARGET the way this delete does), so no waiting mail is orphaned by the
// reclaim. from may already be departed (a claim after gc swept the seed).
// The clone's INSERT deliberately omits superseded_by (it defaults to ”) —
// the successor starts unsuperseded even if from was itself a superseded row
// (a chained become A→B→C leaves B's superseded_by pointing at C, never
// inherited backward onto C). superseded_by is the ground truth
// hookSessionStartResume uses to keep a retired seed from resurrecting on
// resume, in place of inferring retirement from tuple coincidence. One
// transaction: a crash mid-become never leaves both rows live, and never
// leaves a reclaimed target half-deleted.
//
// device_id is part of the cloned tuple, not an afterthought: the successor
// must land on the SAME device as the seed, because the session tuple that
// addresses it is (device, socket, session). A clone that defaulted device_id
// to "" would put the successor on a tuple no live session queries — the
// claim would appear to succeed and the badge would go dark forever.
func (s *Store) Become(from, to string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var departed bool
	err = tx.QueryRow(`SELECT departed FROM agents WHERE alias=?`, to).Scan(&departed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// to does not exist at all: proceed, exactly as before.
	case err != nil:
		return err
	case !departed:
		return ErrBecomeToExists
	default:
		// to is a tombstone: reclaim it. The DELETE and the clone INSERT
		// below run in the same transaction as the retire of from, so a
		// crash between them cannot leave to half-gone.
		if _, err := tx.Exec(`DELETE FROM agents WHERE alias=?`, to); err != nil {
			return err
		}
	}
	now := clock.NowMillis()
	res, err := tx.Exec(`
INSERT INTO agents (alias, role, model_type, socket_path, pane_id, session_name, session_id, session_created, device_id, device_name, harness_session_id, transcript_path, project, label, label_manual, departed, registered_at, last_seen, last_read_entry_id, last_read_standing_entry_id, last_read_at)
SELECT ?, role, model_type, socket_path, pane_id, session_name, session_id, session_created, device_id, device_name, harness_session_id, transcript_path, project, label, label_manual, 0, ?, ?, last_read_entry_id, last_read_standing_entry_id, last_read_at
FROM agents WHERE alias=?`, to, now, now, from)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err != nil {
		return err
	} else if rows == 0 {
		return ErrBecomeFromMissing
	}
	if _, err := tx.Exec(`UPDATE agents SET departed=1, superseded_by=? WHERE alias=?`, to, from); err != nil {
		return err
	}
	return tx.Commit()
}

// SessionUnread is the ONE canonical session-level unread query (spec §3):
// all aliases sharing the exact (socketPath, sessionID) tuple are one actor
// identity for unread math and actor exclusion. total is the count of
// distinct threads concerning ANY alias of the session (threadConcernsJoin —
// semantically threadConcerns, re-expressed as a join; see
// TestThreadConcernsSessionJoinEquivalence) that have an entry newer than
// that alias's own watermark written by someone who is NOT any alias of the
// session — so a session's own writes under either alias never make its own
// threads unread, and a broadcast concerning two sibling aliases counts once,
// never twice (no summing of per-alias counts). action is the subset whose
// effective intent (effectiveIntent) is action-requested. An empty sessionID
// never groups: it matches no agents, so both results are 0 (per-alias
// identity is UnreadCount's job for such agents). socketPath MAY be empty —
// ("", harness session UUID) is the paneless tuple (see internal/harnessenv),
// a real session identity whose sibling aliases group exactly like a tmux
// session's; the sessionID guard alone keeps pre-harnessenv no-tmux rows
// (both fields empty) from ever grouping with each other.
//
// The session's identity is (deviceID, socketPath, sessionID), never the pair
// alone — see store.API. The device dimension is what makes the actor
// exclusion below correct in a shared store: without it, an alias on ANOTHER
// machine that happens to share this tuple is treated as a sibling of this
// session, so its message to this session is discarded as "one of my own
// writes" and the badge never lights. That is the two-device miss the hosted
// backend exists to make impossible.
//
// Mail follows the name, wherever the conversation moved (become-retired
// lineage, found live: a straggler addressed to a retired seed alias must
// still light the badge on the tuple its identity moved to). The sess CTE is
// therefore a WITH RECURSIVE lineage walk, not a flat tuple match: the base
// case is every alias currently sitting on the queried (device, socket,
// session) tuple; each recursive step adds rows whose superseded_by points at
// an alias already in the set. store.Become stamps superseded_by on the SEED
// pointing FORWARD at its successor (from.superseded_by = to), so the walk goes
// backward through the chain — for A→B→C (A became B, B became C: A's
// superseded_by='B', B's superseded_by='C'), starting from C on the live
// tuple the first step finds B (superseded_by='C') and the next step finds
// A (superseded_by='B'), even though A's own row still sits on a
// long-dead tuple. Each lineage row keeps ITS OWN
// last_read_entry_id as the EXISTS watermark below — a superseded row's
// watermark is frozen at the moment it was retired (exactly the read state
// Become cloned forward onto its successor), so this stays per-row
// semantics, never a session-wide max. UNION (not UNION ALL) dedups on the
// full row; since alias is a primary key, that collapses to "one alias
// enters the set at most once" — a malformed superseded_by cycle (A→B→A)
// simply produces no new row on the step that would re-add an
// already-present alias, so the recursion terminates instead of hanging
// (see TestSessionUnreadLineageCycleGuard).
//
// deviceID says WHICH MACHINE's tuple; sessionCreated says WHICH INCARNATION
// of it. BOTH scope the BASE CASE ONLY and the recursive lineage step stays
// unscoped by both — one rule, two dimensions, argued once on store.API's
// SessionUnread declaration. Concretely here: only rows whose session_created
// equals sessionCreated seed the walk, and 0 seeds nothing (attribution
// requires proof — spec §5.1, 2026-08-05); an empty socketPath (the paneless
// tuple) is exempt from the incarnation check, because harness UUIDs are
// never recycled. Superseded rows sit on old tuples, on possibly other
// machines, forever — so scoping the recursive step would drop true
// positives, never remove a false one.
//
// NOTE the ?1=” exemption is on socketPath, NOT deviceID: a paneless
// registration still belongs to exactly one machine, so the device filter is
// unconditional while the incarnation filter is not.
//
// The base case also excludes a departed row with no successor
// (superseded_by = ”): such a row is either this tuple's own alias that was
// deregistered without ever being claimed onward (its mail is orphaned — no
// live identity owns it any more), or, on a reused tuple, a PRIOR
// conversation's tombstone that happens to share it — and the two are
// indistinguishable from the row alone, so both are excluded alike. A
// departed row that WAS claimed via Become stays fully in scope (it enters
// through the recursive step, unfiltered): that is the live successor's own
// lineage, not somebody else's leftover.
func (s *Store) SessionUnread(deviceID, socketPath, sessionID string, sessionCreated int64) (total, action int, err error) {
	err = s.db.QueryRow(`
WITH RECURSIVE sess AS (
  SELECT alias, last_read_entry_id, last_read_standing_entry_id, superseded_by FROM agents
  WHERE device_id = ?3 AND socket_path = ?1 AND session_id = ?2 AND ?2 != ''
    AND (?1 = '' OR (session_created = ?4 AND ?4 != 0))
    AND (departed = 0 OR superseded_by != '')
  UNION
  SELECT a.alias, a.last_read_entry_id, a.last_read_standing_entry_id, a.superseded_by
  FROM agents a JOIN sess ON a.superseded_by = sess.alias
)
SELECT
  COUNT(DISTINCT threads.id),
  COUNT(DISTINCT CASE WHEN `+effectiveIntent+` = 'action-requested' THEN threads.id END)
FROM threads
JOIN sess ON `+threadConcernsJoin+`
WHERE EXISTS (SELECT 1 FROM entries e
              WHERE e.thread_id = threads.id
                AND ((threads.standing = 1 AND threads.standing_retracted = 0 AND e.id > sess.last_read_standing_entry_id)
                  OR (threads.standing = 0 AND e.id > sess.last_read_entry_id))
                AND e.from_agent NOT IN (SELECT alias FROM sess))`,
		socketPath, sessionID, deviceID, sessionCreated).Scan(&total, &action)
	return total, action, err
}

// SessionAliasLineage returns every alias belonging to a session's
// supersession lineage — the exact same WITH RECURSIVE walk SessionUnread
// runs (see its doc comment for the "mail follows the name" rule, the
// tombstone-with-no-successor exclusion, and the cycle-termination
// argument), projected down to just the alias column. It backs the daemon's
// session_aliases op, which includes a departed alias ON PURPOSE once it is
// IN the walk (their unread mail still needs draining) — a become-retired
// seed on a long-dead tuple belongs in the list precisely because it is
// departed, not despite it. What it does not include is a departed row that
// was never claimed onward: nothing points forward from it, and (per
// SessionUnread's doc comment) it is indistinguishable from a prior
// conversation's leftover tombstone on a reused tuple. Result is sorted and
// deduplicated; an empty sessionID matches no
// agents (mirrors SessionUnread's empty-tuple guard) and returns an empty
// slice.
//
// Base-case-scoped on BOTH dimensions exactly like SessionUnread — device and
// incarnation — with the recursive superseded_by step unscoped by both. The
// argument is identical and lives on store.API's SessionUnread declaration;
// what makes it bite HERE is that this op's answer is what the SessionStart
// hook drains and the nudge path addresses, so an unscoped base case would
// hand a session a colliding machine's aliases, or a recycled session ID's
// dead ghosts, to drain and address as its own.
func (s *Store) SessionAliasLineage(deviceID, socketPath, sessionID string, sessionCreated int64) ([]string, error) {
	rows, err := s.db.Query(`
WITH RECURSIVE sess AS (
  SELECT alias, superseded_by FROM agents
  WHERE device_id = ?3 AND socket_path = ?1 AND session_id = ?2 AND ?2 != ''
    AND (?1 = '' OR (session_created = ?4 AND ?4 != 0))
    AND (departed = 0 OR superseded_by != '')
  UNION
  SELECT a.alias, a.superseded_by
  FROM agents a JOIN sess ON a.superseded_by = sess.alias
)
SELECT alias FROM sess ORDER BY alias`, socketPath, sessionID, deviceID, sessionCreated)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		out = append(out, alias)
	}
	return out, rows.Err()
}
