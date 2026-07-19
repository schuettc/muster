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
// either branch, so it survives the roundtrip intact.
func (s *Store) RegisterAgent(a Agent) error {
	now := clock.NowMillis()
	_, err := s.db.Exec(`
INSERT INTO agents (alias, role, model_type, socket_path, pane_id, session_name, session_id, session_created, project, label, label_manual, departed, registered_at, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
ON CONFLICT(alias) DO UPDATE SET
    role=excluded.role,
    model_type=excluded.model_type,
    socket_path=excluded.socket_path,
    pane_id=excluded.pane_id,
    session_name=excluded.session_name,
    session_id=excluded.session_id,
    session_created=excluded.session_created,
    project=excluded.project,
    label=excluded.label,
    label_manual=excluded.label_manual,
    departed=0,
    last_seen=excluded.last_seen`,
		a.Alias, a.Role, a.ModelType, a.SocketPath, a.PaneID, a.SessionName, a.SessionID, a.SessionCreated,
		a.Project, a.Label, a.LabelManual, now, now)
	return err
}

// ListAgents returns all agents ordered by alias — departed (tombstoned)
// agents included: their rows are history, not gone (see DepartAgent).
func (s *Store) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(`
SELECT alias, role, model_type, socket_path, pane_id, session_name, session_id, session_created, project, label, label_manual, registered_at, last_seen, last_read_entry_id, departed
FROM agents ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.SessionID, &a.SessionCreated, &a.Project, &a.Label, &a.LabelManual, &a.RegisteredAt, &a.LastSeen, &a.LastReadEntryID, &a.Departed); err != nil {
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
	var a Agent
	err := s.db.QueryRow(`
SELECT alias, role, model_type, socket_path, pane_id, session_name, session_id, session_created, project, label, label_manual, registered_at, last_seen, last_read_entry_id, departed
FROM agents WHERE alias=?`, alias).
		Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.SessionID, &a.SessionCreated, &a.Project, &a.Label, &a.LabelManual, &a.RegisteredAt, &a.LastSeen, &a.LastReadEntryID, &a.Departed)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, false, nil
	}
	if err != nil {
		return Agent{}, false, err
	}
	return a, true, nil
}

// TouchAgent bumps last_seen. No error if the agent is unknown.
func (s *Store) TouchAgent(alias string) error {
	_, err := s.db.Exec(`UPDATE agents SET last_seen=? WHERE alias=?`, clock.NowMillis(), alias)
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
func (s *Store) SetSessionLabel(socketPath, sessionID, label string, manual bool) (int64, error) {
	if socketPath == "" || sessionID == "" {
		return 0, nil
	}
	res, err := s.db.Exec(`
UPDATE agents SET label=?, label_manual=?
WHERE socket_path=? AND session_id=? AND departed=0`,
		label, manual, socketPath, sessionID)
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
func (s *Store) DepartStaleSiblings(socketPath, sessionID string, created int64, keepAlias string) ([]string, error) {
	if socketPath == "" || sessionID == "" || created == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
SELECT alias FROM agents
WHERE socket_path=? AND session_id=? AND departed=0
  AND session_created != 0 AND session_created != ? AND alias != ?`,
		socketPath, sessionID, created, keepAlias)
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
                AND e.id > COALESCE((SELECT last_read_entry_id FROM agents WHERE alias=?), 0)
                AND e.from_agent != ?)`,
		alias, alias, alias, alias, alias).Scan(&n)
	return n, err
}

// MarkRead records that alias has read its inbox up to the highest entry ID
// that exists right now: the read and the watermark snapshot happen in one
// transaction, so an entry appended concurrently (even in the same
// millisecond) is never mistaken for already read. last_read_at is also
// stamped, for display purposes only — it is no longer consulted by any
// unread predicate.
func (s *Store) MarkRead(alias string) error {
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var maxID int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM entries`).Scan(&maxID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agents SET last_read_entry_id=?, last_read_at=? WHERE alias=?`, maxID, now, alias); err != nil {
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
// effective intent (effectiveIntent) is action-requested. An empty
// socketPath or sessionID never groups: it matches no agents, so both
// results are 0 (per-alias identity is UnreadCount's job for such agents,
// e.g. one registered without a live tmux pane).
func (s *Store) SessionUnread(socketPath, sessionID string) (total, action int, err error) {
	err = s.db.QueryRow(`
WITH sess AS (SELECT alias, last_read_entry_id FROM agents
              WHERE socket_path = ?1 AND session_id = ?2 AND ?1 != '' AND ?2 != '')
SELECT
  COUNT(DISTINCT threads.id),
  COUNT(DISTINCT CASE WHEN `+effectiveIntent+` = 'action-requested' THEN threads.id END)
FROM threads
JOIN sess ON `+threadConcernsJoin+`
WHERE EXISTS (SELECT 1 FROM entries e
              WHERE e.thread_id = threads.id
                AND e.id > sess.last_read_entry_id
                AND e.from_agent NOT IN (SELECT alias FROM sess))`,
		socketPath, sessionID).Scan(&total, &action)
	return total, action, err
}
