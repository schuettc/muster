package store

// SessionRef identifies one tmux session on one device: the pair a wake is
// addressed to once the device is already known (DevicePoll only ever returns
// sessions belonging to the device that asked, so repeating the device id on
// every row would be noise).
type SessionRef struct {
	SocketPath string `json:"socket_path"`
	SessionID  string `json:"session_id"`
}

// DevicePollResult is what a device needs to know after a poll: which of its
// sessions have new mail, and the watermark to resume from.
//
// MaxEntryID is the highest entry id the poll CONSIDERED, not the highest that
// concerned the device — mail for somebody else still moves it. Otherwise a
// busy bus would re-read the same entries on every tick and the poll would
// never go quiet.
type DevicePollResult struct {
	MaxEntryID int64        `json:"max_entry_id"`
	Sessions   []SessionRef `json:"sessions"`
}

// DevicePoll answers "which of this device's sessions need their badge
// recomputed, and where do I resume from" in one round trip.
//
// The SERVER does the filtering because the server is the only place that
// holds both halves — the roster and the entries. A device that fetched
// entries and filtered them itself would be shipping the whole bus's traffic
// to every laptop on it, and would need its own copy of the concern predicate,
// which is precisely the copy that drifts.
//
// "Concerns" is the full four-arm threadConcerns predicate (addressed to the
// agent, to its role, broadcast, or ORIGINATED by it) — the same predicate
// Inbox and UnreadCount use, reached here through threadConcernsJoin rather
// than restated. A poller matching only the recipient arm would leave a peer's
// reply on a thread the local agent started sitting unread in `muster inbox`
// with the pane dark.
//
// Entries authored by an agent ON this device still count: the reconcile they
// trigger is idempotent (it recomputes from stored state and writes the same
// badge), so special-casing them would add a branch that can be wrong in
// exchange for nothing.
//
// Only agents that could carry a badge are considered — live (not departed),
// with a non-empty tuple — matching what the daemon's ReconcileLocalSessions
// is willing to write.
func (s *Store) DevicePoll(deviceID string, sinceEntryID int64) (DevicePollResult, error) {
	out := DevicePollResult{MaxEntryID: sinceEntryID, Sessions: []SessionRef{}}

	// One read transaction so the watermark and the sessions come from the
	// same snapshot: computed separately, an entry landing in between would be
	// counted under the new watermark without ever having been examined.
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(id), ?1) FROM entries WHERE id > ?1`, sinceEntryID,
	).Scan(&out.MaxEntryID); err != nil {
		return DevicePollResult{MaxEntryID: sinceEntryID}, err
	}

	rows, err := tx.Query(`
WITH sess AS (SELECT alias, socket_path, session_id FROM agents
              WHERE device_id = ?1 AND departed = 0
                AND socket_path != '' AND session_id != '')
SELECT DISTINCT sess.socket_path, sess.session_id
FROM entries e
JOIN threads ON threads.id = e.thread_id
JOIN sess ON `+threadConcernsJoin+`
WHERE e.id > ?2
ORDER BY sess.socket_path, sess.session_id`, deviceID, sinceEntryID)
	if err != nil {
		return DevicePollResult{MaxEntryID: sinceEntryID}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref SessionRef
		if err := rows.Scan(&ref.SocketPath, &ref.SessionID); err != nil {
			return DevicePollResult{MaxEntryID: sinceEntryID}, err
		}
		out.Sessions = append(out.Sessions, ref)
	}
	if err := rows.Err(); err != nil {
		return DevicePollResult{MaxEntryID: sinceEntryID}, err
	}
	return out, nil
}
