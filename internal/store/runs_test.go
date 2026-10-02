package store

import (
	"errors"
	"testing"
	"time"
)

func strPtr(value string) *string { return &value }

func seedJobForRuns(t *testing.T, st *Store, id string) {
	t.Helper()
	next := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := st.CreateJob(sampleJob(id, "job "+id, true, &next)); err != nil {
		t.Fatalf("create job: %v", err)
	}
}

func TestCreateRunPersistsFirstAttempt(t *testing.T) {
	st := newTestStore(t)
	seedJobForRuns(t, st, "j1")
	scheduled := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	started := scheduled.Add(5 * time.Second)
	finished := started.Add(5 * time.Second)

	run, err := st.CreateRun(NewRunInput{
		RunID: "r1", JobID: "j1", ScheduledFor: scheduled,
		StartedAt: started, FinishedAt: finished,
		Outcome: OutcomeFailed, Error: strPtr("boom"),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(run.Attempts) != 1 || run.Attempts[0].Attempt != 1 {
		t.Fatalf("attempts = %+v", run.Attempts)
	}
	if run.Attempts[0].Error == nil || *run.Attempts[0].Error != "boom" {
		t.Fatalf("error = %v", run.Attempts[0].Error)
	}

	got, err := st.GetRun("j1", "r1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !got.ScheduledFor.Equal(scheduled) || got.Attempts[0].Outcome != OutcomeFailed {
		t.Fatalf("stored run = %+v", got)
	}
}

func TestCreateRunRejectsMissingAndDeletedJobs(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	input := NewRunInput{
		RunID: "rx", JobID: "ghost", ScheduledFor: now,
		StartedAt: now, FinishedAt: now, Outcome: OutcomeSucceeded,
	}
	if _, err := st.CreateRun(input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job err = %v, want ErrNotFound", err)
	}

	seedJobForRuns(t, st, "j2")
	if err := st.DeleteJob("j2", now); err != nil {
		t.Fatalf("delete: %v", err)
	}
	input.JobID = "j2"
	if _, err := st.CreateRun(input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted job err = %v, want ErrNotFound", err)
	}
}

func TestAddRetryIncrementsAndGatesOnLatestOutcome(t *testing.T) {
	st := newTestStore(t)
	seedJobForRuns(t, st, "j3")
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	run, err := st.CreateRun(NewRunInput{
		RunID: "r3", JobID: "j3", ScheduledFor: base,
		StartedAt: base, FinishedAt: base.Add(time.Second),
		Outcome: OutcomeFailed, Error: strPtr("nope"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	run, err = st.AddRetry("j3", "r3", base.Add(time.Minute), base.Add(61*time.Second), OutcomeFailed, strPtr("again"))
	if err != nil {
		t.Fatalf("retry 2: %v", err)
	}
	if run.Attempts[1].Attempt != 2 || run.Attempts[1].ScheduledFor != run.Attempts[0].ScheduledFor {
		t.Fatalf("retry attempt = %+v", run.Attempts[1])
	}
	run, err = st.AddRetry("j3", "r3", base.Add(2*time.Minute), base.Add(121*time.Second), OutcomeSucceeded, nil)
	if err != nil {
		t.Fatalf("retry 3: %v", err)
	}
	if len(run.Attempts) != 3 || run.Attempts[2].Error != nil {
		t.Fatalf("attempts = %+v", run.Attempts)
	}

	if _, err := st.AddRetry("j3", "r3", base.Add(3*time.Minute), base.Add(181*time.Second), OutcomeFailed, strPtr("x")); !errors.Is(err, ErrRetryNotAllowed) {
		t.Fatalf("retry after success err = %v, want ErrRetryNotAllowed", err)
	}
	if _, err := st.AddRetry("j3", "missing", base, base.Add(time.Second), OutcomeFailed, strPtr("x")); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing run err = %v, want ErrRunNotFound", err)
	}
}

func TestGetRunDistinguishesJobAndRun(t *testing.T) {
	st := newTestStore(t)
	seedJobForRuns(t, st, "j4")
	seedJobForRuns(t, st, "j5")
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := st.CreateRun(NewRunInput{
		RunID: "r4", JobID: "j4", ScheduledFor: base,
		StartedAt: base, FinishedAt: base.Add(time.Second), Outcome: OutcomeSucceeded,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.GetRun("j4", "nope"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing run err = %v, want ErrRunNotFound", err)
	}
	if _, err := st.GetRun("ghost", "r4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetRun("j5", "r4"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign run err = %v, want ErrRunNotFound", err)
	}

	if err := st.DeleteJob("j4", base); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, err := st.GetRun("j4", "r4"); err != nil || len(got.Attempts) != 1 {
		t.Fatalf("history after delete = %+v, %v", got, err)
	}
	if _, err := st.AddRetry("j4", "r4", base.Add(time.Minute), base.Add(61*time.Second), OutcomeSucceeded, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry after delete err = %v, want ErrNotFound", err)
	}
}

func TestExecutedRunsWindowAndOrder(t *testing.T) {
	st := newTestStore(t)
	seedJobForRuns(t, st, "a")
	seedJobForRuns(t, st, "b")
	at := func(h, m int) time.Time { return time.Date(2026, 10, 1, h, m, 0, 0, time.UTC) }

	mustRun := func(runID, jobID string, scheduled time.Time, outcome string, message *string) {
		t.Helper()
		if _, err := st.CreateRun(NewRunInput{
			RunID: runID, JobID: jobID, ScheduledFor: scheduled,
			StartedAt: scheduled, FinishedAt: scheduled.Add(time.Second),
			Outcome: outcome, Error: message,
		}); err != nil {
			t.Fatalf("create %s: %v", runID, err)
		}
	}
	mustRun("r-a", "a", at(10, 0), OutcomeFailed, strPtr("x"))
	if _, err := st.AddRetry("a", "r-a", at(10, 5), at(10, 6), OutcomeSucceeded, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	mustRun("r-b", "b", at(10, 0), OutcomeSucceeded, nil)
	mustRun("r-c", "b", at(11, 0), OutcomeSucceeded, nil)
	mustRun("r-late", "b", at(12, 0), OutcomeSucceeded, nil)

	from, to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	rows, err := st.ExecutedRuns(from, to)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5: %+v", len(rows), rows)
	}
	type row struct {
		jobID   string
		runID   string
		attempt int
	}
	got := make([]row, len(rows))
	for i, entry := range rows {
		got[i] = row{entry.JobID, entry.RunID, entry.Attempt}
	}
	want := []row{
		{"a", "r-a", 1}, {"a", "r-a", 2}, {"b", "r-b", 1},
		{"b", "r-c", 1}, {"b", "r-late", 1},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %+v, want %+v; all %+v", i, got[i], want[i], got)
		}
	}

	// Half-open window: [10:00, 11:00) keeps both attempts of r-a and r-b.
	rows, err = st.ExecutedRuns(at(10, 0), at(12, 0))
	if err != nil {
		t.Fatalf("boundary: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("boundary rows = %d, want 4", len(rows))
	}
}
