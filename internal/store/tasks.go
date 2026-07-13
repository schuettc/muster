package store

import (
	"errors"
	"fmt"

	"github.com/schuettc/muster/internal/clock"
)

// ErrNotClaimable is returned when claiming a task that is not open.
var ErrNotClaimable = errors.New("task not claimable")

// TaskStates is the set of valid task statuses.
var TaskStates = map[string]bool{
	"open": true, "claimed": true, "needs_info": true, "blocked": true,
	"completed": true, "declined": true, "cancelled": true,
}

// ClaimTask atomically moves a task from open → claimed and records it.
func (s *Store) ClaimTask(threadID int64, byAgent string) error {
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`UPDATE threads SET status='claimed', updated_at=? WHERE id=? AND status='open'`, now, threadID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotClaimable
	}
	if _, err := tx.Exec(`INSERT INTO entries (thread_id, from_agent, body, status_change, created_at) VALUES (?, ?, '', 'claimed', ?)`, threadID, byAgent, now); err != nil {
		return err
	}
	return tx.Commit()
}

// TransitionTask sets a new (validated) status and records the change as an entry.
func (s *Store) TransitionTask(threadID int64, byAgent, newStatus, note string) error {
	if !TaskStates[newStatus] {
		return fmt.Errorf("invalid task status %q", newStatus)
	}
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`UPDATE threads SET status=?, updated_at=? WHERE id=?`, newStatus, now, threadID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrThreadNotFound
	}
	if _, err := tx.Exec(`INSERT INTO entries (thread_id, from_agent, body, status_change, created_at) VALUES (?, ?, ?, ?, ?)`, threadID, byAgent, note, newStatus, now); err != nil {
		return err
	}
	return tx.Commit()
}
