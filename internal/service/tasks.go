package service

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/luwa07832/cron-job-manager/internal/cron"
	"github.com/luwa07832/cron-job-manager/internal/store"
)

// TaskInput carries create/update fields supplied by callers.
type TaskInput struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Action   string `json:"action"`
}

// CreateTask validates and stores a new task, returning it with the current
// scheduling information.
func (s *Service) CreateTask(input TaskInput) (*Task, error) {
	if err := validateTaskInput(input); err != nil {
		return nil, err
	}
	parsed, err := cron.Parse(input.Schedule)
	if err != nil {
		return nil, ErrInvalidSchedule
	}
	if !s.executor.Supports(input.Action) {
		return nil, ErrInvalidAction
	}
	now := s.now()
	job := store.Job{
		ID:          input.ID,
		Name:        strings.TrimSpace(input.Name),
		Schedule:    strings.TrimSpace(input.Schedule),
		Action:      strings.TrimSpace(input.Action),
		CreatedAtNs: now.UnixNano(),
		UpdatedAtNs: now.UnixNano(),
	}
	if job.ID == "" {
		job.ID = newID()
	}
	if err := s.store.CreateJob(job); err != nil {
		if errors.Is(err, store.ErrDuplicateID) {
			return nil, ErrTaskIDDuplicate
		}
		return nil, err
	}
	return s.toTask(job, parsed, now), nil
}

// UpdateTask overwrites an existing task and returns its scheduling info.
func (s *Service) UpdateTask(id string, input TaskInput) (*Task, error) {
	input.ID = id
	if err := validateTaskInput(input); err != nil {
		return nil, err
	}
	parsed, err := cron.Parse(input.Schedule)
	if err != nil {
		return nil, ErrInvalidSchedule
	}
	if !s.executor.Supports(input.Action) {
		return nil, ErrInvalidAction
	}
	existing, err := s.store.GetJob(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	now := s.now()
	existing.Name = strings.TrimSpace(input.Name)
	existing.Schedule = strings.TrimSpace(input.Schedule)
	existing.Action = strings.TrimSpace(input.Action)
	existing.UpdatedAtNs = now.UnixNano()
	if err := s.store.UpdateJob(existing); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	return s.toTask(existing, parsed, now), nil
}

// DeleteTask removes a task. Historical run records remain queryable.
func (s *Service) DeleteTask(id string) error {
	if err := s.store.DeleteJob(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrTaskNotFound
		}
		return err
	}
	return nil
}

// GetTask returns one task with its current scheduling information.
func (s *Service) GetTask(id string) (*Task, error) {
	job, err := s.store.GetJob(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	return s.jobToTask(job)
}

// ListTasks returns every task with current scheduling information.
func (s *Service) ListTasks() ([]Task, error) {
	jobs, err := s.store.ListJobs()
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(jobs))
	for _, job := range jobs {
		task, err := s.jobToTask(job)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, *task)
	}
	return tasks, nil
}

// ValidateSchedule checks an expression and returns when it would fire around
// now, using the same time semantics as task create/update responses.
func (s *Service) ValidateSchedule(expression string) (Scheduler, error) {
	parsed, err := cron.Parse(expression)
	if err != nil {
		return Scheduler{}, ErrInvalidSchedule
	}
	return schedulerFor(parsed, s.now()), nil
}

func validateTaskInput(input TaskInput) error {
	if strings.TrimSpace(input.Name) == "" {
		return ErrInvalidTask
	}
	if strings.TrimSpace(input.Schedule) == "" {
		return ErrInvalidTask
	}
	if strings.TrimSpace(input.Action) == "" {
		return ErrInvalidTask
	}
	if strings.TrimSpace(input.ID) != input.ID && input.ID != "" {
		return ErrInvalidTask
	}
	return nil
}

func (s *Service) jobToTask(job store.Job) (*Task, error) {
	parsed, err := cron.Parse(job.Schedule)
	if err != nil {
		return nil, err
	}
	return s.toTask(job, parsed, s.now()), nil
}

func (s *Service) toTask(job store.Job, parsed *cron.Schedule, now time.Time) *Task {
	return &Task{
		ID:        job.ID,
		Name:      job.Name,
		Schedule:  job.Schedule,
		Action:    job.Action,
		CreatedAt: time.Unix(0, job.CreatedAtNs).UTC(),
		UpdatedAt: time.Unix(0, job.UpdatedAtNs).UTC(),
		Scheduler: schedulerFor(parsed, now),
	}
}

// schedulerFor reports the most recent planned slot at or before now (when it
// lies inside the search horizon) and the next slot strictly after now.
func schedulerFor(parsed *cron.Schedule, now time.Time) Scheduler {
	info := Scheduler{Schedule: parsed.Expression()}
	if current, ok := parsed.LatestAtOrBefore(now); ok {
		current = current.UTC()
		info.CurrentFireTime = &current
	}
	if next, ok := parsed.Next(now); ok {
		next = next.UTC()
		info.NextFireTime = &next
	}
	return info
}

func nullTime(ns sql.NullInt64) *time.Time {
	if !ns.Valid {
		return nil
	}
	value := time.Unix(0, ns.Int64).UTC()
	return &value
}
