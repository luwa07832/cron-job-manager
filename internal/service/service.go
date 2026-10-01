// Package service holds the scheduling, execution-record and time-window query
// logic on top of the SQLite store.
//
// All time values cross package boundaries as time.Time values anchored in UTC
// and are rendered with explicit offsets, so observable results never depend
// on the host machine zone.
package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/luwa07832/cron-job-manager/internal/store"
)

// Record type discriminators. Callers use these to tell original executions
// apart from retry executions and pending slots.
const (
	RecordOriginal = "original"
	RecordRetry    = "retry"
	RecordPending  = "pending"
)

// Trigger type discriminators.
const (
	TriggerScheduled = "scheduled"
	TriggerManual    = "manual"
)

// Terminal execution outcomes. Only these two values are ever exposed.
const (
	StatusSuccess = store.RunSuccess
	StatusFailure = store.RunFailure
)

// MaxQuerySpan is the largest inclusive window a query may span.
const MaxQuerySpan = 90 * 24 * time.Hour

// Sentinel errors. The HTTP layer maps each one to a unique error code.
var (
	ErrTaskNotFound     = errors.New("task not found")
	ErrTaskIDDuplicate  = errors.New("task id already exists")
	ErrInvalidSchedule  = errors.New("invalid schedule expression")
	ErrInvalidTask      = errors.New("invalid task definition")
	ErrInvalidAction    = errors.New("invalid task action")
	ErrRunNotFound      = errors.New("run record not found")
	ErrRunNotRetriable  = errors.New("run record is not retriable")
	ErrInvalidWindow    = errors.New("invalid time window")
	ErrWindowTooLarge   = errors.New("query window exceeds maximum span")
	ErrInvalidTimeInput = errors.New("invalid time input")
)

// Clock yields the current instant. Production uses UTC wall clock; tests pass
// a controllable one.
type Clock func() time.Time

// Executor runs one task action. Failure reasons must be non-empty; returning
// nil means success. Implementations must not panic (panics are still captured
// defensively by the service).
type Executor interface {
	Supports(action string) bool
	Execute(jobID, action string) error
}

// ExecutorFunc adapts a function to Executor. It treats every action as
// supported; wrap it when validation behavior matters.
type ExecutorFunc func(jobID, action string) error

// Supports implements Executor.
func (ExecutorFunc) Supports(string) bool { return true }

// Execute implements Executor.
func (f ExecutorFunc) Execute(jobID, action string) error { return f(jobID, action) }

// Scheduler returns scheduling information for one task.
type Scheduler struct {
	Schedule        string     `json:"schedule"`
	CurrentFireTime *time.Time `json:"current_fire_time,omitempty"`
	NextFireTime    *time.Time `json:"next_fire_time,omitempty"`
}

// Task is a task definition plus its current scheduling information.
type Task struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Schedule  string    `json:"schedule"`
	Action    string    `json:"action"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Scheduler
}

// RunRecord is an executed (terminal) run: original execution or retry.
type RunRecord struct {
	RecordType      string     `json:"record_type"`
	Attempt         int        `json:"attempt"`
	ID              string     `json:"id"`
	TaskID          string     `json:"task_id"`
	TaskName        string     `json:"task_name"`
	Schedule        string     `json:"schedule"`
	TriggerType     string     `json:"trigger_type,omitempty"`
	PlannedFireTime time.Time  `json:"planned_fire_time"`
	ActualFireTime  time.Time  `json:"actual_fire_time"`
	NextFireTime    *time.Time `json:"next_fire_time,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      time.Time  `json:"finished_at"`
	Status          string     `json:"status"`
	FailureReason   string     `json:"failure_reason,omitempty"`
	OriginalRunID   string     `json:"original_run_id,omitempty"`
	ParentRunID     string     `json:"parent_run_id,omitempty"`
}

// PendingRecord is a scheduled slot inside a window that has not executed yet.
type PendingRecord struct {
	RecordType      string    `json:"record_type"`
	Attempt         int       `json:"attempt"`
	TaskID          string    `json:"task_id"`
	TaskName        string    `json:"task_name"`
	Schedule        string    `json:"schedule"`
	TriggerType     string    `json:"trigger_type"`
	PlannedFireTime time.Time `json:"planned_fire_time"`
	NextFireTime    time.Time `json:"next_fire_time"`
	Status          string    `json:"status"`
}

// WindowResult separates pending and executed records for one query window.
type WindowResult struct {
	WindowStart time.Time       `json:"window_start"`
	WindowEnd   time.Time       `json:"window_end"`
	Pending     []PendingRecord `json:"pending"`
	Executed    []RunRecord     `json:"executed"`
}

// Service implements task management, execution recording and window queries.
type Service struct {
	store    *store.Store
	now      Clock
	executor Executor
}

// New builds a Service with the UTC wall clock and the built-in action runner.
func New(st *store.Store) *Service {
	return NewWithClock(st, func() time.Time { return time.Now().UTC() }, NewBuiltinExecutor())
}

// NewWithClock injects a clock and executor, mainly for deterministic tests.
func NewWithClock(st *store.Store, clock Clock, executor Executor) *Service {
	return &Service{store: st, now: clock, executor: executor}
}

// Ping reports whether the backing storage is usable.
func (s *Service) Ping() error { return s.store.Ping() }

// Now returns the service clock instant in UTC.
func (s *Service) Now() time.Time { return s.now() }

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw[:])
}

// ParseTime parses an RFC3339 timestamp; empty input means "now".
func ParseTime(raw string, now Clock) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return now(), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, ErrInvalidTimeInput
	}
	return parsed.UTC(), nil
}
