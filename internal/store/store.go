// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Sentinel errors shared by the service. They map one-to-one onto the public
// error codes documented in README.md.
var (
	// ErrJobNotFound is returned when a job ID does not identify a live job.
	ErrJobNotFound = errors.New("job not found")
	// ErrRunNotFound is returned when a run record ID does not identify a run.
	ErrRunNotFound = errors.New("run record not found")
	// ErrRetryNotRetryable is returned when a retry targets a non-original run
	// or a run that already reported success.
	ErrRetryNotRetryable = errors.New("run cannot be retried")
)

const (
	// RecordOriginal identifies the first execution of a planned/manual trigger.
	RecordOriginal = "original"
	// RecordRetry identifies a retry of an original execution.
	RecordRetry = "retry"

	// TriggerScheduled identifies a run produced by the scheduler entry.
	TriggerScheduled = "scheduled"
	// TriggerManual identifies a run produced by the manual entry.
	TriggerManual = "manual"

	// ResultSuccess and ResultFailure are the only accepted execution results.
	ResultSuccess = "success"
	ResultFailure = "failure"
)

// Job is the current scheduling definition of a task. Every time field is a
// canonical UTC RFC3339Nano string ending in "Z".
type Job struct {
	ID         string
	Name       string
	Expression string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// RunRecord is one independent execution attempt: either the original attempt
// for a planned/manual trigger or one retry of a failed original attempt.
type RunRecord struct {
	Seq              int64
	JobID            string
	JobName          string
	Expression       string
	RecordType       string
	TriggerType      string
	RetryOfSeq       sql.NullInt64
	RetryNumber      int
	RetryCount       int
	ScheduledFor     time.Time
	TriggeredAt      time.Time
	StartedAt        time.Time
	FinishedAt       time.Time
	Result           string
	FailureReason    sql.NullString
	NextScheduledFor sql.NullTime
	CreatedAt        time.Time
}

// NewJob carries an accepted create/update request.
type NewJob struct {
	Name       string
	Expression string
}

// NewRun carries the outcome of one execution attempt.
type NewRun struct {
	JobID           string
	RecordType      string
	TriggerType     string
	RetryOfSeq      int64
	RetryNumber     int
	ScheduledFor    time.Time
	TriggeredAt     time.Time
	StartedAt       time.Time
	FinishedAt      time.Time
	Result          string
	FailureReason   string
	NextScheduledAt time.Time
}

// PendingItem is a planned trigger inside the query window that has not yet
// produced an original execution record.
type PendingItem struct {
	JobID        string
	JobName      string
	Expression   string
	ScheduledFor time.Time
}

// WindowResult groups the two views returned by a time-window query.
type WindowResult struct {
	Pending  []PendingItem
	Executed []RunRecord
}

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
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

// DB exposes the raw handle for tests that need to simulate storage faults.
func (s *Store) DB() *sql.DB { return s.db }

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	expression TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS run_records (
	seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id              TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	job_name            TEXT NOT NULL,
	expression          TEXT NOT NULL,
	record_type         TEXT NOT NULL,
	trigger_type        TEXT NOT NULL,
	retry_of_seq        INTEGER,
	retry_number        INTEGER NOT NULL DEFAULT 0,
	retry_count         INTEGER NOT NULL DEFAULT 0,
	scheduled_for       TEXT NOT NULL,
	triggered_at        TEXT NOT NULL,
	started_at          TEXT NOT NULL,
	finished_at         TEXT NOT NULL,
	result              TEXT NOT NULL,
	failure_reason      TEXT,
	next_scheduled_for  TEXT,
	created_at          TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_runs_scheduled ON run_records(scheduled_for, seq);
CREATE INDEX IF NOT EXISTS idx_runs_original ON run_records(job_id, scheduled_for) WHERE record_type = 'original';
`
