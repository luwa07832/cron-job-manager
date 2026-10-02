package store

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNextRunConflict reports that the stored next_run no longer matches the
// instant the caller based its dispatch on, so another caller won the
// cursor race.
var ErrNextRunConflict = errors.New("store: next_run conflict")

// AdvanceNextRun moves the scheduling cursor of one enabled, active job from
// expected to next in a single conditional UPDATE. It returns ErrNotFound
// when the job is missing, soft-deleted or disabled and ErrNextRunConflict
// when the stored cursor differs from expected; in both cases no row changes.
func (s *Store) AdvanceNextRun(jobID string, expected, next, updatedAt time.Time) error {
	result, err := s.db.Exec(
		`UPDATE jobs
		    SET next_run = ?, updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL AND enabled = 1 AND next_run = ?`,
		next.UnixNano(), updatedAt.UnixNano(), jobID, expected.UnixNano(),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}

	var enabled int
	var deletedNS sql.NullInt64
	var currentNS sql.NullInt64
	lookupErr := s.db.QueryRow(
		`SELECT enabled, deleted_at, next_run FROM jobs WHERE id = ?`, jobID,
	).Scan(&enabled, &deletedNS, &currentNS)
	if errors.Is(lookupErr, sql.ErrNoRows) || deletedNS.Valid || enabled != 1 {
		return ErrNotFound
	}
	if lookupErr != nil {
		return lookupErr
	}
	return ErrNextRunConflict
}
