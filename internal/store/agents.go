package store

import "github.com/schuettc/muster/internal/clock"

// RegisterAgent upserts by Alias: inserts on first sight (stamping RegisteredAt),
// and on conflict refreshes the tuple + LastSeen while preserving RegisteredAt.
func (s *Store) RegisterAgent(a Agent) error {
	now := clock.NowMillis()
	_, err := s.db.Exec(`
INSERT INTO agents (alias, role, model_type, socket_path, pane_id, session_name, registered_at, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(alias) DO UPDATE SET
    role=excluded.role,
    model_type=excluded.model_type,
    socket_path=excluded.socket_path,
    pane_id=excluded.pane_id,
    session_name=excluded.session_name,
    last_seen=excluded.last_seen`,
		a.Alias, a.Role, a.ModelType, a.SocketPath, a.PaneID, a.SessionName, now, now)
	return err
}

// ListAgents returns all agents ordered by alias.
func (s *Store) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(`
SELECT alias, role, model_type, socket_path, pane_id, session_name, registered_at, last_seen
FROM agents ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.RegisteredAt, &a.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TouchAgent bumps last_seen. No error if the agent is unknown.
func (s *Store) TouchAgent(alias string) error {
	_, err := s.db.Exec(`UPDATE agents SET last_seen=? WHERE alias=?`, clock.NowMillis(), alias)
	return err
}
