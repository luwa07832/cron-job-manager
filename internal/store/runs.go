package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/luwa07832/cron-job-manager/internal/schedule"
)

const runColumns = `seq, job_id, job_name, expression, record_type, trigger_type,
	retry_of_seq, retry_number, retry_count, scheduled_for, triggered_at,
	started_at, finished_at, result, failure_reason, next_scheduled_for, created_at`

// RecordRun persists an original execution attempt. Duplicate calls for the
// same job and planned time always create independent rows.
func (s *Store) RecordRun(input NewRun) (RunRecord, error) {
	job, err := s.GetJob(input.JobID)
	if err != nil {
		return RunRecord{}, err
	}
	var next sql.NullString
	if !input.NextScheduledAt.IsZero() {
		next = sql.NullString{String: Canonical(input.NextScheduledAt), Valid: true}
	}
	res, err := s.db.Exec(
		`INSERT INTO run_records
			(job_id, job_name, expression, record_type, trigger_type, retry_of_seq,
			 retry_number, retry_count, scheduled_for, triggered_at, started_at,
			 finished_at, result, failure_reason, next_scheduled_for, created_at)
		 VALUES (?, ?, ?, ?, ?, NULL, 0, 0, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.Expression, input.RecordType, input.TriggerType,
		Canonical(input.ScheduledFor), Canonical(input.TriggeredAt), Canonical(input.StartedAt),
		Canonical(input.FinishedAt), input.Result, nullableReason(input.Result, input.FailureReason),
		next, Canonical(input.FinishedAt),
	)
	if err != nil {
		return RunRecord{}, fmt.Errorf("insert run: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return RunRecord{}, fmt.Errorf("insert run id: %w", err)
	}
	return s.GetRun(seq)
}

func nullableReason(result, reason string) sql.NullString {
	if result == ResultFailure {
		return sql.NullString{String: reason, Valid: true}
	}
	return sql.NullString{}
}

// RecordRetry appends a retry attempt for an original run, stamps the retry
// number onto the new row and bumps the original run's retry counter. The
// whole operation is atomic.
func (s *Store) RecordRetry(input NewRun) (RunRecord, error) {
	if input.RetryOfSeq <= 0 {
		return RunRecord{}, ErrRunNotFound
	}
	tx, err := s.db.Begin()
	if err != nil {
		return RunRecord{}, fmt.Errorf("begin retry tx: %w", err)
	}
	defer tx.Rollback()

	var recordType, result, triggerType, jobName, expression string
	var jobID string
	err = tx.QueryRow(
		`SELECT r.job_id, r.record_type, r.result, r.trigger_type, j.name, j.expression
		 FROM run_records r JOIN jobs j ON j.id = r.job_id WHERE r.seq = ?`, input.RetryOfSeq,
	).Scan(&jobID, &recordType, &result, &triggerType, &jobName, &expression)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, ErrRunNotFound
	}
	if err != nil {
		return RunRecord{}, fmt.Errorf("load original run: %w", err)
	}
	if recordType != RecordOriginal {
		return RunRecord{}, ErrRetryNotRetryable
	}
	if result != ResultFailure {
		return RunRecord{}, ErrRetryNotRetryable
	}

	var retryCount int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM run_records WHERE retry_of_seq = ?`, input.RetryOfSeq,
	).Scan(&retryCount); err != nil {
		return RunRecord{}, fmt.Errorf("count retries: %w", err)
	}
	retryNumber := retryCount + 1
	var next sql.NullString
	if !input.NextScheduledAt.IsZero() {
		next = sql.NullString{String: Canonical(input.NextScheduledAt), Valid: true}
	}
	retryRes, err := tx.Exec(
		`INSERT INTO run_records
			(job_id, job_name, expression, record_type, trigger_type, retry_of_seq,
			 retry_number, retry_count, scheduled_for, triggered_at, started_at,
			 finished_at, result, failure_reason, next_scheduled_for, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, jobName, expression, RecordRetry, triggerType, input.RetryOfSeq,
		retryNumber, Canonical(input.ScheduledFor), Canonical(input.TriggeredAt),
		Canonical(input.StartedAt), Canonical(input.FinishedAt), input.Result,
		nullableReason(input.Result, input.FailureReason), next, Canonical(input.FinishedAt),
	)
	if err != nil {
		return RunRecord{}, fmt.Errorf("insert retry: %w", err)
	}
	insertSeq, err := retryRes.LastInsertId()
	if err != nil {
		return RunRecord{}, fmt.Errorf("insert retry id: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE run_records SET retry_count = ? WHERE seq = ?`, retryNumber, input.RetryOfSeq,
	); err != nil {
		return RunRecord{}, fmt.Errorf("bump retry count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RunRecord{}, fmt.Errorf("commit retry: %w", err)
	}
	return s.GetRun(insertSeq)
}

// GetRun loads one run record by sequence number.
func (s *Store) GetRun(seq int64) (RunRecord, error) {
	row := s.db.QueryRow(`SELECT `+runColumns+` FROM run_records WHERE seq = ?`, seq)
	record, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, ErrRunNotFound
	}
	return record, err
}

