package service

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/luwa07832/cron-job-manager/internal/cron"
	"github.com/luwa07832/cron-job-manager/internal/store"
)

// TriggerInput optionally pins the planned fire time of a scheduled trigger.
type TriggerInput struct {
	PlannedFireTime time.Time
}

// TriggerScheduled records and performs a scheduler-driven execution. When no
// planned time is supplied, the most recent scheduled slot at or before now is
// used. Repeated triggers for the same planned time create independent records.
func (s *Service) TriggerScheduled(jobID string, input TriggerInput) (*RunRecord, error) {
	job, err := s.store.GetJob(jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	parsed, err := cron.Parse(job.Schedule)
	if err != nil {
		return nil, ErrInvalidSchedule
	}
	now := s.now()
	var planned time.Time
	if !input.PlannedFireTime.IsZero() {
		planned = input.PlannedFireTime.UTC()
	} else {
		latest, ok := parsed.LatestAtOrBefore(now)
		if !ok {
			return nil, ErrInvalidSchedule
		}
		planned = latest
	}
	return s.execute(job, parsed, executionRequest{
		RecordType:      RecordOriginal,
		TriggerType:     TriggerScheduled,
		Attempt:         1,
		PlannedFireTime: planned,
		ActualFireTime:  now,
		OriginalRunID:   "",
		ParentRunID:     "",
	})
}

// RunManual performs an ad-hoc execution outside the schedule. The planned fire
// time equals the trigger instant; such records stay original executions.
func (s *Service) RunManual(jobID string) (*RunRecord, error) {
	job, err := s.store.GetJob(jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	parsed, err := cron.Parse(job.Schedule)
	if err != nil {
		return nil, ErrInvalidSchedule
	}
	now := s.now()
	return s.execute(job, parsed, executionRequest{
		RecordType:      RecordOriginal,
		TriggerType:     TriggerManual,
		Attempt:         1,
		PlannedFireTime: now,
		ActualFireTime:  now,
	})
}

// Retry re-runs a failed execution record. The new record is a retry of the
// same execution chain and carries its own planned time and outcome.
func (s *Service) Retry(runID string, at time.Time) (*RunRecord, error) {
	parent, err := s.store.GetRun(runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	if parent.Status != store.RunFailure {
		return nil, ErrRunNotRetriable
	}
	job, err := s.store.GetJob(parent.JobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	parsed, err := cron.Parse(parent.ScheduleSnapshot)
	if err != nil {
		return nil, err
	}
	now := s.now()
	planned := now
	if !at.IsZero() {
		planned = at.UTC()
	}
	chainID := parent.OriginalRunID
	if chainID == "" {
		chainID = parent.ID
	}
	return s.execute(job, parsed, executionRequest{
		RecordType:      RecordRetry,
		TriggerType:     parent.TriggerType,
		Attempt:         parent.Attempt + 1,
		PlannedFireTime: planned,
		ActualFireTime:  now,
		OriginalRunID:   chainID,
		ParentRunID:     parent.ID,
	})
}

// GetRun returns one execution record by id.
func (s *Service) GetRun(runID string) (*RunRecord, error) {
	run, err := s.store.GetRun(runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	if run.Status == store.RunRunning {
		return nil, ErrRunNotFound
	}
	record := toRunRecord(run)
	return &record, nil
}

type executionRequest struct {
	RecordType      string
	TriggerType     string
	Attempt         int
	PlannedFireTime time.Time
	ActualFireTime  time.Time
	OriginalRunID   string
	ParentRunID     string
}

func (s *Service) execute(job store.Job, parsed *cron.Schedule, request executionRequest) (*RunRecord, error) {
	runID := newID()
	if request.OriginalRunID == "" {
		request.OriginalRunID = runID
	}
	seq, err := s.store.NextRunSeq()
	if err != nil {
		return nil, err
	}
	nextFire, ok := parsed.Next(request.PlannedFireTime)
	var nextFireNs sql.NullInt64
	if ok {
		nextFireNs = sql.NullInt64{Int64: nextFire.UnixNano(), Valid: true}
	}
	run := store.Run{
		ID:                runID,
		JobID:             job.ID,
		JobNameSnapshot:   job.Name,
		ScheduleSnapshot:  job.Schedule,
		RecordType:        request.RecordType,
		TriggerType:       request.TriggerType,
		Attempt:           request.Attempt,
		OriginalRunID:     request.OriginalRunID,
		PlannedFireTimeNs: request.PlannedFireTime.UnixNano(),
		ActualFireTimeNs:  request.ActualFireTime.UnixNano(),
		NextFireTimeNs:    nextFireNs,
		Status:            store.RunRunning,
		CreatedAtNs:       request.ActualFireTime.UnixNano(),
		Seq:               seq,
	}
	if request.ParentRunID != "" {
		run.ParentRunID = sql.NullString{String: request.ParentRunID, Valid: true}
	}
	if err := s.store.InsertRun(run); err != nil {
		return nil, err
	}

	started := s.now()
	status, reason := s.safeExecute(job)
	finished := s.now()
	if finished.Before(started) {
		finished = started
	}
	reasonNull := sql.NullString{}
	if status == store.RunFailure {
		reasonNull = sql.NullString{String: reason, Valid: true}
	}
	if err := s.store.FinishRun(runID, started.UnixNano(), finished.UnixNano(), status, reasonNull); err != nil {
		return nil, err
	}
	stored, err := s.store.GetRun(runID)
	if err != nil {
		return nil, err
	}
	record := toRunRecord(stored)
	return &record, nil
}

// safeExecute runs the action and converts a panic into a failed outcome so a
// faulty action can never swallow the record.
func (s *Service) safeExecute(job store.Job) (status, reason string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			status = store.RunFailure
			reason = fmt.Sprintf("task execution panicked: %v", recovered)
		}
	}()
	if err := s.executor.Execute(job.ID, job.Action); err != nil {
		return store.RunFailure, failureText(err)
	}
	return store.RunSuccess, ""
}

func toRunRecord(run store.Run) RunRecord {
	record := RunRecord{
		RecordType:      run.RecordType,
		Attempt:         run.Attempt,
		ID:              run.ID,
		TaskID:          run.JobID,
		TaskName:        run.JobNameSnapshot,
		Schedule:        run.ScheduleSnapshot,
		TriggerType:     run.TriggerType,
		PlannedFireTime: time.Unix(0, run.PlannedFireTimeNs).UTC(),
		ActualFireTime:  time.Unix(0, run.ActualFireTimeNs).UTC(),
		NextFireTime:    nullTime(run.NextFireTimeNs),
		StartedAt:       time.Unix(0, run.StartedAtNs.Int64).UTC(),
		FinishedAt:      time.Unix(0, run.FinishedAtNs.Int64).UTC(),
		Status:          run.Status,
	}
	if run.RecordType == RecordRetry {
		record.OriginalRunID = run.OriginalRunID
	}
	if run.ParentRunID.Valid {
		record.ParentRunID = run.ParentRunID.String
	}
	if run.FailureReason.Valid {
		record.FailureReason = run.FailureReason.String
	}
	return record
}
