package store

import (
	"database/sql"
	"errors"
	"time"
)

// ErrIdempotencyConflict reports that an idempotency key was already stored
// with request semantics that differ from the current request. No run data is
// written when it is returned.
var ErrIdempotencyConflict = errors.New("store: idempotency key reused with conflicting request")

// CreateRunIdempotent records the first attempt of a run while reserving the
// idempotency key for the job. A replay carrying the same key and request
// fingerprint writes nothing and returns the run exactly as first observed; a
// reused key with a different fingerprint returns ErrIdempotencyConflict. The
// whole check and write is one serialized transaction, so concurrent replays
// can create the run only once.
func (s *Store) CreateRunIdempotent(attempt *Attempt, key, fingerprint string) (*Run, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var storedRunID, storedFingerprint string
	var storedCount int
	err = tx.QueryRow(
		`SELECT run_id, request_fingerprint, result_attempt_count FROM run_idempotency_keys
		  WHERE job_id = ? AND idempotency_key = ?`,
		attempt.JobID, key,
	).Scan(&storedRunID, &storedFingerprint, &storedCount)
	switch {
	case err == nil:
		if storedFingerprint != fingerprint {
			return nil, ErrIdempotencyConflict
		}
		return readRunThrough(tx, attempt.JobID, storedRunID, storedCount)
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	if _, err := tx.Exec(
		`INSERT INTO run_idempotency_keys
		    (job_id, idempotency_key, run_id, request_fingerprint, result_attempt_count, created_at)
		 VALUES (?, ?, ?, ?, 1, ?)`,
		attempt.JobID, key, attempt.RunID, fingerprint, time.Now().UTC().UnixNano(),
	); err != nil {
		if isUniqueConstraint(err) {
			tx.Rollback()
			return s.replayCreate(attempt.JobID, key, fingerprint)
		}
		return nil, err
	}

	if _, err := tx.Exec(
		`INSERT INTO run_attempts
		    (job_id, run_id, attempt, scheduled_for, started_at, finished_at, outcome, error)
		 VALUES (?, ?, 1, ?, ?, ?, ?, ?)`,
		attempt.JobID, attempt.RunID,
		attempt.ScheduledFor.UnixNano(), attempt.StartedAt.UnixNano(),
		attempt.FinishedAt.UnixNano(), attempt.Outcome, nullableString(attempt.Error),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Run{
		JobID:        attempt.JobID,
		RunID:        attempt.RunID,
		ScheduledFor: attempt.ScheduledFor.UTC(),
		Attempts:     []*Attempt{attempt},
	}, nil
}

// replayCreate re-reads an existing create-key reservation after a concurrent
// writer won the unique-index race, on the committed database.
func (s *Store) replayCreate(jobID, key, fingerprint string) (*Run, error) {
	var storedRunID, storedFingerprint string
	var storedCount int
	err := s.db.QueryRow(
		`SELECT run_id, request_fingerprint, result_attempt_count FROM run_idempotency_keys
		  WHERE job_id = ? AND idempotency_key = ?`,
		jobID, key,
	).Scan(&storedRunID, &storedFingerprint, &storedCount)
	if err != nil {
		return nil, err
	}
	if storedFingerprint != fingerprint {
		return nil, ErrIdempotencyConflict
	}
	return readRunThrough(s.db, jobID, storedRunID, storedCount)
}

// AppendRetryIdempotent appends another attempt while reserving the retry key
// for the given run. It applies the same replay and conflict rules as
// CreateRunIdempotent, additionally returning ErrRunNotFound for an unknown
// run and ErrRetryNotAllowed when the latest attempt did not fail. The run is
// read and the attempt appended inside one transaction, so a matching replay
// of a request whose retry is now blocked still returns the stored response
// instead of ErrRetryNotAllowed.
func (s *Store) AppendRetryIdempotent(jobID, runID, key, fingerprint string, startedAt, finishedAt time.Time, outcome string, failure *string) (*Run, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var storedFingerprint string
	var storedCount int
	err = tx.QueryRow(
		`SELECT request_fingerprint, result_attempt_count FROM retry_idempotency_keys
		  WHERE job_id = ? AND run_id = ? AND idempotency_key = ?`,
		jobID, runID, key,
	).Scan(&storedFingerprint, &storedCount)
	switch {
	case err == nil:
		if storedFingerprint != fingerprint {
			return nil, ErrIdempotencyConflict
		}
		return readRunThrough(tx, jobID, runID, storedCount)
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	var scheduledNS int64
	var lastOutcome string
	var maxAttempt int
	err = tx.QueryRow(
		`SELECT scheduled_for, attempt, outcome
		   FROM run_attempts
		  WHERE job_id = ? AND run_id = ?
		  ORDER BY attempt DESC
		  LIMIT 1`,
		jobID, runID,
	).Scan(&scheduledNS, &maxAttempt, &lastOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if lastOutcome != "failed" {
		return nil, ErrRetryNotAllowed
	}

	if _, err := tx.Exec(
		`INSERT INTO retry_idempotency_keys
		    (job_id, run_id, idempotency_key, request_fingerprint, result_attempt_count, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		jobID, runID, key, fingerprint, maxAttempt+1, time.Now().UTC().UnixNano(),
	); err != nil {
		if isUniqueConstraint(err) {
			tx.Rollback()
			return s.replayRetry(jobID, runID, key, fingerprint)
		}
		return nil, err
	}

	attempt := &Attempt{
		JobID:        jobID,
		RunID:        runID,
		Attempt:      maxAttempt + 1,
		ScheduledFor: time.Unix(0, scheduledNS).UTC(),
		StartedAt:    startedAt.UTC(),
		FinishedAt:   finishedAt.UTC(),
		Outcome:      outcome,
		Error:        failure,
	}
	if _, err := tx.Exec(
		`INSERT INTO run_attempts
		    (job_id, run_id, attempt, scheduled_for, started_at, finished_at, outcome, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.JobID, attempt.RunID, attempt.Attempt,
		attempt.ScheduledFor.UnixNano(), attempt.StartedAt.UnixNano(),
		attempt.FinishedAt.UnixNano(), attempt.Outcome, nullableString(attempt.Error),
	); err != nil {
		return nil, err
	}

	run, err := readRunThrough(tx, jobID, runID, maxAttempt+1)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

// replayRetry re-reads an existing retry-key reservation after a concurrent
// writer won the unique-index race, on the committed database.
func (s *Store) replayRetry(jobID, runID, key, fingerprint string) (*Run, error) {
	var storedFingerprint string
	var storedCount int
	err := s.db.QueryRow(
		`SELECT request_fingerprint, result_attempt_count FROM retry_idempotency_keys
		  WHERE job_id = ? AND run_id = ? AND idempotency_key = ?`,
		jobID, runID, key,
	).Scan(&storedFingerprint, &storedCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if storedFingerprint != fingerprint {
		return nil, ErrIdempotencyConflict
	}
	return readRunThrough(s.db, jobID, runID, storedCount)
}

// readRunThrough loads at most maxResults attempts of a run in ascending
// order through the given querier, so it works both inside a transaction and
// on the pool. It returns ErrRunNotFound when the run has no attempts.
func readRunThrough(q sqlQueryer, jobID, runID string, maxResults int) (*Run, error) {
	rows, err := q.Query(
		`SELECT job_id, run_id, attempt, scheduled_for, started_at, finished_at, outcome, error
		   FROM run_attempts
		  WHERE job_id = ? AND run_id = ?
		  ORDER BY attempt ASC
		  LIMIT ?`,
		jobID, runID, maxResults,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	run, err := scanRun(rows)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	return run, nil
}
