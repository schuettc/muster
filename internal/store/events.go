package store

import (
	"fmt"
	"strings"

	"github.com/schuettc/muster/internal/clock"
)

// maxEventLimit bounds any single Events query.
const maxEventLimit = 1000

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

// EventQuery selects journal rows. Mode is explicit: Backlog=true reads
// newest-first up to Limit; otherwise follow mode reads id > AfterID
// oldest-first. Agent matches events the alias is CONCERNED in — as actor,
// as exact 'agent:<alias>' target, as bare-alias target (nudge), or via the
// event's thread satisfying threadConcerns (what makes replies on your
// threads match despite their empty target). Role matching uses the alias's
// current role, same as threadConcerns everywhere else.
type EventQuery struct {
	Agent    string // exact-alias concern filter ("" = all)
	Kind     string // exact kind ("" = all)
	ThreadID int64  // >0 filters to one thread
	AfterID  int64  // follow mode: id > AfterID, oldest-first
	Limit    int    // backlog mode row cap; 0 in backlog mode = no rows
	Backlog  bool   // true: newest-first LIMIT; false: follow mode
}

// Events runs q against the journal (see EventQuery for mode semantics).
func (s *Store) Events(q EventQuery) ([]Event, error) {
	if q.AfterID < 0 || q.ThreadID < 0 {
		return nil, fmt.Errorf("negative id in event query")
	}
	if q.Backlog && q.Limit <= 0 {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 || limit > maxEventLimit {
		limit = maxEventLimit
	}
	where := []string{"1=1"}
	var args []any
	if q.Agent != "" {
		where = append(where, `(events.agent = ?
   OR events.target = 'agent:'||?
   OR events.target = ?
   OR (events.thread_id > 0 AND EXISTS (
        SELECT 1 FROM threads WHERE threads.id = events.thread_id AND `+threadConcerns+`)))`)
		args = append(args, q.Agent, q.Agent, q.Agent, q.Agent, q.Agent, q.Agent)
	}
	if q.Kind != "" {
		where = append(where, "events.kind = ?")
		args = append(args, q.Kind)
	}
	if q.ThreadID > 0 {
		where = append(where, "events.thread_id = ?")
		args = append(args, q.ThreadID)
	}
	order := "events.id DESC"
	if !q.Backlog {
		where = append(where, "events.id > ?")
		args = append(args, q.AfterID)
		order = "events.id ASC"
	}
	args = append(args, limit)
	rows, err := s.db.Query(`
SELECT events.id, events.ts, events.kind, events.agent, events.target,
       events.thread_id, events.count, events.detail,
       COALESCE(threads.subject, '')
FROM events LEFT JOIN threads ON threads.id = events.thread_id
WHERE `+strings.Join(where, " AND ")+`
ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Agent, &e.Target, &e.ThreadID, &e.Count, &e.Detail, &e.Subject); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents deletes journal rows with ts < olderThanMillis (a row exactly
// at the cutoff survives), returning the count deleted.
func (s *Store) PruneEvents(olderThanMillis int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM events WHERE ts < ?`, olderThanMillis)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MaxEventID returns the journal high-water mark (0 on an empty journal).
func (s *Store) MaxEventID() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&n)
	return n, err
}
