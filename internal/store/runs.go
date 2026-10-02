package store

import (
	"database/sql"
	"errors"
	"time"
)

// Outcome values stored on run attempts.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
)

// ErrRunNotFound reports that no run matches the given identifier.
var ErrRunNotFound = errors.New("store: run not found")

// ErrRetryNotAllowed reports that a run's latest attempt did not fail.
var ErrRetryNotAllowed = errors.New("store: retry is not allowed")

// Run groups the attempts recorded for one scheduled execution of a job.
type Run struct {
	ID           string
	JobID        string
	ScheduledFor time.Time
	Attempts     []*RunAttempt
}

// RunAttempt is one recorded outcome: attempt 1 is the original execution and
// later attempts are retries appended in order.
type RunAttempt struct {
	RunID        string
	JobID        string
	Attempt      int
	ScheduledFor time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	Outcome      string
	Error        *string
}

// NewRunInput carries a validated original execution.
type NewRunInput struct {
	RunID        string
	JobID        string
	ScheduledFor time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	Outcome      string
	Error        *string
}

// CreateRun records the first attempt of a new run. The job must exist and
// remain active; soft-deleted jobs reject new records with ErrNotFound.
func (s *Store) CreateRun(input NewRunInput) (*Run, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := requireActiveJob(tx, input.JobID); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`INSERT INTO runs (id, job_id, scheduled_for) VALUES (?, ?, ?)`,
		input.RunID, input.JobID, input.ScheduledFor.UnixNano(),
	); err != nil {
		return nil, mapWriteError(err)
	}
	if _, err := tx.Exec(
		`INSERT INTO run_attempts (run_id, job_id, attempt, started_at, finished_at, outcome, error)
		 VALUES (?, ?, 1, ?, ?, ?, ?)`,
		input.RunID, input.JobID,
		input.StartedAt.UnixNano(), input.FinishedAt.UnixNano(),
		input.Outcome, nullableText(input.Error),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetRun(input.JobID, input.RunID)
}

// AddRetry appends another attempt to an existing run. Only runs whose latest
// attempt failed accept retries; otherwise ErrRetryNotAllowed is returned.
// Soft-deleted jobs keep their history but reject new attempts with
// ErrNotFound.
func (s *Store) AddRetry(jobID, runID string, startedAt, finishedAt time.Time, outcome string, message *string) (*Run, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := requireActiveJob(tx, jobID); err != nil {
		return nil, err
	}

	var scheduledNS int64
	err = tx.QueryRow(
		`SELECT scheduled_for FROM runs WHERE id = ? AND job_id = ?`, runID, jobID,
	).Scan(&scheduledNS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}

	var latestAttempt int
	var latestOutcome string
	err = tx.QueryRow(
		`SELECT attempt, outcome FROM run_attempts WHERE run_id = ? ORDER BY attempt DESC LIMIT 1`,
		runID,
	).Scan(&latestAttempt, &latestOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if latestOutcome != OutcomeFailed {
		return nil, ErrRetryNotAllowed
	}

	if _, err := tx.Exec(
		`INSERT INTO run_attempts (run_id, job_id, attempt, started_at, finished_at, outcome, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		runID, jobID, latestAttempt+1,
		startedAt.UnixNano(), finishedAt.UnixNano(), outcome, nullableText(message),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetRun(jobID, runID)
}

// GetRun loads one run with every attempt ordered by attempt number. The run
// stays readable after its job is soft-deleted. Missing jobs report
// ErrNotFound; runs outside the job's history report ErrRunNotFound.
func (s *Store) GetRun(jobID, runID string) (*Run, error) {
	var scheduledNS int64
	err := s.db.QueryRow(
		`SELECT scheduled_for FROM runs WHERE id = ? AND job_id = ?`, runID, jobID,
	).Scan(&scheduledNS)
	if errors.Is(err, sql.ErrNoRows) {
		if s.jobExists(jobID) {
			return nil, ErrRunNotFound
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT run_id, job_id, attempt, started_at, finished_at, outcome, error
		   FROM run_attempts WHERE run_id = ? ORDER BY attempt ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	run := &Run{ID: runID, JobID: jobID, ScheduledFor: time.Unix(0, scheduledNS).UTC()}
	for rows.Next() {
		attempt, err := scanRunAttempt(rows, scheduledNS)
		if err != nil {
			return nil, err
		}
		run.Attempts = append(run.Attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return run, nil
}

// ExecutedRuns returns every attempt of runs scheduled in [from, to),
// including attempts of soft-deleted jobs. Rows are ordered by
// scheduled_for, job_id, run_id and attempt so a run's attempts stay grouped.
func (s *Store) ExecutedRuns(from, to time.Time) ([]*RunAttempt, error) {
	rows, err := s.db.Query(
		`SELECT a.run_id, a.job_id, a.attempt, r.scheduled_for,
		        a.started_at, a.finished_at, a.outcome, a.error
		   FROM run_attempts a
		   JOIN runs r ON r.id = a.run_id
		  WHERE r.scheduled_for >= ? AND r.scheduled_for < ?
		  ORDER BY r.scheduled_for ASC, a.job_id ASC, a.run_id ASC, a.attempt ASC`,
		from.UnixNano(), to.UnixNano(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attempts []*RunAttempt
	for rows.Next() {
		attempt := &RunAttempt{}
		var scheduledNS, startedNS, finishedNS int64
		var runError sql.NullString
		if err := rows.Scan(
			&attempt.RunID, &attempt.JobID, &attempt.Attempt, &scheduledNS,
			&startedNS, &finishedNS, &attempt.Outcome, &runError,
		); err != nil {
			return nil, err
		}
		attempt.ScheduledFor = time.Unix(0, scheduledNS).UTC()
		attempt.StartedAt = time.Unix(0, startedNS).UTC()
		attempt.FinishedAt = time.Unix(0, finishedNS).UTC()
		if runError.Valid {
			message := runError.String
			attempt.Error = &message
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return attempts, nil
}

type txQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// requireActiveJob returns ErrNotFound for missing or soft-deleted jobs.
func requireActiveJob(querier txQuerier, jobID string) error {
	var deleted sql.NullInt64
	err := querier.QueryRow(`SELECT deleted_at FROM jobs WHERE id = ?`, jobID).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if deleted.Valid {
		return ErrNotFound
	}
	return nil
}

func (s *Store) jobExists(jobID string) bool {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE id = ?`, jobID).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func nullableText(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func scanRunAttempt(rows *sql.Rows, scheduledNS int64) (*RunAttempt, error) {
	attempt := &RunAttempt{}
	var startedNS, finishedNS int64
	var runError sql.NullString
	if err := rows.Scan(
		&attempt.RunID, &attempt.JobID, &attempt.Attempt,
		&startedNS, &finishedNS, &attempt.Outcome, &runError,
	); err != nil {
		return nil, err
	}
	attempt.ScheduledFor = time.Unix(0, scheduledNS).UTC()
	attempt.StartedAt = time.Unix(0, startedNS).UTC()
	attempt.FinishedAt = time.Unix(0, finishedNS).UTC()
	if runError.Valid {
		message := runError.String
		attempt.Error = &message
	}
	return attempt, nil
}
