package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/schuettc/muster/internal/clock"
)

// ErrThreadNotFound is returned when an operation targets a threadID that
// does not exist.
var ErrThreadNotFound = errors.New("thread not found")

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// validIntent reports whether intent is a value CreateThread accepts: ""
// (unspecified) or one of the three named intents (models.go).
func validIntent(intent string) bool {
	switch intent {
	case "", IntentFYI, IntentReply, IntentAction:
		return true
	default:
		return false
	}
}

// CreateThread inserts a thread and its first entry atomically.
func (s *Store) CreateThread(t Thread, firstBody string) (int64, error) {
	if !validIntent(t.Intent) {
		return 0, fmt.Errorf("invalid intent %q", t.Intent)
	}
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`
INSERT INTO threads (kind, from_agent, to_kind, to_target, subject, ref, status, intent, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Kind, t.FromAgent, t.ToKind, t.ToTarget, t.Subject, t.Ref, nullable(t.Status), t.Intent, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`
INSERT INTO entries (thread_id, from_agent, body, status_change, created_at)
VALUES (?, ?, ?, ?, ?)`, id, t.FromAgent, firstBody, nil, now); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// AppendEntry adds an entry and advances the thread's updated_at.
func (s *Store) AppendEntry(threadID int64, fromAgent, body, statusChange string) (int64, error) {
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`
INSERT INTO entries (thread_id, from_agent, body, status_change, created_at)
VALUES (?, ?, ?, ?, ?)`, threadID, fromAgent, body, nullable(statusChange), now)
	if err != nil {
		return 0, err
	}
	upd, err := tx.Exec(`UPDATE threads SET updated_at=? WHERE id=?`, now, threadID)
	if err != nil {
		return 0, err
	}
	n, err := upd.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrThreadNotFound
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func scanThread(row interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var status sql.NullString
	err := row.Scan(&t.ID, &t.Kind, &t.FromAgent, &t.ToKind, &t.ToTarget, &t.Subject, &t.Ref, &status, &t.Intent, &t.CreatedAt, &t.UpdatedAt)
	if status.Valid {
		t.Status = status.String
	}
	return t, err
}

const threadCols = `id, kind, from_agent, to_kind, to_target, subject, ref, status, intent, created_at, updated_at`

// effectiveIntent is the ONE canonical SQL fragment for a thread's operative
// intent (spec §2): a task is a request for action, including every
// pre-existing v0.5 task row (intent ” before this migration) — so a task
// with no explicit intent counts as action-requested, never unspecified.
// Every read surface that needs the operative value (Threads, SessionUnread's
// action count) uses this fragment verbatim; it references the bare "threads"
// table name (no alias), so callers must either query FROM threads directly
// or evaluate it in a scope where "threads" is the unaliased table (e.g.
// inside a CTE's own SELECT FROM threads).
const effectiveIntent = `CASE WHEN threads.kind='task' AND COALESCE(threads.intent,'')='' THEN 'action-requested' ELSE COALESCE(threads.intent,'') END`

// GetThread returns the thread and its entries (ordered by id).
func (s *Store) GetThread(id int64) (Thread, []Entry, error) {
	t, err := scanThread(s.db.QueryRow(`SELECT `+threadCols+` FROM threads WHERE id=?`, id))
	if err != nil {
		return Thread{}, nil, err
	}
	rows, err := s.db.Query(`SELECT id, thread_id, from_agent, body, status_change, created_at FROM entries WHERE thread_id=? ORDER BY id`, id)
	if err != nil {
		return Thread{}, nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []Entry
	for rows.Next() {
		var e Entry
		var sc sql.NullString
		if err := rows.Scan(&e.ID, &e.ThreadID, &e.FromAgent, &e.Body, &sc, &e.CreatedAt); err != nil {
			return Thread{}, nil, err
		}
		if sc.Valid {
			e.StatusChange = sc.String
		}
		entries = append(entries, e)
	}
	return t, entries, rows.Err()
}

// threadConcerns is the ONE canonical predicate for "does this thread concern
// agent X": addressed to X directly, to X's role, broadcast, or originated by
// X. Every surface that answers that question — Inbox, UnreadCount, and (by
// construction, since it walks originator+recipients) the daemon's notify
// fan-out — must agree with this fragment; the surfaces diverging is exactly
// how replies to originated threads once went invisible. Binds the alias
// three times.
const threadConcerns = `((threads.to_kind='agent'  AND threads.to_target=?)
   OR (threads.to_kind='role'      AND threads.to_target != '' AND threads.to_target=(SELECT role FROM agents WHERE alias=?))
   OR (threads.to_kind='broadcast')
   OR (threads.from_agent=?))`

// threadConcernsJoin is threadConcerns re-expressed as a JOIN predicate
// against a CTE column (sess.alias) instead of a literal alias bound three
// times — needed by SessionUnread, where "the alias" ranges over every alias
// of a session rather than one bound value. It must stay semantically
// identical to threadConcerns (update both together);
// TestThreadConcernsSessionJoinEquivalence asserts they agree across a
// fixture matrix of thread shapes and aliases. Callers provide a CTE named
// "sess" with an "alias" column.
const threadConcernsJoin = `((threads.to_kind='agent' AND threads.to_target=sess.alias)
   OR (threads.to_kind='role'     AND threads.to_target != '' AND threads.to_target=(SELECT role FROM agents WHERE alias=sess.alias))
   OR (threads.to_kind='broadcast')
   OR (threads.from_agent=sess.alias))`

// Inbox returns every thread that concerns alias (see threadConcerns):
// addressed to it directly, to its role, broadcast, or originated by it —
// so replies on threads the agent started show up here too.
func (s *Store) Inbox(alias string) ([]Thread, error) {
	rows, err := s.db.Query(`
SELECT `+threadCols+` FROM threads
WHERE `+threadConcerns+`
ORDER BY updated_at DESC`, alias, alias, alias)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// clampThreadsLimit enforces Threads()'s documented range: <=0 defaults to
// 100, anything over 500 clamps to 500.
func clampThreadsLimit(limit int) int {
	switch {
	case limit <= 0:
		return 100
	case limit > 500:
		return 500
	default:
		return limit
	}
}

// Threads returns the most recently updated threads (updated_at DESC, ties
// broken by id DESC), limit clamped via clampThreadsLimit. Each thread's
// Intent field is overridden with the effective intent (effectiveIntent), and
// LastFrom/LastAt/EntryCount are populated from its last entry — identified
// by MAX(entries.id) (append order), never MAX(created_at), so two entries
// landing in the same millisecond never mis-pick the last one. The entry
// annotation aggregates only over the already-limited thread set (the
// "recent" CTE is computed first, entries join against it), never the full
// entries table — this runs on a polling cadence (station, once a second).
func (s *Store) Threads(limit int) ([]Thread, error) {
	limit = clampThreadsLimit(limit)
	rows, err := s.db.Query(`
WITH recent AS (
    SELECT *, `+effectiveIntent+` AS eff_intent
    FROM threads
    ORDER BY updated_at DESC, id DESC
    LIMIT ?
),
last AS (
    SELECT e.thread_id, MAX(e.id) AS max_id, COUNT(*) AS n
    FROM entries e
    WHERE e.thread_id IN (SELECT id FROM recent)
    GROUP BY e.thread_id
)
SELECT recent.id, recent.kind, recent.from_agent, recent.to_kind, recent.to_target,
       recent.subject, recent.ref, recent.status, recent.eff_intent,
       recent.created_at, recent.updated_at,
       le.from_agent, le.created_at, last.n
FROM recent
JOIN last ON last.thread_id = recent.id
JOIN entries le ON le.id = last.max_id
ORDER BY recent.updated_at DESC, recent.id DESC`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Thread
	for rows.Next() {
		var t Thread
		var status sql.NullString
		if err := rows.Scan(&t.ID, &t.Kind, &t.FromAgent, &t.ToKind, &t.ToTarget,
			&t.Subject, &t.Ref, &status, &t.Intent,
			&t.CreatedAt, &t.UpdatedAt,
			&t.LastFrom, &t.LastAt, &t.EntryCount); err != nil {
			return nil, err
		}
		if status.Valid {
			t.Status = status.String
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
