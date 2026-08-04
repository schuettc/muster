package store

import (
	"database/sql"
	"errors"

	"github.com/schuettc/muster/internal/clock"
)

// Idempotency record states. A record is created pending by the caller that
// wins the claim and moves to done when that caller records its response;
// there is no third state and no way back.
const (
	idemPending = "pending"
	idemDone    = "done"
)

// IdemBegin claims key for a first delivery.
//
// found=false means this caller owns execution and MUST call IdemComplete when
// the op finishes. found=true with done=true means the op already ran and resp
// is its recorded response, which the caller replays verbatim. found=true with
// done=false means an identical request is in flight — the caller neither
// executes nor replays; it tells the client to retry.
//
// The claim is the INSERT itself, not a read-then-write: ON CONFLICT DO NOTHING
// makes "did I create the row" a RowsAffected question that SQLite answers
// atomically, so N concurrent callers on one key produce exactly one
// found=false. Reading first and inserting second would hand every caller in
// the gap a claim, which is precisely the double-execution this record exists
// to prevent.
//
// A record that vanishes between the failed insert and the read (nothing in
// this backend deletes one, but the DynamoDB backend expires them on a TTL)
// reports in-flight rather than claimable. Both backends answer that way: a
// retry then claims it cleanly, where reporting it claimable here would let two
// callers execute the same op.
func (s *Store) IdemBegin(key string) (resp []byte, done bool, found bool, err error) {
	res, err := s.db.Exec(`
INSERT INTO idem (key, state, resp, created_at) VALUES (?, ?, NULL, ?)
ON CONFLICT(key) DO NOTHING`, key, idemPending, clock.NowMillis())
	if err != nil {
		return nil, false, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, false, err
	}
	if n == 1 {
		return nil, false, false, nil
	}

	var state string
	var stored []byte
	err = s.db.QueryRow(`SELECT state, resp FROM idem WHERE key=?`, key).Scan(&state, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, true, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	return stored, state == idemDone, true, nil
}

// IdemComplete records resp as key's response and marks the record done, so a
// redelivery replays it. An unknown key is a no-op: only the caller that won
// the claim may complete it, and the UPDATE simply matches no rows otherwise.
func (s *Store) IdemComplete(key string, resp []byte) error {
	_, err := s.db.Exec(`UPDATE idem SET state=?, resp=? WHERE key=?`, idemDone, resp, key)
	return err
}
