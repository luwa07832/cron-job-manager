package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Canonical renders t in the unambiguous form used both on disk and over the
// wire: UTC with an explicit "Z" suffix.
func Canonical(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// CreateJob inserts a new task and returns the stored row.
func (s *Store) CreateJob(input NewJob, now time.Time) (Job, error) {
	job := Job{
		ID:         uuid.NewString(),
		Name:       input.Name,
		Expression: input.Expression,
		CreatedAt:  now.UTC(),
		UpdatedAt:  now.UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO jobs (id, name, expression, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.Expression, Canonical(job.CreatedAt), Canonical(job.UpdatedAt),
	)
	if err != nil {
		return Job{}, fmt.Errorf("insert job: %w", err)
	}
	return job, nil
}

// GetJob loads one task.
func (s *Store) GetJob(id string) (Job, error) {
	row := s.db.QueryRow(
		`SELECT id, name, expression, created_at, updated_at FROM jobs WHERE id = ?`, id,
	)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	return job, err
}

// UpdateJob replaces the mutable definition of a task.
func (s *Store) UpdateJob(id string, input NewJob, now time.Time) (Job, error) {
	_, err := s.db.Exec(
		`UPDATE jobs SET name = ?, expression = ?, updated_at = ? WHERE id = ?`,
		input.Name, input.Expression, Canonical(now.UTC()), id,
	)
	if err != nil {
		return Job{}, fmt.Errorf("update job: %w", err)
	}
	return s.GetJob(id)
}

// DeleteJob removes a task and cascades the removal to its run records.
// Deleting an unknown task returns ErrJobNotFound.
func (s *Store) DeleteJob(id string) error {
	res, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete job rows: %w", err)
	}
	if n == 0 {
		return ErrJobNotFound
	}
	return nil
}

type jobScanner interface {
	Scan(dest ...any) error
}

func scanJob(scanner jobScanner) (Job, error) {
	var job Job
	var createdAt, updatedAt string
	if err := scanner.Scan(&job.ID, &job.Name, &job.Expression, &createdAt, &updatedAt); err != nil {
		return Job{}, err
	}
	var err error
	if job.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Job{}, fmt.Errorf("parse created_at: %w", err)
	}
	if job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Job{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return job, nil
}

// ListJobs returns every stored task ordered by creation time.
func (s *Store) ListJobs() ([]Job, error) {
	rows, err := s.db.Query(
		`SELECT id, name, expression, created_at, updated_at FROM jobs ORDER BY created_at, id`,
	)
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
	return jobs, rows.Err()
}
