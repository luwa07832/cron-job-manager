package store

import (
	"errors"
	"testing"
	"time"
)

func createJobForRun(t *testing.T, st *Store, id string) {
	t.Helper()
	next := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	if _, err := st.CreateJob(sampleJob(id, id, true, &next)); err != nil {
		t.Fatalf("create job: %v", err)
	}
}

func TestCreateAndGetRun(t *testing.T) {
	st := newTestStore(t)
	createJobForRun(t, st, "job-1")

	scheduled := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	failure := "boom"
	attempt := &Attempt{
		JobID:        "job-1",
		RunID:        "run-1",
		ScheduledFor: scheduled,
		StartedAt:    scheduled.Add(time.Minute),
		FinishedAt:   scheduled.Add(2 * time.Minute),
		Outcome:      "failed",
		Error:        &failure,
	}
	if err := st.CreateRun(attempt); err != nil {
		t.Fatalf("create run: %v", err)
	}

	run, err := st.GetRun("job-1", "run-1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.JobID != "job-1" || run.RunID != "run-1" || !run.ScheduledFor.Equal(scheduled) {
		t.Fatalf("unexpected run header: %+v", run)
	}
	if len(run.Attempts) != 1 || run.Attempts[0].Attempt != 1 {
		t.Fatalf("attempts = %+v", run.Attempts)
	}
	if run.Attempts[0].Error == nil || *run.Attempts[0].Error != "boom" {
		t.Fatalf("error = %v", run.Attempts[0].Error)
	}
}

func TestGetRunMissingJobAndRun(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.GetRun("ghost", "run-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job err = %v, want ErrNotFound", err)
	}
	createJobForRun(t, st, "job-1")
	if _, err := st.GetRun("job-1", "ghost-run"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing run err = %v, want ErrRunNotFound", err)
	}
}

