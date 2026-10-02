package store

import (
	"errors"
	"testing"
	"time"
)

func TestAdvanceNextRunMovesOnlyMatchingCursor(t *testing.T) {
	st := newTestStore(t)
	current := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	next := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	if _, err := st.CreateJob(sampleJob("job-1", "hourly", true, &current)); err != nil {
		t.Fatalf("create: %v", err)
	}

	updated := time.Date(2026, 10, 2, 9, 5, 0, 0, time.UTC)
	if err := st.AdvanceNextRun("job-1", current, next, updated); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got, err := st.GetJob("job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.NextRun == nil || !got.NextRun.Equal(next) {
		t.Fatalf("next_run = %v, want %s", got.NextRun, next)
	}
	if !got.UpdatedAt.Equal(updated) {
		t.Fatalf("updated_at = %s, want %s", got.UpdatedAt, updated)
	}

	// Reusing the old cursor loses the race and changes nothing.
	later := time.Date(2026, 10, 2, 11, 0, 0, 0, time.UTC)
	err = st.AdvanceNextRun("job-1", current, later, updated)
	if !errors.Is(err, ErrNextRunConflict) {
		t.Fatalf("err = %v, want ErrNextRunConflict", err)
	}
	got, _ = st.GetJob("job-1")
	if got.NextRun == nil || !got.NextRun.Equal(next) {
		t.Fatalf("next_run changed after conflict: %v", got.NextRun)
	}

	// Matching the new cursor succeeds.
	if err := st.AdvanceNextRun("job-1", next, later, updated); err != nil {
		t.Fatalf("second advance: %v", err)
	}
}

func TestAdvanceNextRunRejectsMissingDeletedAndDisabled(t *testing.T) {
	st := newTestStore(t)
	current := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	next := current.Add(time.Hour)
	updated := current.Add(time.Minute)

	err := st.AdvanceNextRun("ghost", current, next, updated)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job err = %v, want ErrNotFound", err)
	}

	if _, err := st.CreateJob(sampleJob("off", "paused", false, nil)); err != nil {
		t.Fatalf("create disabled: %v", err)
	}
	if err := st.AdvanceNextRun("off", current, next, updated); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled job err = %v, want ErrNotFound", err)
	}

	if _, err := st.CreateJob(sampleJob("gone", "to delete", true, &current)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.DeleteJob("gone", updated); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.AdvanceNextRun("gone", current, next, updated); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted job err = %v, want ErrNotFound", err)
	}
}
