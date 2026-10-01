// Package store owns the SQLite file and every write the service performs.
//
// Every timestamp is stored as Unix nanoseconds in UTC. No formatting happens
// here, so stored data never depends on the host zone.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned for unknown task or run identifiers.
var ErrNotFound = errors.New("store: not found")

// ErrDuplicateID is returned when a caller-supplied task id already exists.
var ErrDuplicateID = errors.New("store: duplicate id")

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Job is a scheduled task definition as persisted.
type Job struct {
	ID          string
	Name        string
	Schedule    string
	Action      string
	CreatedAtNs int64
	UpdatedAtNs int64
}

// RunStatus values stored in job_runs.status.
const (
	RunRunning = "running"
	RunSuccess = "success"
	RunFailure = "failure"
)

// Run is one execution record: an original execution or a retry attempt.
type Run struct {
	ID                string
	JobID             string
	JobNameSnapshot   string
	ScheduleSnapshot  string
	RecordType        string
	TriggerType       string
	Attempt           int
	OriginalRunID     string
	ParentRunID       sql.NullString
	PlannedFireTimeNs int64
	ActualFireTimeNs  int64
	NextFireTimeNs    sql.NullInt64
	StartedAtNs       sql.NullInt64
	FinishedAtNs      sql.NullInt64
	Status            string
	FailureReason     sql.NullString
	CreatedAtNs       int64
	Seq               int64
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
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

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	schedule      TEXT NOT NULL,
	action        TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS job_runs (
	id                  TEXT PRIMARY KEY,
	job_id              TEXT NOT NULL,
	job_name_snapshot   TEXT NOT NULL,
	schedule_snapshot   TEXT NOT NULL,
	record_type         TEXT NOT NULL,
	trigger_type        TEXT NOT NULL,
	attempt             INTEGER NOT NULL,
	original_run_id     TEXT NOT NULL,
	parent_run_id       TEXT,
	planned_fire_ns     INTEGER NOT NULL,
	actual_fire_ns      INTEGER NOT NULL,
	next_fire_ns        INTEGER,
	started_at_ns       INTEGER,
	finished_at_ns      INTEGER,
	status              TEXT NOT NULL,
	failure_reason      TEXT,
	created_at_ns       INTEGER NOT NULL,
	seq                 INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_job_runs_planned ON job_runs(planned_fire_ns);
CREATE INDEX IF NOT EXISTS idx_job_runs_job_slot ON job_runs(job_id, planned_fire_ns, record_type, trigger_type);
CREATE INDEX IF NOT EXISTS idx_job_runs_original ON job_runs(original_run_id);
`

// CreateJob persists a new task. It returns ErrDuplicateID when the id exists.
func (s *Store) CreateJob(job Job) error {
	_, err := s.db.Exec(
		`INSERT INTO jobs (id, name, schedule, action, created_at_ns, updated_at_ns)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.Schedule, job.Action, job.CreatedAtNs, job.UpdatedAtNs,
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrDuplicateID
		}
		return fmt.Errorf("create job: %w", err)
	}
	return nil
}

