// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	_ "modernc.org/sqlite"
)

// ErrNotFound reports that no job exists for the requested identifier.
var ErrNotFound = errors.New("job not found")

// ErrNameConflict reports that another live job already uses the name.
var ErrNameConflict = errors.New("job name already exists")

// Job is one scheduled task row. NextRun is nil while the job is disabled.
type Job struct {
	ID         string
	Name       string
	Expression string
	Timezone   string
	Enabled    bool
	CreatedAt  time.Time
	NextRun    *time.Time
}

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
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
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// CreateJob assigns an identifier and creation time, then inserts the job.
func (s *Store) CreateJob(job *Job) error {
	job.ID = uuid.NewString()
	job.CreatedAt = time.Now().UTC()

	_, err := s.db.Exec(
		`INSERT INTO jobs (id, name, expression, timezone, enabled, created_at, next_run)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.Expression, job.Timezone, job.Enabled,
		formatTime(job.CreatedAt), formatOptionalTime(job.NextRun),
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrNameConflict
		}
		return fmt.Errorf("create job: %w", err)
	}
	return nil
}

// GetJob loads one job by identifier.
func (s *Store) GetJob(id string) (*Job, error) {
	row := s.db.QueryRow(
		`SELECT id, name, expression, timezone, enabled, created_at, next_run
		 FROM jobs WHERE id = ?`, id,
	)
	return scanJob(row)
}

// UpdateJob persists every mutable field of an existing job.
func (s *Store) UpdateJob(job *Job) error {
	result, err := s.db.Exec(
		`UPDATE jobs
		 SET name = ?, expression = ?, timezone = ?, enabled = ?, next_run = ?
		 WHERE id = ?`,
		job.Name, job.Expression, job.Timezone, job.Enabled,
		formatOptionalTime(job.NextRun), job.ID,
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrNameConflict
		}
		return fmt.Errorf("update job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update job rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteJob removes one job by identifier.
func (s *Store) DeleteJob(id string) error {
	result, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete job rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPendingJobs returns enabled jobs whose next run falls in [from, to),
// ordered by next run ascending.
func (s *Store) ListPendingJobs(from, to time.Time) ([]*Job, error) {
	rows, err := s.db.Query(
		`SELECT id, name, expression, timezone, enabled, created_at, next_run
		 FROM jobs
		 WHERE enabled = 1 AND next_run IS NOT NULL AND next_run >= ? AND next_run < ?
		 ORDER BY next_run ASC, id ASC`,
		formatTime(from.UTC()), formatTime(to.UTC()),
	)
	if err != nil {
		return nil, fmt.Errorf("list pending jobs: %w", err)
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
		return nil, fmt.Errorf("iterate pending jobs: %w", err)
	}
	return jobs, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(scanner rowScanner) (*Job, error) {
	job := &Job{}
	var enabled int
	var createdAt string
	var nextRun sql.NullString
	if err := scanner.Scan(
		&job.ID, &job.Name, &job.Expression, &job.Timezone, &enabled, &createdAt, &nextRun,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan job: %w", err)
	}
	job.Enabled = enabled == 1
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	job.CreatedAt = created
	if nextRun.Valid {
		parsed, err := time.Parse(time.RFC3339, nextRun.String)
		if err != nil {
			return nil, fmt.Errorf("parse next_run: %w", err)
		}
		job.NextRun = &parsed
	}
	return job, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}

func formatOptionalTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func isUniqueConstraint(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	expression TEXT NOT NULL,
	timezone   TEXT NOT NULL,
	enabled    INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	next_run   TEXT
);

CREATE INDEX IF NOT EXISTS idx_jobs_next_run
	ON jobs (enabled, next_run)
	WHERE enabled = 1 AND next_run IS NOT NULL;
`
