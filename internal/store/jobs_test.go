package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sampleJob(id, name string, enabled bool, next *time.Time) *Job {
	now := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	return &Job{
		ID:         id,
		Name:       name,
		Expression: "0 9 * * *",
		Timezone:   "UTC",
		Enabled:    enabled,
		CreatedAt:  now,
		UpdatedAt:  now,
		NextRun:    next,
	}
}

func TestCreateAndGetJob(t *testing.T) {
	st := newTestStore(t)
	next := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	saved, err := st.CreateJob(sampleJob("job-1", "nightly", true, &next))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if saved.ID != "job-1" {
		t.Fatalf("id = %q", saved.ID)
	}

	got, err := st.GetJob("job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "nightly" || !got.Enabled {
		t.Fatalf("unexpected job: %+v", got)
	}
	if got.NextRun == nil || !got.NextRun.Equal(next) {
		t.Fatalf("next run = %v, want %s", got.NextRun, next)
	}
}

func TestGetMissingJob(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.GetJob("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDuplicateActiveNameConflicts(t *testing.T) {
	st := newTestStore(t)
	next := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	if _, err := st.CreateJob(sampleJob("a", "dup", true, &next)); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := st.CreateJob(sampleJob("b", "dup", true, &next)); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("create duplicate: err = %v, want ErrNameConflict", err)
	}
}

func TestDeletedNameCanBeReusedAndJobHidden(t *testing.T) {
	st := newTestStore(t)
	next := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	if _, err := st.CreateJob(sampleJob("a", "recycle", true, &next)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.DeleteJob("a", time.Now()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetJob("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: err = %v, want ErrNotFound", err)
	}
	if err := st.DeleteJob("a", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again: err = %v, want ErrNotFound", err)
	}
	if _, err := st.CreateJob(sampleJob("b", "recycle", true, &next)); err != nil {
		t.Fatalf("reuse name: %v", err)
	}
}

func TestUpdateJobPersistsFields(t *testing.T) {
	st := newTestStore(t)
	next := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	job := sampleJob("a", "before", true, &next)
	if _, err := st.CreateJob(job); err != nil {
		t.Fatalf("create: %v", err)
	}
	job.Name = "after"
	job.Expression = "0 10 * * *"
	job.Enabled = false
	job.NextRun = nil
	updatedAt := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	job.UpdatedAt = updatedAt
	saved, err := st.UpdateJob(job)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if saved.Name != "after" || saved.Enabled || saved.NextRun != nil {
		t.Fatalf("unexpected saved job: %+v", saved)
	}
	got, _ := st.GetJob("a")
	if got.Name != "after" || got.NextRun != nil || !got.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("unexpected stored job: %+v", got)
	}
}

func TestUpdateMissingJob(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.UpdateJob(sampleJob("ghost", "x", true, nil)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPendingJobsWindowAndOrder(t *testing.T) {
	st := newTestStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	inside := []time.Time{
		time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
	}
	disabledNext := time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)
	deletedNext := time.Date(2026, 10, 2, 9, 15, 0, 0, time.UTC)
	beforeNext := time.Date(2026, 10, 1, 23, 59, 0, 0, time.UTC)
	afterNext := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)

	mustCreate := func(job *Job) {
		t.Helper()
		if _, err := st.CreateJob(job); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	mustCreate(sampleJob("early", "early", true, &inside[0]))
	mustCreate(sampleJob("first", "first", true, &inside[1]))
	mustCreate(sampleJob("disabled", "disabled", false, &disabledNext))

	deleted := sampleJob("deleted", "deleted", true, &deletedNext)
	mustCreate(deleted)
	if err := st.DeleteJob("deleted", base); err != nil {
		t.Fatalf("delete: %v", err)
	}
	mustCreate(sampleJob("before", "before", true, &beforeNext))
	mustCreate(sampleJob("after", "after", true, &afterNext))

	from := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	jobs, err := st.PendingJobs(from, to)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(jobs), jobs)
	}
	if jobs[0].ID != "first" || jobs[1].ID != "early" {
		t.Fatalf("order = %s, %s; want first then early", jobs[0].ID, jobs[1].ID)
	}

	// Half-open: to boundary excludes the exact match.
	jobs, _ = st.PendingJobs(from, time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC))
	if len(jobs) != 1 || jobs[0].ID != "first" {
		t.Fatalf("right-open window = %+v", jobs)
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	next := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	if _, err := st.CreateJob(sampleJob("persist", "persist", true, &next)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.GetJob("persist")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.Name != "persist" || got.NextRun == nil || !got.NextRun.Equal(next) {
		t.Fatalf("unexpected persisted job: %+v", got)
	}
}
