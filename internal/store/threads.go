package store

import (
	"database/sql"
	"errors"

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

// CreateThread inserts a thread and its first entry atomically.
func (s *Store) CreateThread(t Thread, firstBody string) (int64, error) {
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`
INSERT INTO threads (kind, from_agent, to_kind, to_target, subject, ref, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Kind, t.FromAgent, t.ToKind, t.ToTarget, t.Subject, t.Ref, nullable(t.Status), now, now)
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
	err := row.Scan(&t.ID, &t.Kind, &t.FromAgent, &t.ToKind, &t.ToTarget, &t.Subject, &t.Ref, &status, &t.CreatedAt, &t.UpdatedAt)
	if status.Valid {
		t.Status = status.String
	}
	return t, err
}

const threadCols = `id, kind, from_agent, to_kind, to_target, subject, ref, status, created_at, updated_at`

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

// Inbox returns threads addressed to alias directly, to alias's role, or broadcast.
func (s *Store) Inbox(alias string) ([]Thread, error) {
	rows, err := s.db.Query(`
SELECT `+threadCols+` FROM threads
WHERE (to_kind='agent'     AND to_target=?)
   OR (to_kind='role'      AND to_target != '' AND to_target=(SELECT role FROM agents WHERE alias=?))
   OR (to_kind='broadcast')
ORDER BY updated_at DESC`, alias, alias)
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
