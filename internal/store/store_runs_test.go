package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openRunsStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestJobCRUDLifecycle(t *testing.T) {
	st := openRunsStore(t)
	job := Job{ID: "j1", Name: "n", Schedule: "* * * * *", Action: "succeed", CreatedAtNs: 1, UpdatedAtNs: 1}
	if err := st.CreateJob(job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.CreateJob(job); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate = %v", err)
	}
	got, err := st.GetJob("j1")
	if err != nil || got.Name != "n" {
		t.Fatalf("get = %+v err=%v", got, err)
	}
	got.Name = "n2"
	got.UpdatedAtNs = 2
	if err := st.UpdateJob(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if refreshed, _ := st.GetJob("j1"); refreshed.Name != "n2" {
		t.Fatalf("update not persisted: %+v", refreshed)
	}
	if err := st.DeleteJob("j1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetJob("j1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get = %v", err)
	}
	if err := st.DeleteJob("j1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete = %v", err)
	}
}

func TestRunPersistenceAndWindowQuery(t *testing.T) {
	st := openRunsStore(t)
	insert := func(id string, planned int64, status, recordType string) int64 {
		seq, err := st.NextRunSeq()
		if err != nil {
			t.Fatalf("seq for %s: %v", id, err)
		}
		if err := st.InsertRun(Run{
			ID: id, JobID: "j1", JobNameSnapshot: "n", ScheduleSnapshot: "* * * * *",
			RecordType: recordType, TriggerType: "scheduled", Attempt: 1, OriginalRunID: id,
			PlannedFireTimeNs: planned, ActualFireTimeNs: planned + 1,
			NextFireTimeNs: sql.NullInt64{Int64: planned + 60, Valid: true},
			Status:         status, CreatedAtNs: planned, Seq: seq,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		return seq
	}
	seq1 := insert("r1", 100, RunFailure, "original")
	seq2 := insert("r2", 100, RunSuccess, "original") // repeated trigger same slot
	insert("r3", 200, RunSuccess, "retry")
	insert("r4", 300, RunRunning, "original")
	if seq2 != seq1+1 {
		t.Fatalf("seq = %d,%d, want monotonic after insert", seq1, seq2)
	}

	runs, err := st.RunsInWindow(100, 200)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("terminal runs in window = %d, want 3 (running excluded)", len(runs))
	}
	if runs[0].ID != "r1" || runs[1].ID != "r2" || runs[2].ID != "r3" {
		t.Fatalf("window order = %s,%s,%s", runs[0].ID, runs[1].ID, runs[2].ID)
	}

	slots, err := st.OriginalScheduledSlots("j1", 0, 500)
	if err != nil {
		t.Fatalf("slots: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("occupied slots = %v, want {100,300}", slots)
	}
	if _, ok := slots[100]; !ok {
		t.Fatal("slot 100 missing")
	}
	if _, ok := slots[300]; !ok {
		t.Fatal("running slot 300 should still occupy its planned time")
	}

	if err := st.FinishRun("r4", 300, 301, RunSuccess, sql.NullString{}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := st.GetRun("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run = %v", err)
	}
}