// UpdateJob overwrites the mutable fields of an existing task.
func (s *Store) UpdateJob(job Job) error {
	result, err := s.db.Exec(
		`UPDATE jobs SET name = ?, schedule = ?, action = ?, updated_at_ns = ? WHERE id = ?`,
		job.Name, job.Schedule, job.Action, job.UpdatedAtNs, job.ID,
	)
	if err != nil {
		return fmt.Errorf("update job: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteJob removes a task definition. Run records are retained as history.
func (s *Store) DeleteJob(id string) error {
	result, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// GetJob loads one task definition.
func (s *Store) GetJob(id string) (Job, error) {
	return s.queryJob(`SELECT id, name, schedule, action, created_at_ns, updated_at_ns FROM jobs WHERE id = ?`, id)
}

// ListJobs returns every task ordered by creation time.
func (s *Store) ListJobs() ([]Job, error) {
	rows, err := s.db.Query(
		`SELECT id, name, schedule, action, created_at_ns, updated_at_ns FROM jobs ORDER BY created_at_ns, id`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return jobs, nil
}

func (s *Store) queryJob(query string, args ...any) (Job, error) {
	row := s.db.QueryRow(query, args...)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	return job, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(scanner rowScanner) (Job, error) {
	var job Job
	if err := scanner.Scan(&job.ID, &job.Name, &job.Schedule, &job.Action, &job.CreatedAtNs, &job.UpdatedAtNs); err != nil {
		return Job{}, fmt.Errorf("scan job: %w", err)
	}
	return job, nil
}

// InsertRun stores a new execution record (possibly still running).
func (s *Store) InsertRun(run Run) error {
	_, err := s.db.Exec(
		`INSERT INTO job_runs (id, job_id, job_name_snapshot, schedule_snapshot, record_type,
			trigger_type, attempt, original_run_id, parent_run_id, planned_fire_ns, actual_fire_ns,
			next_fire_ns, started_at_ns, finished_at_ns, status, failure_reason, created_at_ns, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.JobID, run.JobNameSnapshot, run.ScheduleSnapshot, run.RecordType,
		run.TriggerType, run.Attempt, run.OriginalRunID, run.ParentRunID, run.PlannedFireTimeNs,
		run.ActualFireTimeNs, run.NextFireTimeNs, run.StartedAtNs, run.FinishedAtNs, run.Status,
		run.FailureReason, run.CreatedAtNs, run.Seq,
	)
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

// FinishRun writes the terminal outcome of an execution.
func (s *Store) FinishRun(id string, startedAtNs, finishedAtNs int64, status string, failureReason sql.NullString) error {
	result, err := s.db.Exec(
		`UPDATE job_runs SET started_at_ns = ?, finished_at_ns = ?, status = ?, failure_reason = ? WHERE id = ?`,
		startedAtNs, finishedAtNs, status, failureReason, id,
	)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// GetRun loads one execution record.
func (s *Store) GetRun(id string) (Run, error) {
	row := s.db.QueryRow(`SELECT `+runColumns+` FROM job_runs WHERE id = ?`, id)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	return run, nil
}

// RunsInWindow returns every terminal execution record whose planned fire time
// falls inside [fromNs, toNs], ordered by planned fire time then creation seq.
func (s *Store) RunsInWindow(fromNs, toNs int64) ([]Run, error) {
	rows, err := s.db.Query(
		`SELECT `+runColumns+` FROM job_runs
		 WHERE planned_fire_ns BETWEEN ? AND ? AND status IN (?, ?)
		 ORDER BY planned_fire_ns, seq`,
		fromNs, toNs, RunSuccess, RunFailure,
	)
	if err != nil {
		return nil, fmt.Errorf("query runs in window: %w", err)
	}
	defer rows.Close()
	return collectRuns(rows)
}

// OriginalScheduledSlots returns the set of planned fire nanoseconds for which
// the given job already has an original scheduled execution record.
func (s *Store) OriginalScheduledSlots(jobID string, fromNs, toNs int64) (map[int64]struct{}, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT planned_fire_ns FROM job_runs
		 WHERE job_id = ? AND record_type = 'original' AND trigger_type = 'scheduled'
		   AND planned_fire_ns BETWEEN ? AND ?`,
		jobID, fromNs, toNs,
	)
	if err != nil {
		return nil, fmt.Errorf("query scheduled slots: %w", err)
	}
	defer rows.Close()
	slots := make(map[int64]struct{})
	for rows.Next() {
		var planned int64
		if err := rows.Scan(&planned); err != nil {
			return nil, fmt.Errorf("scan scheduled slot: %w", err)
		}
		slots[planned] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query scheduled slots: %w", err)
	}
	return slots, nil
}

const runColumns = `id, job_id, job_name_snapshot, schedule_snapshot, record_type, trigger_type,
	attempt, original_run_id, parent_run_id, planned_fire_ns, actual_fire_ns, next_fire_ns,
	started_at_ns, finished_at_ns, status, failure_reason, created_at_ns, seq`

func collectRuns(rows *sql.Rows) ([]Run, error) {
	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan runs: %w", err)
	}
	return runs, nil
}

func scanRun(scanner rowScanner) (Run, error) {
	var run Run
	err := scanner.Scan(
		&run.ID, &run.JobID, &run.JobNameSnapshot, &run.ScheduleSnapshot, &run.RecordType,
		&run.TriggerType, &run.Attempt, &run.OriginalRunID, &run.ParentRunID, &run.PlannedFireTimeNs,
		&run.ActualFireTimeNs, &run.NextFireTimeNs, &run.StartedAtNs, &run.FinishedAtNs, &run.Status,
		&run.FailureReason, &run.CreatedAtNs, &run.Seq,
	)
	if err != nil {
		return Run{}, fmt.Errorf("scan run: %w", err)
	}
	return run, nil
}

// NextRunSeq returns a monotonic per-record sequence used to keep repeated
// executions at the same planned time in stable insertion order. The pool is
// limited to a single connection, so allocate-then-insert never interleaves.
func (s *Store) NextRunSeq() (int64, error) {
	row := s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM job_runs`)
	var seq int64
	if err := row.Scan(&seq); err != nil {
		return 0, fmt.Errorf("allocate run seq: %w", err)
	}
	return seq, nil
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "UNIQUE constraint failed") && strings.Contains(message, "jobs.id")
}
