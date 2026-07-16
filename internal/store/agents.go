package store

import (
	"database/sql"
	"errors"

	"github.com/schuettc/muster/internal/clock"
)

// RegisterAgent upserts by Alias: inserts on first sight (stamping RegisteredAt),
// and on conflict refreshes the tuple + LastSeen while preserving RegisteredAt.
func (s *Store) RegisterAgent(a Agent) error {
	now := clock.NowMillis()
	_, err := s.db.Exec(`
INSERT INTO agents (alias, role, model_type, socket_path, pane_id, session_name, session_id, project, label, label_manual, registered_at, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(alias) DO UPDATE SET
    role=excluded.role,
    model_type=excluded.model_type,
    socket_path=excluded.socket_path,
    pane_id=excluded.pane_id,
    session_name=excluded.session_name,
    session_id=excluded.session_id,
    project=excluded.project,
    label=excluded.label,
    label_manual=excluded.label_manual,
    last_seen=excluded.last_seen`,
		a.Alias, a.Role, a.ModelType, a.SocketPath, a.PaneID, a.SessionName, a.SessionID,
		a.Project, a.Label, a.LabelManual, now, now)
	return err
}

// ListAgents returns all agents ordered by alias.
func (s *Store) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(`
SELECT alias, role, model_type, socket_path, pane_id, session_name, session_id, project, label, label_manual, registered_at, last_seen, last_read_entry_id
FROM agents ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.SessionID, &a.Project, &a.Label, &a.LabelManual, &a.RegisteredAt, &a.LastSeen, &a.LastReadEntryID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAgent looks up a single agent by alias. ok is false if no such agent is registered.
func (s *Store) GetAgent(alias string) (Agent, bool, error) {
	var a Agent
	err := s.db.QueryRow(`
SELECT alias, role, model_type, socket_path, pane_id, session_name, session_id, project, label, label_manual, registered_at, last_seen, last_read_entry_id
FROM agents WHERE alias=?`, alias).
		Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.SessionID, &a.Project, &a.Label, &a.LabelManual, &a.RegisteredAt, &a.LastSeen, &a.LastReadEntryID)
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

// DeleteAgent removes an agent's registration by alias. Unknown alias is a
// no-op (no error). Message/task history is unaffected — threads store the
// alias as text, not a foreign key.
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
