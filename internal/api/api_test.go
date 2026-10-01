package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/luwa07832/cron-job-manager/internal/service"
	"github.com/luwa07832/cron-job-manager/internal/store"
)

type apiFixture struct {
	router http.Handler
	store  *store.Store
	svc    *service.Service
	base   time.Time
}

func newAPIFixture(t *testing.T, now string) apiFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base, err := time.Parse(time.RFC3339, now)
	if err != nil {
		t.Fatalf("parse now: %v", err)
	}
	clock := func() time.Time { return base.UTC() }
	svc := service.NewWithClock(st, clock, service.NewBuiltinExecutor())
	return apiFixture{router: NewRouterWithService(svc), store: st, svc: svc, base: base.UTC()}
}

func (f apiFixture) close() { f.store.Close() }

func (f apiFixture) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
}

func wantErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d body = %s, want %d", recorder.Code, recorder.Body.String(), status)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, recorder, &envelope)
	if envelope.Error.Code != code {
		t.Fatalf("code = %q, want %q (body %s)", envelope.Error.Code, code, recorder.Body.String())
	}
	if envelope.Error.Message == "" {
		t.Fatalf("error message empty: %s", recorder.Body.String())
	}
}

type runView struct {
	ID              string     `json:"id"`
	RecordType      string     `json:"record_type"`
	Attempt         int        `json:"attempt"`
	TaskID          string     `json:"task_id"`
	TriggerType     string     `json:"trigger_type"`
	PlannedFireTime time.Time  `json:"planned_fire_time"`
	ActualFireTime  time.Time  `json:"actual_fire_time"`
	NextFireTime    *time.Time `json:"next_fire_time"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      time.Time  `json:"finished_at"`
	Status          string     `json:"status"`
	FailureReason   string     `json:"failure_reason"`
	OriginalRunID   string     `json:"original_run_id"`
	ParentRunID     string     `json:"parent_run_id"`
}

type taskView struct {
	ID       string `json:"id"`
	Schedule string `json:"schedule"`
}

func TestCreateTriggerQueryLifecycle(t *testing.T) {
	fixture := newAPIFixture(t, "2026-01-01T10:07:00Z")
	defer fixture.close()

	created := fixture.do(t, http.MethodPost, "/api/v1/tasks", map[string]string{
		"id": "t1", "name": "nightly", "schedule": "*/15 * * * *", "action": "fail:boom",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", created.Code, created.Body.String())
	}
	var createEnvelope struct {
		Task struct {
			taskView
			NextFireTime *time.Time `json:"next_fire_time"`
		} `json:"task"`
	}
	decode(t, created, &createEnvelope)
	if createEnvelope.Task.ID != "t1" {
		t.Fatalf("created id = %q", createEnvelope.Task.ID)
	}
	if createEnvelope.Task.NextFireTime == nil ||
		!createEnvelope.Task.NextFireTime.Equal(mustTime("2026-01-01T10:15:00Z")) {
		t.Fatalf("next fire = %v", createEnvelope.Task.NextFireTime)
	}

	triggered := fixture.do(t, http.MethodPost, "/api/v1/tasks/t1/trigger", nil)
	if triggered.Code != http.StatusCreated {
		t.Fatalf("trigger = %d %s", triggered.Code, triggered.Body.String())
	}
	var runEnvelope struct {
		Run runView `json:"run"`
	}
	decode(t, triggered, &runEnvelope)
	original := runEnvelope.Run
	if original.Status != "failure" || original.FailureReason != "boom" {
		t.Fatalf("original = %+v", original)
	}
	if !original.PlannedFireTime.Equal(mustTime("2026-01-01T10:00:00Z")) {
		t.Fatalf("planned = %s", original.PlannedFireTime)
	}
	if original.OriginalRunID != "" || original.ParentRunID != "" {
		t.Fatalf("original linkage leaked: %+v", original)
	}

	// Record must be queryable immediately after execution finishes.
	window := fixture.do(t, http.MethodGet,
		"/api/v1/runs?start=2026-01-01T10:00:00Z&end=2026-01-01T10:30:00Z", nil)
	if window.Code != http.StatusOK {
		t.Fatalf("window = %d %s", window.Code, window.Body.String())
	}
	var result struct {
		Pending  []json.RawMessage `json:"pending"`
		Executed []runView         `json:"executed"`
	}
	decode(t, window, &result)
	if len(result.Executed) != 1 || len(result.Pending) != 2 {
		t.Fatalf("window counts executed=%d pending=%d", len(result.Executed), len(result.Pending))
	}

	// Retry the failed record at an explicit planned time.
	retried := fixture.do(t, http.MethodPost, "/api/v1/runs/"+original.ID+"/retry",
		map[string]string{"planned_fire_time": "2026-01-01T10:05:00Z"})
	if retried.Code != http.StatusCreated {
		t.Fatalf("retry = %d %s", retried.Code, retried.Body.String())
	}
	decode(t, retried, &runEnvelope)
	retry := runEnvelope.Run
	if retry.RecordType != "retry" || retry.Attempt != 2 ||
		retry.OriginalRunID != original.ID || retry.ParentRunID != original.ID {
		t.Fatalf("retry = %+v", retry)
	}

	// The retry is immediately visible and ordered before the original's slot
	// by its own planned time.
	window = fixture.do(t, http.MethodGet,
		"/api/v1/runs?start=2026-01-01T10:00:00Z&end=2026-01-01T10:30:00Z", nil)
	decode(t, window, &result)
	if len(result.Executed) != 2 {
		t.Fatalf("executed after retry = %d", len(result.Executed))
	}
	if !result.Executed[0].PlannedFireTime.Equal(mustTime("2026-01-01T10:00:00Z")) ||
		!result.Executed[1].PlannedFireTime.Equal(mustTime("2026-01-01T10:05:00Z")) {
		t.Fatalf("order wrong: %+v", result.Executed)
	}
	if result.Executed[1].RecordType != "retry" {
		t.Fatalf("second record type = %q", result.Executed[1].RecordType)
	}
}

func TestUniqueErrorCodes(t *testing.T) {
	fixture := newAPIFixture(t, "2026-01-01T10:07:00Z")
	defer fixture.close()

	wantErrorCode(t, fixture.do(t, http.MethodGet, "/api/v1/tasks/missing", nil),
		http.StatusNotFound, "task_not_found")
	wantErrorCode(t, fixture.do(t, http.MethodDelete, "/api/v1/tasks/missing", nil),
		http.StatusNotFound, "task_not_found")
	wantErrorCode(t, fixture.do(t, http.MethodPost, "/api/v1/tasks/missing/trigger", nil),
		http.StatusNotFound, "task_not_found")

	invalidSchedule := fixture.do(t, http.MethodPost, "/api/v1/tasks", map[string]string{
		"name": "x", "schedule": "not a cron", "action": "succeed",
	})
	wantErrorCode(t, invalidSchedule, http.StatusBadRequest, "invalid_schedule")

	wantErrorCode(t, fixture.do(t, http.MethodGet,
		"/api/v1/runs?start=2026-01-02T00:00:00Z&end=2026-01-01T00:00:00Z", nil),
		http.StatusBadRequest, "invalid_time_window")
	wantErrorCode(t, fixture.do(t, http.MethodGet,
		"/api/v1/runs?start=2026-01-01T00:00:00Z&end=2026-06-01T00:00:00Z", nil),
		http.StatusBadRequest, "query_range_too_large")
	wantErrorCode(t, fixture.do(t, http.MethodGet,
		"/api/v1/runs?start=bogus&end=2026-01-01T00:00:00Z", nil),
		http.StatusBadRequest, "invalid_time_format")
	wantErrorCode(t, fixture.do(t, http.MethodGet, "/api/v1/runs?end=2026-01-01T00:00:00Z", nil),
		http.StatusBadRequest, "invalid_time_window")
	wantErrorCode(t, fixture.do(t, http.MethodPost, "/api/v1/runs/missing/retry", nil),
		http.StatusNotFound, "run_not_found")
}

func TestEmptyWindowReturnsSuccessWithArrays(t *testing.T) {
	fixture := newAPIFixture(t, "2026-01-01T10:07:00Z")
	defer fixture.close()
	recorder := fixture.do(t, http.MethodGet,
		"/api/v1/runs?start=2030-01-01T00:00:00Z&end=2030-01-02T00:00:00Z", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !bytes.Contains([]byte(body), []byte(`"pending":[]`)) ||
		!bytes.Contains([]byte(body), []byte(`"executed":[]`)) {
		t.Fatalf("empty arrays missing: %s", body)
	}
}

func TestValidateSchedule(t *testing.T) {
	fixture := newAPIFixture(t, "2026-01-01T10:07:00Z")
	defer fixture.close()
	recorder := fixture.do(t, http.MethodPost, "/api/v1/tasks/schedules/validate",
		map[string]string{"schedule": "*/15 * * * *"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("validate = %d %s", recorder.Code, recorder.Body.String())
	}
	wantErrorCode(t, fixture.do(t, http.MethodPost, "/api/v1/tasks/schedules/validate",
		map[string]string{"schedule": "99 * * * *"}), http.StatusBadRequest, "invalid_schedule")
}

func TestSuccessfulRunHasNoFailureReasonField(t *testing.T) {
	fixture := newAPIFixture(t, "2026-01-01T10:07:00Z")
	defer fixture.close()
	fixture.do(t, http.MethodPost, "/api/v1/tasks", map[string]string{
		"id": "ok", "name": "ok", "schedule": "*/15 * * * *", "action": "succeed",
	})
	recorder := fixture.do(t, http.MethodPost, "/api/v1/tasks/ok/trigger", nil)
	if bytes.Contains(recorder.Body.Bytes(), []byte("failure_reason")) {
		t.Fatalf("success response contains failure reason: %s", recorder.Body.String())
	}
}

func mustTime(ts string) time.Time {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	return parsed.UTC()
}
