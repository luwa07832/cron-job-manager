// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound reports that no active job matches the given identifier.
var ErrNotFound = errors.New("store: job not found")

// ErrNameConflict reports that an active job already uses the name.
var ErrNameConflict = errors.New("store: job name already in use")

// ErrNextRunConflict reports that the stored scheduling cursor no longer
// matches the value the caller read, so the advance was refused.
var ErrNextRunConflict = errors.New("store: next_run no longer matches")

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Job is one scheduled task as stored. Time fields are UTC and NextRun is nil
// while the job is disabled or has no reachable schedule.
type Job struct {
	ID         string
	Name       string
	Expression string
	Timezone   string
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
	NextRun    *time.Time
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// SQLite admits a single writer; one connection serializes every
	// statement so concurrent writes queue instead of failing with
	// SQLITE_BUSY, which keeps compare-and-swap updates race-safe.
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// CreateJob persists a new job and returns its stored state.
func (s *Store) CreateJob(job *Job) (*Job, error) {
	var nextRun any
	if job.NextRun != nil {
		nextRun = job.NextRun.UnixNano()
	}
	_, err := s.db.Exec(
		`INSERT INTO jobs (id, name, expression, timezone, enabled, created_at, updated_at, next_run)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.Expression, job.Timezone, job.Enabled,
		job.CreatedAt.UnixNano(), job.UpdatedAt.UnixNano(), nextRun,
	)
	if err != nil {
		return nil, mapWriteError(err)
	}
	return job, nil
}

// GetJob fetches one active job. It returns ErrNotFound when the job is
// missing or was deleted.
func (s *Store) GetJob(id string) (*Job, error) {
	return s.queryJob(
		`SELECT id, name, expression, timezone, enabled, created_at, updated_at, next_run
		 FROM jobs WHERE id = ? AND deleted_at IS NULL`,
		id,
	)
}

// UpdateJob replaces the mutable fields of an active job. It returns
// ErrNotFound when the job has been removed concurrently and ErrNameConflict
// when another active job already owns the name.
func (s *Store) UpdateJob(job *Job) (*Job, error) {
	var nextRun any
	if job.NextRun != nil {
		nextRun = job.NextRun.UnixNano()
	}
	result, err := s.db.Exec(
		`UPDATE jobs
		    SET name = ?, expression = ?, timezone = ?, enabled = ?, updated_at = ?, next_run = ?
		  WHERE id = ? AND deleted_at IS NULL`,
		job.Name, job.Expression, job.Timezone, job.Enabled,
		job.UpdatedAt.UnixNano(), nextRun, job.ID,
	)
	if err != nil {
		return nil, mapWriteError(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, ErrNotFound
	}
	return job, nil
}

// DeleteJob soft-deletes an active job. It returns ErrNotFound when the job
// does not exist or was already deleted.
func (s *Store) DeleteJob(id string, deletedAt time.Time) error {
	result, err := s.db.Exec(
		`UPDATE jobs SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		deletedAt.UnixNano(), id,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AdvanceNextRun atomically moves the scheduling cursor of an enabled,
// active job from expected to next, bumping updated_at. It returns
// ErrNotFound when the job is missing, deleted or disabled and
// ErrNextRunConflict when another writer moved the cursor first.
func (s *Store) AdvanceNextRun(id string, expected time.Time, next *time.Time, updatedAt time.Time) error {
	var nextNS any
	if next != nil {
		nextNS = next.UnixNano()
	}
	result, err := s.db.Exec(
		`UPDATE jobs
		    SET next_run = ?, updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL AND enabled = 1 AND next_run = ?`,
		nextNS, updatedAt.UnixNano(), id, expected.UnixNano(),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		return nil
	}
	var enabled int
	err = s.db.QueryRow(
		`SELECT enabled FROM jobs WHERE id = ? AND deleted_at IS NULL`, id,
	).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if enabled != 1 {
		return ErrNotFound
	}
	return ErrNextRunConflict
}

// PendingJobs returns enabled, active jobs whose next firing instant falls in
// [from, to), ordered by next firing instant.
func (s *Store) PendingJobs(from, to time.Time) ([]*Job, error) {
	rows, err := s.db.Query(
		`SELECT id, name, expression, timezone, enabled, created_at, updated_at, next_run
		   FROM jobs
		  WHERE deleted_at IS NULL AND enabled = 1
		    AND next_run IS NOT NULL
		    AND next_run >= ? AND next_run < ?
		  ORDER BY next_run ASC, created_at ASC, id ASC`,
		from.UnixNano(), to.UnixNano(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return jobs, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

// sqlQueryer is satisfied by both *sql.DB and *sql.Tx.
type sqlQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func (s *Store) queryJob(query string, args ...any) (*Job, error) {
	row := s.db.QueryRow(query, args...)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return job, nil
}

func scanJob(scanner rowScanner) (*Job, error) {
	job := &Job{}
	var enabled int
	var createdNS, updatedNS int64
	var nextNS sql.NullInt64
	if err := scanner.Scan(
		&job.ID, &job.Name, &job.Expression, &job.Timezone, &enabled,
		&createdNS, &updatedNS, &nextNS,
	); err != nil {
		return nil, err
	}
	job.Enabled = enabled == 1
	job.CreatedAt = time.Unix(0, createdNS).UTC()
	job.UpdatedAt = time.Unix(0, updatedNS).UTC()
	if nextNS.Valid {
		instant := time.Unix(0, nextNS.Int64).UTC()
		job.NextRun = &instant
	}
	return job, nil
}

func mapWriteError(err error) error {
	if isUniqueConstraint(err) {
		return ErrNameConflict
	}
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	expression TEXT NOT NULL,
	timezone   TEXT NOT NULL,
	enabled    INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	next_run   INTEGER,
	deleted_at INTEGER
);

CREATE UNIQUE INDEX IF NOT EXISTS jobs_active_name_key
	ON jobs (name) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS jobs_pending_idx
	ON jobs (next_run)
	WHERE deleted_at IS NULL AND enabled = 1 AND next_run IS NOT NULL;

CREATE TABLE IF NOT EXISTS run_attempts (
	job_id        TEXT NOT NULL,
	run_id        TEXT NOT NULL,
	attempt       INTEGER NOT NULL,
	scheduled_for INTEGER NOT NULL,
	started_at    INTEGER NOT NULL,
	finished_at   INTEGER NOT NULL,
	outcome       TEXT NOT NULL,
	error         TEXT,
	PRIMARY KEY (job_id, run_id, attempt)
);

CREATE INDEX IF NOT EXISTS run_attempts_run_idx
	ON run_attempts (job_id, run_id, attempt);

CREATE INDEX IF NOT EXISTS run_attempts_scheduled_idx
	ON run_attempts (scheduled_for);

CREATE TABLE IF NOT EXISTS run_idempotency_keys (
	job_id              TEXT NOT NULL,
	idempotency_key     TEXT NOT NULL,
	run_id              TEXT NOT NULL,
	request_fingerprint TEXT NOT NULL,
	result_attempt_count INTEGER NOT NULL,
	created_at          INTEGER NOT NULL,
	PRIMARY KEY (job_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS retry_idempotency_keys (
	job_id              TEXT NOT NULL,
	run_id              TEXT NOT NULL,
	idempotency_key     TEXT NOT NULL,
	request_fingerprint TEXT NOT NULL,
	result_attempt_count INTEGER NOT NULL,
	created_at          INTEGER NOT NULL,
	PRIMARY KEY (job_id, run_id, idempotency_key)
);
`
