package store

import "github.com/schuettc/muster/internal/clock"

// defaultEventLimit bounds RecentEvents when the caller passes no limit.
const defaultEventLimit = 50

// AppendEvent records one observability event, stamped now. Callers treat
// event logging as best-effort: an append failure must never fail the bus
// operation it describes.
func (s *Store) AppendEvent(e Event) error {
	_, err := s.db.Exec(`
INSERT INTO events (ts, kind, agent, target, thread_id, count, detail)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		clock.NowMillis(), e.Kind, e.Agent, e.Target, e.ThreadID, e.Count, e.Detail)
	return err
}

// RecentEvents returns the newest events first, optionally filtered to one
// agent. limit <= 0 falls back to defaultEventLimit.
func (s *Store) RecentEvents(agent string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = defaultEventLimit
	}
	rows, err := s.db.Query(`
SELECT id, ts, kind, agent, thread_id, count, detail FROM events
WHERE (? = '' OR agent = ?)
ORDER BY id DESC LIMIT ?`, agent, agent, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Agent, &e.ThreadID, &e.Count, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
