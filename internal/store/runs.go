package store

import (
	"database/sql"
	"errors"
	"time"
)

// ErrRunNotFound reports that no run matches the given identifiers. It is
// returned both when the job is unknown and when the job owns no such run.
var ErrRunNotFound = errors.New("store: run not found")

// ErrRetryNotAllowed reports that the latest attempt of a run did not fail, so
// no further retry may be appended.
var ErrRetryNotAllowed = errors.New("store: run retry not allowed")

// Attempt is one recorded execution attempt of a job. Time fields are UTC.
type Attempt struct {
	JobID        string
	RunID        string
	Attempt      int
	ScheduledFor time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	Outcome      string
	Error        *string
}

// Run is a scheduled execution together with all of its attempts.
type Run struct {
	JobID        string
	RunID        string
	ScheduledFor time.Time
	Attempts     []*Attempt
}

// JobExists reports whether an active (not soft-deleted) job owns the id.
func (s *Store) JobExists(id string) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT 1 FROM jobs WHERE id = ? AND deleted_at IS NULL`, id,
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// CreateRun records the first attempt of a run. The run identifier is chosen
// by the caller and attempt starts at 1.
func (s *Store) CreateRun(attempt *Attempt) error {
	_, err := s.db.Exec(
		`INSERT INTO run_attempts
		    (job_id, run_id, attempt, scheduled_for, started_at, finished_at, outcome, error)
		 VALUES (?, ?, 1, ?, ?, ?, ?, ?)`,
		attempt.JobID, attempt.RunID,
		attempt.ScheduledFor.UnixNano(), attempt.StartedAt.UnixNano(),
		attempt.FinishedAt.UnixNano(), attempt.Outcome, nullableString(attempt.Error),
	)
	return err
}

// AppendRetry adds another attempt to an existing run. It returns
// ErrRunNotFound when the run is unknown and ErrRetryNotAllowed when its most
// recent attempt is not failed. The attempt number and scheduled instant are
// taken from the stored run, not from the caller.
func (s *Store) AppendRetry(jobID, runID string, startedAt, finishedAt time.Time, outcome string, failure *string) (*Attempt, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var scheduledNS, lastStarted, lastFinished int64
	var lastOutcome string
	var lastError sql.NullString
	var maxAttempt int
	err = tx.QueryRow(
		`SELECT scheduled_for, attempt, started_at, finished_at, outcome, error
		   FROM run_attempts
		  WHERE job_id = ? AND run_id = ?
		  ORDER BY attempt DESC
		  LIMIT 1`,
		jobID, runID,
	).Scan(&scheduledNS, &maxAttempt, &lastStarted, &lastFinished, &lastOutcome, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if lastOutcome != "failed" {
		return nil, ErrRetryNotAllowed
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
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return attempt, nil
}

// GetRun fetches one run with all of its attempts ordered ascending. It
// returns ErrNotFound when the job is missing or deleted and ErrRunNotFound
// when the job owns no such run. Run history remains readable after the job
// is soft-deleted.
func (s *Store) GetRun(jobID, runID string) (*Run, error) {
	var jobExists int
	err := s.db.QueryRow(`SELECT 1 FROM jobs WHERE id = ?`, jobID).Scan(&jobExists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT job_id, run_id, attempt, scheduled_for, started_at, finished_at, outcome, error
		   FROM run_attempts
		  WHERE job_id = ? AND run_id = ?
		  ORDER BY attempt ASC`,
		jobID, runID,
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

// ExecutedAttempts returns every attempt whose scheduled instant falls in
// [from, to), ordered by scheduled instant, job and run identifiers and then
// attempt. History of soft-deleted jobs is included.
func (s *Store) ExecutedAttempts(from, to time.Time) ([]*Attempt, error) {
	rows, err := s.db.Query(
		`SELECT job_id, run_id, attempt, scheduled_for, started_at, finished_at, outcome, error
		   FROM run_attempts
		  WHERE scheduled_for >= ? AND scheduled_for < ?
		  ORDER BY scheduled_for ASC, job_id ASC, run_id ASC, attempt ASC`,
		from.UnixNano(), to.UnixNano(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attempts []*Attempt
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return attempts, nil
}

func scanRun(rows *sql.Rows) (*Run, error) {
	var run *Run
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		if run == nil {
			run = &Run{
				JobID:        attempt.JobID,
				RunID:        attempt.RunID,
				ScheduledFor: attempt.ScheduledFor,
			}
		}
		run.Attempts = append(run.Attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return run, nil
}

func scanAttempt(scanner rowScanner) (*Attempt, error) {
	attempt := &Attempt{}
	var scheduledNS, startedNS, finishedNS int64
	var failure sql.NullString
	if err := scanner.Scan(
		&attempt.JobID, &attempt.RunID, &attempt.Attempt,
		&scheduledNS, &startedNS, &finishedNS, &attempt.Outcome, &failure,
	); err != nil {
		return nil, err
	}
	attempt.ScheduledFor = time.Unix(0, scheduledNS).UTC()
	attempt.StartedAt = time.Unix(0, startedNS).UTC()
	attempt.FinishedAt = time.Unix(0, finishedNS).UTC()
	if failure.Valid {
		text := failure.String
		attempt.Error = &text
	}
	return attempt, nil
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
