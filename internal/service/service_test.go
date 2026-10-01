package service

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/luwa07832/cron-job-manager/internal/store"
)

func newTestService(t *testing.T, clockTime string) (*Service, func()) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base := parseTestTime(clockTime)
	clock := func() time.Time { return base }
	return NewWithClock(st, clock, NewBuiltinExecutor()), func() {
		st.Close()
	}
}

func parseTestTime(ts string) time.Time {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	return parsed.UTC()
}

func createTestTask(t *testing.T, svc *Service, id, schedule, action string) *Task {
	t.Helper()
	task, err := svc.CreateTask(TaskInput{ID: id, Name: "task-" + id, Schedule: schedule, Action: action})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

func TestCreateTaskReturnsSchedulingInfo(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	task := createTestTask(t, svc, "t1", "*/15 * * * *", "succeed")

	if task.Schedule != "*/15 * * * *" {
		t.Fatalf("schedule = %q", task.Schedule)
	}
	if task.NextFireTime == nil || !task.NextFireTime.Equal(parseTestTime("2026-01-01T10:15:00Z")) {
		t.Fatalf("next = %v", task.NextFireTime)
	}
	if task.CurrentFireTime == nil || !task.CurrentFireTime.Equal(parseTestTime("2026-01-01T10:00:00Z")) {
		t.Fatalf("current = %v", task.CurrentFireTime)
	}
}

func TestTriggerRecordsOriginalExecutionWithTimesAndResult(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	createTestTask(t, svc, "t1", "*/15 * * * *", "fail:boom")

	record, err := svc.TriggerScheduled("t1", TriggerInput{})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if record.RecordType != RecordOriginal {
		t.Fatalf("record type = %q", record.RecordType)
	}
	if !record.PlannedFireTime.Equal(parseTestTime("2026-01-01T10:00:00Z")) {
		t.Fatalf("planned = %s", record.PlannedFireTime)
	}
	if !record.ActualFireTime.Equal(parseTestTime("2026-01-01T10:07:00Z")) {
		t.Fatalf("actual = %s", record.ActualFireTime)
	}
	if record.NextFireTime == nil || !record.NextFireTime.Equal(parseTestTime("2026-01-01T10:15:00Z")) {
		t.Fatalf("next = %v", record.NextFireTime)
	}
	if record.Status != StatusFailure || record.FailureReason != "boom" {
		t.Fatalf("status = %q reason = %q", record.Status, record.FailureReason)
	}
	if !record.StartedAt.Equal(record.ActualFireTime) || record.FinishedAt.Before(record.StartedAt) {
		t.Fatalf("execution bounds wrong: start=%s finish=%s", record.StartedAt, record.FinishedAt)
	}
	if record.OriginalRunID != "" || record.ParentRunID != "" {
		t.Fatalf("original run must not carry retry linkage: %+v", record)
	}
	if record.Attempt != 1 {
		t.Fatalf("attempt = %d", record.Attempt)
	}
}

func TestSuccessRecordHasNoFailureReason(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	createTestTask(t, svc, "t1", "*/15 * * * *", "succeed")
	record, err := svc.TriggerScheduled("t1", TriggerInput{})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if record.Status != StatusSuccess {
		t.Fatalf("status = %q", record.Status)
	}
	if record.FailureReason != "" {
		t.Fatalf("success record forged failure reason %q", record.FailureReason)
	}
}

func TestRetryBuildsOrderedChainWithItsOwnTimesAndResult(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	createTestTask(t, svc, "t1", "*/15 * * * *", "fail:boom")

	original, err := svc.TriggerScheduled("t1", TriggerInput{PlannedFireTime: parseTestTime("2026-01-01T10:00:00Z")})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	retry1, err := svc.Retry(original.ID, parseTestTime("2026-01-01T10:01:00Z"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry1.RecordType != RecordRetry || retry1.Attempt != 2 {
		t.Fatalf("retry = %+v", retry1)
	}
	if retry1.OriginalRunID != original.ID || retry1.ParentRunID != original.ID {
		t.Fatalf("retry linkage = %+v", retry1)
	}
	if !retry1.PlannedFireTime.Equal(parseTestTime("2026-01-01T10:01:00Z")) {
		t.Fatalf("retry planned = %s", retry1.PlannedFireTime)
	}
	if retry1.Status != StatusFailure || retry1.FailureReason != "boom" {
		t.Fatalf("retry result = %q/%q", retry1.Status, retry1.FailureReason)
	}

	retry2, err := svc.Retry(retry1.ID, parseTestTime("2026-01-01T10:02:00Z"))
	if err != nil {
		t.Fatalf("retry2: %v", err)
	}
	if retry2.Attempt != 3 || retry2.OriginalRunID != original.ID || retry2.ParentRunID != retry1.ID {
		t.Fatalf("retry2 = %+v", retry2)
	}
}

func TestRetryRequiresFailure(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	createTestTask(t, svc, "t1", "*/15 * * * *", "succeed")
	ok, err := svc.TriggerScheduled("t1", TriggerInput{})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := svc.Retry(ok.ID, time.Time{}); !errors.Is(err, ErrRunNotRetriable) {
		t.Fatalf("retry err = %v, want ErrRunNotRetriable", err)
	}
	if _, err := svc.Retry("missing", time.Time{}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("retry missing err = %v, want ErrRunNotFound", err)
	}
}

func TestRepeatedTriggerSameSlotKeepsIndependentRecords(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	createTestTask(t, svc, "t1", "*/15 * * * *", "succeed")
	slot := parseTestTime("2026-01-01T10:00:00Z")
	first, err := svc.TriggerScheduled("t1", TriggerInput{PlannedFireTime: slot})
	if err != nil {
		t.Fatalf("trigger1: %v", err)
	}
	second, err := svc.TriggerScheduled("t1", TriggerInput{PlannedFireTime: slot})
	if err != nil {
		t.Fatalf("trigger2: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("repeated trigger merged into one record")
	}
}

type panickingExecutor struct{}

func (panickingExecutor) Supports(string) bool      { return true }
func (panickingExecutor) Execute(_, _ string) error { panic("kaboom") }

func TestPanickingExecutorIsRecordedAsFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "panic.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	base := parseTestTime("2026-01-01T10:07:00Z")
	svc := NewWithClock(st, func() time.Time { return base }, panickingExecutor{})
	createTestTask(t, svc, "t1", "*/15 * * * *", "anything")
	record, err := svc.TriggerScheduled("t1", TriggerInput{})
	if err != nil {
		t.Fatalf("trigger must not surface the panic: %v", err)
	}
	if record.Status != StatusFailure {
		t.Fatalf("status = %q", record.Status)
	}
	if record.FailureReason == "" {
		t.Fatal("panic failure reason must be queryable")
	}
	result, err := svc.QueryWindow(parseTestTime("2026-01-01T10:00:00Z"), parseTestTime("2026-01-01T10:15:00Z"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Executed) != 1 {
		t.Fatalf("panic run not recorded immediately: %+v", result)
	}
}