func TestAppendRetryIncrementsAndCopiesSchedule(t *testing.T) {
	st := newTestStore(t)
	createJobForRun(t, st, "job-1")

	scheduled := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	failure := "first failure"
	if err := st.CreateRun(&Attempt{
		JobID: "job-1", RunID: "run-1", ScheduledFor: scheduled,
		StartedAt: scheduled, FinishedAt: scheduled.Add(time.Minute),
		Outcome: "failed", Error: &failure,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	second, err := st.AppendRetry("job-1", "run-1",
		scheduled.Add(2*time.Minute), scheduled.Add(3*time.Minute), "succeeded", nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.Attempt != 2 || !second.ScheduledFor.Equal(scheduled) {
		t.Fatalf("retry = %+v", second)
	}

	run, _ := st.GetRun("job-1", "run-1")
	if len(run.Attempts) != 2 {
		t.Fatalf("attempts = %d", len(run.Attempts))
	}
	if run.Attempts[0].Attempt != 1 || run.Attempts[1].Attempt != 2 {
		t.Fatalf("attempt order = %+v", run.Attempts)
	}
	if run.Attempts[1].Error != nil || run.Attempts[1].Outcome != "succeeded" {
		t.Fatalf("retry result = %+v", run.Attempts[1])
	}
}

func TestRetryRejectedUnlessLatestFailed(t *testing.T) {
	st := newTestStore(t)
	createJobForRun(t, st, "job-1")
	scheduled := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	if err := st.CreateRun(&Attempt{
		JobID: "job-1", RunID: "run-1", ScheduledFor: scheduled,
		StartedAt: scheduled, FinishedAt: scheduled.Add(time.Minute), Outcome: "succeeded",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := st.AppendRetry("job-1", "run-1", scheduled, scheduled.Add(time.Minute), "failed", strp("x")); !errors.Is(err, ErrRetryNotAllowed) {
		t.Fatalf("err = %v, want ErrRetryNotAllowed", err)
	}
	if _, err := st.AppendRetry("job-1", "ghost", scheduled, scheduled.Add(time.Minute), "failed", strp("x")); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}

	// After a failed run is followed by a success, retrying again is denied.
	failure := "x"
	if err := st.CreateRun(&Attempt{
		JobID: "job-1", RunID: "run-2", ScheduledFor: scheduled,
		StartedAt: scheduled, FinishedAt: scheduled.Add(time.Minute),
		Outcome: "failed", Error: &failure,
	}); err != nil {
		t.Fatalf("create run 2: %v", err)
	}
	if _, err := st.AppendRetry("job-1", "run-2", scheduled, scheduled.Add(time.Minute), "succeeded", nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := st.AppendRetry("job-1", "run-2", scheduled, scheduled.Add(time.Minute), "failed", strp("y")); !errors.Is(err, ErrRetryNotAllowed) {
		t.Fatalf("err = %v, want ErrRetryNotAllowed", err)
	}
}

func TestExecutedAttemptsWindowOrderAndDeletedJobs(t *testing.T) {
	st := newTestStore(t)
	createJobForRun(t, st, "a")
	createJobForRun(t, st, "b")
	createJobForRun(t, st, "gone")

	at := func(day, hour int) time.Time {
		return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC)
	}
	failure := "x"
	mustRun := func(jobID, runID string, scheduled time.Time, outcome string) {
		t.Helper()
		var errText *string
		if outcome == "failed" {
			errText = &failure
		}
		if err := st.CreateRun(&Attempt{
			JobID: jobID, RunID: runID, ScheduledFor: scheduled,
			StartedAt: scheduled, FinishedAt: scheduled.Add(time.Minute),
			Outcome: outcome, Error: errText,
		}); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}
	mustRun("a", "r-a1", at(2, 9), "succeeded")
	mustRun("b", "r-b1", at(2, 9), "failed")
	mustRun("a", "r-a2", at(3, 8), "succeeded")
	mustRun("gone", "r-g", at(2, 10), "succeeded")
	// Retry of r-b1 at the same scheduled_for, attempt 2.
	if _, err := st.AppendRetry("b", "r-b1", at(2, 10), at(2, 11), "succeeded", nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := st.DeleteJob("gone", time.Now()); err != nil {
		t.Fatalf("delete: %v", err)
	}

	from := at(2, 0)
	to := at(3, 0)
	attempts, err := st.ExecutedAttempts(from, to)
	if err != nil {
		t.Fatalf("executed: %v", err)
	}
	if len(attempts) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(attempts), attempts)
	}
	type key struct {
		jobID, runID string
		attempt      int
	}
	got := make([]key, len(attempts))
	for i, attempt := range attempts {
		got[i] = key{attempt.JobID, attempt.RunID, attempt.Attempt}
	}
	want := []key{
		{"a", "r-a1", 1},
		{"b", "r-b1", 1},
		{"b", "r-b1", 2},
		{"gone", "r-g", 1},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %+v, want %+v; full %+v", i, got[i], want[i], got)
		}
	}

	// Half-open window: exact match on to is excluded.
	attempts, _ = st.ExecutedAttempts(from, at(2, 9))
	if len(attempts) != 0 {
		t.Fatalf("right-open window = %+v", attempts)
	}
}

func TestDeletedJobHistoryStaysReadableAndActiveCheck(t *testing.T) {
	st := newTestStore(t)
	createJobForRun(t, st, "job-1")
	scheduled := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	failure := "x"
	if err := st.CreateRun(&Attempt{
		JobID: "job-1", RunID: "run-1", ScheduledFor: scheduled,
		StartedAt: scheduled, FinishedAt: scheduled.Add(time.Minute),
		Outcome: "failed", Error: &failure,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := st.DeleteJob("job-1", time.Now()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	exists, err := st.JobExists("job-1")
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if exists {
		t.Fatalf("deleted job reported as active")
	}
	run, err := st.GetRun("job-1", "run-1")
	if err != nil {
		t.Fatalf("history after delete: %v", err)
	}
	if len(run.Attempts) != 1 {
		t.Fatalf("attempts = %d", len(run.Attempts))
	}
}

func strp(value string) *string { return &value }
