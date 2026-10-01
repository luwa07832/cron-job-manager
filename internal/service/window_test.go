package service

import (
	"errors"
	"testing"
	"time"
)

func TestWindowSeparatesPendingAndExecuted(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	createTestTask(t, svc, "t1", "*/15 * * * *", "succeed")

	// Execute the 10:00 slot; 10:15 and 10:30 remain pending.
	if _, err := svc.TriggerScheduled("t1", TriggerInput{PlannedFireTime: parseTestTime("2026-01-01T10:00:00Z")}); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	result, err := svc.QueryWindow(parseTestTime("2026-01-01T10:00:00Z"), parseTestTime("2026-01-01T10:30:00Z"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Executed) != 1 {
		t.Fatalf("executed = %d records: %+v", len(result.Executed), result.Executed)
	}
	if got := result.Executed[0].PlannedFireTime; !got.Equal(parseTestTime("2026-01-01T10:00:00Z")) {
		t.Fatalf("executed planned = %s", got)
	}
	if len(result.Pending) != 2 {
		t.Fatalf("pending = %d: %+v", len(result.Pending), result.Pending)
	}
	for _, pending := range result.Pending {
		if pending.RecordType != RecordPending || pending.Status != "pending" {
			t.Fatalf("pending record = %+v", pending)
		}
	}
}

func TestWindowClosedBounds(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	createTestTask(t, svc, "t1", "*/15 * * * *", "succeed")
	slot := parseTestTime("2026-01-01T10:00:00Z")
	if _, err := svc.TriggerScheduled("t1", TriggerInput{PlannedFireTime: slot}); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	result, err := svc.QueryWindow(slot, slot)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Executed) != 1 || len(result.Pending) != 0 {
		t.Fatalf("exact-boundary window = %+v", result)
	}
}