type runScanner interface {
	Scan(dest ...any) error
}

func scanRun(scanner runScanner) (RunRecord, error) {
	var r RunRecord
	var scheduledFor, triggeredAt, startedAt, finishedAt, createdAt string
	var next sql.NullString
	if err := scanner.Scan(
		&r.Seq, &r.JobID, &r.JobName, &r.Expression, &r.RecordType, &r.TriggerType,
		&r.RetryOfSeq, &r.RetryNumber, &r.RetryCount, &scheduledFor, &triggeredAt,
		&startedAt, &finishedAt, &r.Result, &r.FailureReason, &next, &createdAt,
	); err != nil {
		return RunRecord{}, err
	}
	var err error
	if r.ScheduledFor, err = parseCanonical(scheduledFor); err != nil {
		return RunRecord{}, err
	}
	if r.TriggeredAt, err = parseCanonical(triggeredAt); err != nil {
		return RunRecord{}, err
	}
	if r.StartedAt, err = parseCanonical(startedAt); err != nil {
		return RunRecord{}, err
	}
	if r.FinishedAt, err = parseCanonical(finishedAt); err != nil {
		return RunRecord{}, err
	}
	if r.CreatedAt, err = parseCanonical(createdAt); err != nil {
		return RunRecord{}, err
	}
	if next.Valid {
		if r.NextScheduledFor, err = nullTime(next.String); err != nil {
			return RunRecord{}, err
		}
	}
	return r, nil
}

func parseCanonical(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time: %w", err)
	}
	return t, nil
}

func nullTime(raw string) (sql.NullTime, error) {
	t, err := parseCanonical(raw)
	if err != nil {
		return sql.NullTime{}, err
	}
	return sql.NullTime{Time: t, Valid: true}, nil
}

// RunsInWindow returns every run record whose planned trigger time falls in
// the closed interval [start, end], ordered by planned time then insertion
// order so retries keep their chronological relationship to other attempts.
func (s *Store) RunsInWindow(start, end time.Time) ([]RunRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+runColumns+` FROM run_records
		 WHERE scheduled_for >= ? AND scheduled_for <= ?
		 ORDER BY scheduled_for, seq`,
		Canonical(start), Canonical(end),
	)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()
	var records []RunRecord
	for rows.Next() {
		record, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// WindowInclusive returns executed records and pending planned triggers for
// the closed interval [start, end]. A planned trigger is pending when no
// original run record exists for that job at that exact planned instant.
func (s *Store) WindowInclusive(start, end time.Time) (WindowResult, error) {
	executed, err := s.RunsInWindow(start, end)
	if err != nil {
		return WindowResult{}, err
	}

	jobs, err := s.ListJobs()
	if err != nil {
		return WindowResult{}, err
	}
	executedOriginals, err := s.originalTimesInWindow(start, end)
	if err != nil {
		return WindowResult{}, err
	}

	var pending []PendingItem
	for _, job := range jobs {
		cron, err := schedule.Parse(job.Expression)
		if err != nil {
			continue
		}
		cursor := start.Add(-time.Minute)
		for {
			next, err := cron.Next(cursor)
			if err != nil || next.After(end) {
				break
			}
			key := Canonical(next)
			if !executedOriginals[job.ID][key] {
				pending = append(pending, PendingItem{
					JobID:        job.ID,
					JobName:      job.Name,
					Expression:   job.Expression,
					ScheduledFor: next.UTC(),
				})
			}
			cursor = next
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].ScheduledFor.Equal(pending[j].ScheduledFor) {
			return pending[i].JobID < pending[j].JobID
		}
		return pending[i].ScheduledFor.Before(pending[j].ScheduledFor)
	})
	return WindowResult{Pending: pending, Executed: executed}, nil
}

func (s *Store) originalTimesInWindow(start, end time.Time) (map[string]map[string]bool, error) {
	rows, err := s.db.Query(
		`SELECT job_id, scheduled_for FROM run_records
		 WHERE record_type = ? AND scheduled_for >= ? AND scheduled_for <= ?`,
		RecordOriginal, Canonical(start), Canonical(end),
	)
	if err != nil {
		return nil, fmt.Errorf("query original times: %w", err)
	}
	defer rows.Close()
	result := map[string]map[string]bool{}
	for rows.Next() {
		var jobID, scheduledFor string
		if err := rows.Scan(&jobID, &scheduledFor); err != nil {
			return nil, err
		}
		if result[jobID] == nil {
			result[jobID] = map[string]bool{}
		}
		result[jobID][scheduledFor] = true
	}
	return result, rows.Err()
}
