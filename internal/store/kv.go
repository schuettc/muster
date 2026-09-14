package store

import (
	"database/sql"
	"errors"

	"github.com/schuettc/muster/internal/clock"
)

// KVSet upserts a shared fact.
func (s *Store) KVSet(key, value, updatedBy string) error {
	_, err := s.db.Exec(`
INSERT INTO kv (key, value, updated_by, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		key, value, updatedBy, clock.NowMillis())
	return err
}

// KVList returns every pair whose key has the literal prefix, ordered by key.
func (s *Store) KVList(prefix string) ([]KVPair, error) {
	rows, err := s.db.Query(`
SELECT key, value, updated_by, updated_at
FROM kv
WHERE substr(key, 1, length(?)) = ?
ORDER BY key`, prefix, prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	pairs := []KVPair{}
	for rows.Next() {
		var pair KVPair
		if err := rows.Scan(&pair.Key, &pair.Value, &pair.UpdatedBy, &pair.UpdatedAt); err != nil {
			return nil, err
		}
		pairs = append(pairs, pair)
	}
	return pairs, rows.Err()
}

// KVDelete removes key and reports whether a pair existed.
func (s *Store) KVDelete(key string) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM kv WHERE key=?`, key)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

// KVGet returns the pair for key; ok is false if the key is absent.
func (s *Store) KVGet(key string) (KVPair, bool, error) {
	var p KVPair
	err := s.db.QueryRow(`SELECT key, value, updated_by, updated_at FROM kv WHERE key=?`, key).
		Scan(&p.Key, &p.Value, &p.UpdatedBy, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return KVPair{}, false, nil
	}
	if err != nil {
		return KVPair{}, false, err
	}
	return p, true, nil
}
