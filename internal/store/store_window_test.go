package store

import (
	"errors"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/service.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustJob(t *testing.T, st *Store, name, expr string, now time.Time) Job {
	t.Helper()
	job, err := st.CreateJob(NewJob{Name: name, Expression: expr}, now)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return job
}

func TestJobCRUDLifecycle(t *testing.T) {
	st := testStore(t)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

	job, err := st.CreateJob(NewJob{Name: "nightly", Expression: "0 2 * * *"}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err := st.GetJob(job.ID)
	if err != nil || loaded.Name != "nightly" {
		t.Fatalf("get: %v %+v", err, loaded)
	}
	updated := now.Add(time.Hour)
	if _, err := st.UpdateJob(job.ID, NewJob{Name: "nightly2", Expression: "0 3 * * *"}, updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	loaded, _ = st.GetJob(job.ID)
	if loaded.Name != "nightly2" || loaded.Expression != "0 3 * * *" {
		t.Fatalf("update not applied: %+v", loaded)
	}
	if err := st.DeleteJob(job.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetJob(job.ID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("get after delete err = %v, want ErrJobNotFound", err)
	}
	if err := st.DeleteJob(job.ID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("delete missing err = %v, want ErrJobNotFound", err)
	}
}

func TestRecordRunIndependentRowsAndRetries(t *testing.T) {
	st := testStore(t)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	job := mustJob(t, st, "j", "0 9 * * *", now)
	planned := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	first, err := st.RecordRun(NewRun{
		JobID: job.ID, RecordType: RecordOriginal, TriggerType: TriggerScheduled,
		ScheduledFor: planned, TriggeredAt: now, StartedAt: now, FinishedAt: now.Add(2 * time.Second),
		Result: ResultFailure, FailureReason: "boom",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if first.RecordType != RecordOriginal || first.RetryNumber != 0 || first.Expression != "0 9 * * *" {
		t.Fatalf("unexpected first run: %+v", first)
	}
	if !first.FailureReason.Valid || first.FailureReason.String != "boom" {
		t.Fatalf("failure reason not retained: %+v", first.FailureReason)
	}

	// Triggering the same planned time again creates an independent record.
	second, err := st.RecordRun(NewRun{
		JobID: job.ID, RecordType: RecordOriginal, TriggerType: TriggerScheduled,
		ScheduledFor: planned, TriggeredAt: now, StartedAt: now, FinishedAt: now,
		Result: ResultSuccess,
	})
	if err != nil {
		t.Fatalf("record duplicate: %v", err)
	}
	if second.Seq == first.Seq {
		t.Fatalf("duplicate trigger reused seq %d", second.Seq)
	}

	// Retry of the failed first attempt.
	retry1, err := st.RecordRetry(NewRun{
		RetryOfSeq: first.Seq, ScheduledFor: now.Add(time.Minute),
		TriggeredAt: now.Add(time.Minute), StartedAt: now.Add(time.Minute),
		FinishedAt: now.Add(2 * time.Minute), Result: ResultFailure, FailureReason: "still bad",
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry1.RecordType != RecordRetry || retry1.RetryNumber != 1 {
		t.Fatalf("unexpected retry: %+v", retry1)
	}
	if !retry1.RetryOfSeq.Valid || retry1.RetryOfSeq.Int64 != first.Seq {
		t.Fatalf("retry link wrong: %+v", retry1.RetryOfSeq)
	}

	// Second retry gets the next number and the original counter advances.
	if _, err := st.RecordRetry(NewRun{
		RetryOfSeq: first.Seq, ScheduledFor: now.Add(3 * time.Minute),
		TriggeredAt: now.Add(3 * time.Minute), StartedAt: now.Add(3 * time.Minute),
		FinishedAt: now.Add(3 * time.Minute), Result: ResultSuccess,
	}); err != nil {
		t.Fatalf("retry 2: %v", err)
	}
	updated, err := st.GetRun(first.Seq)
	if err != nil {
		t.Fatalf("reload original: %v", err)
	}
	if updated.RetryCount != 2 {
		t.Fatalf("retry count = %d, want 2", updated.RetryCount)
	}
}

func TestRetryGuards(t *testing.T) {
	st := testStore(t)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	job := mustJob(t, st, "j", "0 9 * * *", now)

	ok := func(result, reason string) NewRun {
		return NewRun{
			JobID: job.ID, RecordType: RecordOriginal, TriggerType: TriggerScheduled,
			ScheduledFor: now, TriggeredAt: now, StartedAt: now, FinishedAt: now,
			Result: result, FailureReason: reason,
		}
	}
	failed, err := st.RecordRun(ok(ResultFailure, "x"))
	if err != nil {
		t.Fatalf("record failed: %v", err)
	}
	succeeded, err := st.RecordRun(ok(ResultSuccess, ""))
	if err != nil {
		t.Fatalf("record success: %v", err)
	}
	retryBody := func(seq int64, result string) NewRun {
		return NewRun{RetryOfSeq: seq, ScheduledFor: now, TriggeredAt: now, StartedAt: now, FinishedAt: now, Result: result}
	}
	if _, err := st.RecordRetry(retryBody(succeeded.Seq, ResultSuccess)); !errors.Is(err, ErrRetryNotRetryable) {
		t.Fatalf("retry success err = %v, want ErrRetryNotRetryable", err)
	}
	retry, err := st.RecordRetry(retryBody(failed.Seq, ResultFailure))
	if err != nil {
		t.Fatalf("retry failed run: %v", err)
	}
	if _, err := st.RecordRetry(retryBody(retry.Seq, ResultSuccess)); !errors.Is(err, ErrRetryNotRetryable) {
		t.Fatalf("retry of retry err = %v, want ErrRetryNotRetryable", err)
	}
	if _, err := st.RecordRetry(retryBody(999999, ResultSuccess)); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("retry missing err = %v, want ErrRunNotFound", err)
	}
	if _, err := st.RecordRun(NewRun{
		JobID: "missing", RecordType: RecordOriginal, TriggerType: TriggerManual,
		ScheduledFor: now, TriggeredAt: now, StartedAt: now, FinishedAt: now, Result: ResultSuccess,
	}); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("record for missing job err = %v, want ErrJobNotFound", err)
	}
}

func TestWindowInclusiveSplitsPendingAndExecuted(t *testing.T) {
	st := testStore(t)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	job := mustJob(t, st, "hourly", "0 * * * *", now)

	windowStart := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	windowEnd := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// Execute the 10:00 planned trigger; 09:00, 11:00 and 12:00 stay pending.
	executedAt := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	original, err := st.RecordRun(NewRun{
		JobID: job.ID, RecordType: RecordOriginal, TriggerType: TriggerScheduled,
		ScheduledFor: executedAt, TriggeredAt: executedAt, StartedAt: executedAt,
		FinishedAt: executedAt.Add(time.Second), Result: ResultFailure, FailureReason: "nope",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	// Retry also scheduled at 10:00 must survive in executed results.
	if _, err := st.RecordRetry(NewRun{
		RetryOfSeq: original.Seq, ScheduledFor: executedAt.Add(30 * time.Second),
		TriggeredAt: executedAt.Add(30 * time.Second), StartedAt: executedAt.Add(30 * time.Second),
		FinishedAt: executedAt.Add(31 * time.Second), Result: ResultSuccess,
	}); err != nil {
		t.Fatalf("retry: %v", err)
	}

	result, err := st.WindowInclusive(windowStart, windowEnd)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	wantPending := []string{"2026-10-01T09:00:00Z", "2026-10-01T11:00:00Z", "2026-10-01T12:00:00Z"}
	if len(result.Pending) != len(wantPending) {
		t.Fatalf("pending len = %d, want %d: %+v", len(result.Pending), len(wantPending), result.Pending)
	}
	for i, item := range result.Pending {
		if got := Canonical(item.ScheduledFor); got != wantPending[i] {
			t.Fatalf("pending[%d] = %s, want %s", i, got, wantPending[i])
		}
	}
	if len(result.Executed) != 2 {
		t.Fatalf("executed len = %d, want 2 (original + retry)", len(result.Executed))
	}
	if result.Executed[0].ScheduledFor.After(result.Executed[1].ScheduledFor) {
		t.Fatalf("executed not in chronological order")
	}
}

func TestWindowEmptyIsSuccess(t *testing.T) {
	st := testStore(t)
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	result, err := st.WindowInclusive(start, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if len(result.Pending) != 0 || len(result.Executed) != 0 {
		t.Fatalf("expected empty slices, got %+v", result)
	}
}

func TestCanonicalIsTimezoneStable(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*60*60)
	local := time.Date(2026, 10, 1, 16, 30, 0, 0, loc)
	if got, want := Canonical(local), "2026-10-01T08:30:00Z"; got != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}
