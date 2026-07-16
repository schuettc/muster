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
SELECT alias, role, model_type, socket_path, pane_id, session_name, session_id, project, label, label_manual, registered_at, last_seen
FROM agents ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.SessionID, &a.Project, &a.Label, &a.LabelManual, &a.RegisteredAt, &a.LastSeen); err != nil {
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
SELECT alias, role, model_type, socket_path, pane_id, session_name, session_id, project, label, label_manual, registered_at, last_seen
FROM agents WHERE alias=?`, alias).
		Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.SessionID, &a.Project, &a.Label, &a.LabelManual, &a.RegisteredAt, &a.LastSeen)
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
// matching Inbox exactly) contain an entry newer than the agent's last inbox
// read that was written by someone else. Judging entries rather than the
// thread's updated_at means an agent's own reply never re-flags its own
// inbox, and a peer's reply on a thread the agent originated does.
func (s *Store) UnreadCount(alias string) (int, error) {
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(*) FROM threads
WHERE `+threadConcerns+`
  AND EXISTS (SELECT 1 FROM entries e
              WHERE e.thread_id = threads.id
                AND e.created_at > COALESCE((SELECT last_read_at FROM agents WHERE alias=?), 0)
                AND e.from_agent != ?)`,
		alias, alias, alias, alias, alias).Scan(&n)
	return n, err
}

// MarkRead records that alias has read its inbox up to now.
func (s *Store) MarkRead(alias string) error {
	_, err := s.db.Exec(`UPDATE agents SET last_read_at=? WHERE alias=?`, clock.NowMillis(), alias)
	return err
}
