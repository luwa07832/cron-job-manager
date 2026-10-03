package store

import (
	"errors"
	"testing"
	"time"
)

func newFailedAttempt(runID string, scheduled time.Time) *Attempt {
	failure := "boom"
	return &Attempt{
		JobID:        "job-1",
		RunID:        runID,
		Attempt:      1,
		ScheduledFor: scheduled,
		StartedAt:    scheduled,
		FinishedAt:   scheduled.Add(time.Minute),
		Outcome:      "failed",
		Error:        &failure,
	}
}

func TestCreateRunWithIdempotencyReplayAndConflict(t *testing.T) {
	st := newTestStore(t)
	createJobForRun(t, st, "job-1")
	scheduled := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

	idem := &IdempotencyRequest{Key: "evt-1", Fingerprint: "fp-a"}
	attempt := newFailedAttempt("run-first", scheduled)
	created, err := st.CreateRunWithIdempotency(attempt, idem)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.RunID != "run-first" || len(created.Attempts) != 1 {
		t.Fatalf("created = %+v", created)
	}

	// Replay with a different caller-chosen id must still return the stored run.
	replay, err := st.CreateRunWithIdempotency(newFailedAttempt("run-second", scheduled),
		&IdempotencyRequest{Key: "evt-1", Fingerprint: "fp-a"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.RunID != "run-first" || len(replay.Attempts) != 1 {
		t.Fatalf("replay = %+v", replay)
	}

	// Same key, different semantics conflicts without adding a row.
	_, err = st.CreateRunWithIdempotency(newFailedAttempt("run-third", scheduled),
		&IdempotencyRequest{Key: "evt-1", Fingerprint: "fp-b"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	run, err := st.GetRun("job-1", "run-first")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(run.Attempts) != 1 {
		t.Fatalf("attempts after conflict = %d", len(run.Attempts))
	}

	// Nil key keeps legacy behavior: every call creates a new run.
	legacy, err := st.CreateRunWithIdempotency(newFailedAttempt("run-legacy", scheduled), nil)
	if err != nil {
		t.Fatalf("legacy create: %v", err)
	}
	if legacy.RunID != "run-legacy" {
		t.Fatalf("legacy = %+v", legacy)
	}
}

func TestCreateRunIdempotencyKeyScopedPerJob(t *testing.T) {
	st := newTestStore(t)
	createJobForRun(t, st, "job-1")
	createJobForRun(t, st, "job-2")
	scheduled := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

	first := newFailedAttempt("run-a", scheduled)
	first.JobID = "job-1"
	if _, err := st.CreateRunWithIdempotency(first,
		&IdempotencyRequest{Key: "shared", Fingerprint: "fp"}); err != nil {
		t.Fatalf("job-1 create: %v", err)
	}
	second := newFailedAttempt("run-b", scheduled)
	second.JobID = "job-2"
	if _, err := st.CreateRunWithIdempotency(second,
		&IdempotencyRequest{Key: "shared", Fingerprint: "fp"}); err != nil {
		t.Fatalf("job-2 create: %v", err)
	}
}

func TestAppendRetryIdempotencyReplayDoesNotAddAttempt(t *testing.T) {
	st := newTestStore(t)
	createJobForRun(t, st, "job-1")
	scheduled := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	if err := st.CreateRun(newFailedAttempt("run-1", scheduled)); err != nil {
		t.Fatalf("create: %v", err)
	}

	idem := &IdempotencyRequest{Key: "retry-1", Fingerprint: "fp-a"}
	appended, err := st.AppendRetryWithIdempotency("job-1", "run-1",
		scheduled.Add(2*time.Minute), scheduled.Add(3*time.Minute), "failed", strp("again"), idem)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(appended.Attempts) != 2 || appended.Attempts[1].Attempt != 2 {
		t.Fatalf("appended = %+v", appended.Attempts)
	}

	// Replay after the chain advances returns the stored snapshot (attempt 2),
	// never appends another attempt.
	if _, err := st.AppendRetry("job-1", "run-1",
		scheduled.Add(4*time.Minute), scheduled.Add(5*time.Minute), "succeeded", nil); err != nil {
		t.Fatalf("follow-up retry: %v", err)
	}
	replay, err := st.AppendRetryWithIdempotency("job-1", "run-1",
		scheduled.Add(2*time.Minute), scheduled.Add(3*time.Minute), "failed", strp("again"),
		&IdempotencyRequest{Key: "retry-1", Fingerprint: "fp-a"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replay.Attempts) != 2 || replay.Attempts[1].Attempt != 2 {
		t.Fatalf("replay snapshot = %+v", replay.Attempts)
	}
	fresh, err := st.GetRun("job-1", "run-1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(fresh.Attempts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(fresh.Attempts))
	}

	// Same key with different semantics conflicts.
	_, err = st.AppendRetryWithIdempotency("job-1", "run-1",
		scheduled.Add(2*time.Minute), scheduled.Add(3*time.Minute), "succeeded", nil,
		&IdempotencyRequest{Key: "retry-1", Fingerprint: "fp-b"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict = %v", err)
	}
}

func TestAppendRetryIdempotencyScopedByJobAndRun(t *testing.T) {
	st := newTestStore(t)
	createJobForRun(t, st, "job-1")
	createJobForRun(t, st, "job-2")
	scheduled := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	first := newFailedAttempt("run-1", scheduled)
	if err := st.CreateRun(first); err != nil {
		t.Fatalf("create job-1 run: %v", err)
	}
	second := newFailedAttempt("run-1", scheduled)
	second.JobID = "job-2"
	if err := st.CreateRun(second); err != nil {
		t.Fatalf("create job-2 run: %v", err)
	}

	idem := &IdempotencyRequest{Key: "k", Fingerprint: "fp"}
	if _, err := st.AppendRetryWithIdempotency("job-1", "run-1",
		scheduled.Add(time.Minute), scheduled.Add(2*time.Minute), "succeeded", nil, idem); err != nil {
		t.Fatalf("job-1 retry: %v", err)
	}
	if _, err := st.AppendRetryWithIdempotency("job-2", "run-1",
		scheduled.Add(time.Minute), scheduled.Add(2*time.Minute), "succeeded", nil,
		&IdempotencyRequest{Key: "k", Fingerprint: "fp"}); err != nil {
		t.Fatalf("job-2 retry: %v", err)
	}

	// Same key text against a different run within the job conflicts.
	other := newFailedAttempt("run-2", scheduled)
	if err := st.CreateRun(other); err != nil {
		t.Fatalf("create run-2: %v", err)
	}
	_, err := st.AppendRetryWithIdempotency("job-1", "run-2",
		scheduled.Add(time.Minute), scheduled.Add(2*time.Minute), "succeeded", nil,
		&IdempotencyRequest{Key: "k", Fingerprint: "fp"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cross-run conflict = %v", err)
	}
}