func TestWindowOrderingKeepsPerTaskChronology(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	createTestTask(t, svc, "t1", "*/15 * * * *", "fail:boom")
	times := []string{
		"2026-01-01T10:30:00Z",
		"2026-01-01T10:00:00Z",
		"2026-01-01T10:15:00Z",
	}
	var firstID string
	for index, ts := range times {
		record, err := svc.TriggerScheduled("t1", TriggerInput{PlannedFireTime: parseTestTime(ts)})
		if err != nil {
			t.Fatalf("trigger %d: %v", index, err)
		}
		if index == 1 {
			firstID = record.ID
		}
	}
	retry, err := svc.Retry(firstID, parseTestTime("2026-01-01T10:05:00Z"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	result, err := svc.QueryWindow(parseTestTime("2026-01-01T10:00:00Z"), parseTestTime("2026-01-01T10:30:00Z"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Executed) != 4 {
		t.Fatalf("executed = %d: %+v", len(result.Executed), result.Executed)
	}
	wantOrder := []string{
		"2026-01-01T10:00:00Z",
		"2026-01-01T10:05:00Z",
		"2026-01-01T10:15:00Z",
		"2026-01-01T10:30:00Z",
	}
	for i, want := range wantOrder {
		if got := result.Executed[i].PlannedFireTime; !got.Equal(parseTestTime(want)) {
			t.Fatalf("executed[%d] = %s, want %s", i, got, want)
		}
	}
	if result.Executed[1].ID != retry.ID || result.Executed[1].RecordType != RecordRetry {
		t.Fatalf("retry position/type wrong: %+v", result.Executed[1])
	}
}

func TestWindowEmptyIsSuccessNotError(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	result, err := svc.QueryWindow(parseTestTime("2026-02-01T00:00:00Z"), parseTestTime("2026-02-02T00:00:00Z"))
	if err != nil {
		t.Fatalf("empty query: %v", err)
	}
	if result.Pending == nil || result.Executed == nil || len(result.Pending) != 0 || len(result.Executed) != 0 {
		t.Fatalf("empty result = %+v", result)
	}
}

func TestWindowValidation(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	_, err := svc.QueryWindow(parseTestTime("2026-01-02T00:00:00Z"), parseTestTime("2026-01-01T00:00:00Z"))
	if !errors.Is(err, ErrInvalidWindow) {
		t.Fatalf("reversed window err = %v", err)
	}
	_, err = svc.QueryWindow(parseTestTime("2026-01-01T00:00:00Z"), parseTestTime("2026-01-01T00:00:00Z").Add(MaxQuerySpan+time.Nanosecond))
	if !errors.Is(err, ErrWindowTooLarge) {
		t.Fatalf("oversized window err = %v", err)
	}
	// Exact maximum span is allowed.
	_, err = svc.QueryWindow(parseTestTime("2026-01-01T00:00:00Z"), parseTestTime("2026-01-01T00:00:00Z").Add(MaxQuerySpan))
	if err != nil {
		t.Fatalf("max span rejected: %v", err)
	}
}

func TestTaskErrors(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	if _, err := svc.GetTask("nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	if err := svc.DeleteTask("nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
	if _, err := svc.TriggerScheduled("nope", TriggerInput{}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("trigger missing = %v", err)
	}
	if _, err := svc.RunManual("nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("manual missing = %v", err)
	}
	if _, err := svc.CreateTask(TaskInput{Name: "x", Schedule: "nonsense", Action: "succeed"}); !errors.Is(err, ErrInvalidSchedule) {
		t.Fatalf("bad schedule = %v", err)
	}
	if _, err := svc.CreateTask(TaskInput{Name: "x", Schedule: "* * * * *", Action: "explode"}); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("bad action = %v", err)
	}
	if _, err := svc.CreateTask(TaskInput{Name: "  ", Schedule: "* * * * *", Action: "succeed"}); !errors.Is(err, ErrInvalidTask) {
		t.Fatalf("blank name = %v", err)
	}
}

func TestScheduleUpdateDoesNotRewriteHistory(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	task := createTestTask(t, svc, "t1", "*/15 * * * *", "succeed")
	record, err := svc.TriggerScheduled("t1", TriggerInput{PlannedFireTime: parseTestTime("2026-01-01T10:00:00Z")})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := svc.UpdateTask(task.ID, TaskInput{Name: "renamed", Schedule: "0 * * * *", Action: "succeed"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	result, err := svc.QueryWindow(parseTestTime("2026-01-01T10:00:00Z"), parseTestTime("2026-01-01T10:00:00Z"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Executed) != 1 {
		t.Fatalf("history lost: %+v", result)
	}
	history := result.Executed[0]
	if history.Schedule != "*/15 * * * *" || history.TaskName != "task-t1" {
		t.Fatalf("history rewritten: %+v", history)
	}
	if history.ID != record.ID {
		t.Fatal("history record identity changed")
	}
}

func TestParseTimeAcceptsExplicitOffsets(t *testing.T) {
	clock := func() time.Time { return parseTestTime("2026-01-01T00:00:00Z") }
	utcTime, err := ParseTime("2026-01-01T08:00:00+08:00", clock)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !utcTime.Equal(parseTestTime("2026-01-01T00:00:00Z")) {
		t.Fatalf("offset time = %s", utcTime)
	}
	if _, err := ParseTime("not-a-time", clock); !errors.Is(err, ErrInvalidTimeInput) {
		t.Fatalf("bad parse = %v", err)
	}
	if got, err := ParseTime("", clock); err != nil || !got.Equal(clock()) {
		t.Fatalf("empty parse = %s err=%v", got, err)
	}
}

func TestManualRunUsesNowAsPlannedTime(t *testing.T) {
	svc, cleanup := newTestService(t, "2026-01-01T10:07:00Z")
	defer cleanup()
	createTestTask(t, svc, "t1", "0 * * * *", "succeed")
	record, err := svc.RunManual("t1")
	if err != nil {
		t.Fatalf("manual: %v", err)
	}
	if record.TriggerType != TriggerManual || record.RecordType != RecordOriginal {
		t.Fatalf("manual record = %+v", record)
	}
	if !record.PlannedFireTime.Equal(record.ActualFireTime) {
		t.Fatalf("manual planned = %s actual = %s", record.PlannedFireTime, record.ActualFireTime)
	}
}
