package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestJobCRUDAndWindowPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	jobs := []*Job{
		{Name: "second", Expression: "0 12 1 6 *", Timezone: "UTC", Enabled: true, NextRun: &second},
		{Name: "first", Expression: "0 0 1 1 *", Timezone: "UTC", Enabled: true, NextRun: &first},
		{Name: "off", Expression: "0 0 1 1 *", Timezone: "UTC", Enabled: false},
	}
	for _, job := range jobs {
		if err := st.CreateJob(job); err != nil {
			t.Fatalf("create %s: %v", job.Name, err)
		}
		if job.ID == "" || job.CreatedAt.IsZero() {
			t.Fatalf("created job missing id or created_at: %+v", job)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	pending, err := reopened.ListPendingJobs(
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("len(pending) = %d, want 2", len(pending))
	}
	if pending[0].Name != "first" || pending[1].Name != "second" {
		t.Fatalf("order = %s, %s; want first then second", pending[0].Name, pending[1].Name)
	}

	loaded, err := reopened.GetJob(pending[0].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.Expression != "0 0 1 1 *" || loaded.Timezone != "UTC" || !loaded.Enabled {
		t.Fatalf("unexpected loaded job: %+v", loaded)
	}
	if loaded.NextRun == nil || !loaded.NextRun.Equal(first) {
		t.Fatalf("next_run = %v, want %s", loaded.NextRun, first)
	}

	renamed := pending[1]
	renamed.Name = "second-renamed"
	if err := reopened.UpdateJob(renamed); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := reopened.DeleteJob(pending[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := reopened.DeleteJob(pending[0].ID); err != ErrNotFound {
		t.Fatalf("repeat delete err = %v, want ErrNotFound", err)
	}
	if _, err := reopened.GetJob(pending[0].ID); err != ErrNotFound {
		t.Fatalf("get deleted err = %v, want ErrNotFound", err)
	}
	if err := reopened.CreateJob(&Job{
		Name: "second-renamed", Expression: "0 0 * * *", Timezone: "UTC", Enabled: false,
	}); err != ErrNameConflict {
		t.Fatalf("duplicate name err = %v, want ErrNameConflict", err)
	}
}
