package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/luwa07832/cron-job-manager/internal/store"
)

type testClient struct {
	t      *testing.T
	router http.Handler
}

func newTestClient(t *testing.T) (*testClient, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &testClient{t: t, router: NewRouter(st)}, st
}

func (tc *testClient) request(method, path string, body any) *httptest.ResponseRecorder {
	tc.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			tc.t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	tc.router.ServeHTTP(recorder, request)
	return recorder
}

func (tc *testClient) requestRaw(method, path, body string) *httptest.ResponseRecorder {
	tc.t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	tc.router.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
}

func expectError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, status, recorder.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeBody(t, recorder, &body)
	if body.Error.Code != code {
		t.Fatalf("code = %q, want %q", body.Error.Code, code)
	}
	if body.Error.Message == "" {
		t.Fatalf("error message is empty")
	}
}

func mustCreateJob(t *testing.T, tc *testClient, body map[string]any) map[string]any {
	t.Helper()
	recorder := tc.request(http.MethodPost, "/api/v1/jobs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body = %s", recorder.Code, recorder.Body.String())
	}
	var job map[string]any
	decodeBody(t, recorder, &job)
	return job
}

func TestCreateJobReturnsCreatedPayload(t *testing.T) {
	tc, _ := newTestClient(t)
	createdAt := time.Now()
	recorder := tc.request(http.MethodPost, "/api/v1/jobs", map[string]any{
		"name":       "nightly",
		"expression": "0 2 * * *",
		"timezone":   "Asia/Shanghai",
		"enabled":    true,
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var job map[string]any
	decodeBody(t, recorder, &job)

	for _, key := range []string{"id", "name", "expression", "timezone", "enabled", "created_at", "next_run"} {
		if _, ok := job[key]; !ok {
			t.Fatalf("response missing key %q: %v", key, job)
		}
	}
	if job["name"] != "nightly" || job["expression"] != "0 2 * * *" || job["timezone"] != "Asia/Shanghai" {
		t.Fatalf("unexpected identity fields: %v", job)
	}
	if job["enabled"] != true {
		t.Fatalf("enabled = %v, want true", job["enabled"])
	}
	nextRunRaw, ok := job["next_run"].(string)
	if !ok {
		t.Fatalf("next_run = %v, want string", job["next_run"])
	}
	nextRun, err := time.Parse(time.RFC3339, nextRunRaw)
	if err != nil {
		t.Fatalf("next_run is not RFC3339: %v", err)
	}
	if !nextRun.UTC().After(createdAt.UTC()) {
		t.Fatalf("next_run %s is not strictly after processing time %s", nextRun, createdAt)
	}
	// 02:00 Shanghai is 18:00 UTC on the previous day.
	if nextRun.UTC().Format("15:04") != "18:00" {
		t.Fatalf("next run utc time = %s, want 18:00", nextRun.UTC())
	}
}

func TestCreateDisabledJobHasNullNextRun(t *testing.T) {
	tc, _ := newTestClient(t)
	job := mustCreateJob(t, tc, map[string]any{
		"name":       "paused",
		"expression": "0 0 * * *",
		"timezone":   "UTC",
		"enabled":    false,
	})
	if job["next_run"] != nil {
		t.Fatalf("next_run = %v, want null", job["next_run"])
	}
}

func TestCreateValidationErrors(t *testing.T) {
	tc, _ := newTestClient(t)

	expectError(t, tc.requestRaw(http.MethodPost, "/api/v1/jobs", "{not json"), http.StatusBadRequest, "invalid_json")
	expectError(t, tc.requestRaw(http.MethodPost, "/api/v1/jobs", `{"name":"oops"}{"`), http.StatusBadRequest, "invalid_json")
	expectError(t, tc.requestRaw(http.MethodPost, "/api/v1/jobs", ""), http.StatusBadRequest, "invalid_json")

	recorder := tc.request(http.MethodPost, "/api/v1/jobs", map[string]any{
		"name": "   ", "expression": "0 0 * * *", "timezone": "UTC", "enabled": true,
	})
	expectError(t, recorder, http.StatusUnprocessableEntity, "name_required")

	recorder = tc.request(http.MethodPost, "/api/v1/jobs", map[string]any{
		"name": "bad-cron", "expression": "99 * * * *", "timezone": "UTC", "enabled": true,
	})
	expectError(t, recorder, http.StatusUnprocessableEntity, "schedule_invalid")

	recorder = tc.request(http.MethodPost, "/api/v1/jobs", map[string]any{
		"name": "bad-zone", "expression": "0 0 * * *", "timezone": "Mars/Olympus", "enabled": true,
	})
	expectError(t, recorder, http.StatusUnprocessableEntity, "timezone_invalid")

	recorder = tc.request(http.MethodPost, "/api/v1/jobs", map[string]any{
		"name": "impossible", "expression": "0 0 30 2 *", "timezone": "UTC", "enabled": true,
	})
	expectError(t, recorder, http.StatusUnprocessableEntity, "schedule_invalid")

	mustCreateJob(t, tc, map[string]any{
		"name": "dup", "expression": "0 0 * * *", "timezone": "UTC", "enabled": false,
	})
	recorder = tc.request(http.MethodPost, "/api/v1/jobs", map[string]any{
		"name": "dup", "expression": "0 0 * * *", "timezone": "UTC", "enabled": false,
	})
	expectError(t, recorder, http.StatusUnprocessableEntity, "name_conflict")
}

func TestGetUpdateAndDeleteJob(t *testing.T) {
	tc, _ := newTestClient(t)
	job := mustCreateJob(t, tc, map[string]any{
		"name":       "editable",
		"expression": "0 0 * * *",
		"timezone":   "UTC",
		"enabled":    true,
	})
	id := job["id"].(string)

	recorder := tc.request(http.MethodGet, "/api/v1/jobs/"+id, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var fetched map[string]any
	decodeBody(t, recorder, &fetched)
	if fetched["id"] != id {
		t.Fatalf("fetched id = %v, want %s", fetched["id"], id)
	}

	recorder = tc.request(http.MethodPatch, "/api/v1/jobs/"+id, map[string]any{"enabled": false})
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var disabled map[string]any
	decodeBody(t, recorder, &disabled)
	if disabled["next_run"] != nil {
		t.Fatalf("next_run after disable = %v, want null", disabled["next_run"])
	}
	if disabled["enabled"] != false {
		t.Fatalf("enabled = %v, want false", disabled["enabled"])
	}

	recorder = tc.request(http.MethodPatch, "/api/v1/jobs/"+id, map[string]any{
		"enabled":    true,
		"expression": "0 9 * * *",
		"timezone":   "Asia/Shanghai",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var enabled map[string]any
	decodeBody(t, recorder, &enabled)
	nextRunRaw, ok := enabled["next_run"].(string)
	if !ok {
		t.Fatalf("next_run after enable = %v, want string", enabled["next_run"])
	}
	nextRun, err := time.Parse(time.RFC3339, nextRunRaw)
	if err != nil {
		t.Fatalf("parse next_run: %v", err)
	}
	if nextRun.UTC().Format("15:04") != "01:00" {
		t.Fatalf("next run = %s, want 01:00 UTC", nextRun.UTC())
	}

	recorder = tc.request(http.MethodPatch, "/api/v1/jobs/"+id, map[string]any{"name": "renamed"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("rename status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	recorder = tc.request(http.MethodPatch, "/api/v1/jobs/"+id, map[string]any{"name": ""})
	expectError(t, recorder, http.StatusUnprocessableEntity, "name_required")
	recorder = tc.request(http.MethodPatch, "/api/v1/jobs/"+id, map[string]any{"expression": "x y z"})
	expectError(t, recorder, http.StatusUnprocessableEntity, "schedule_invalid")

	recorder = tc.request(http.MethodDelete, "/api/v1/jobs/"+id, nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("delete body = %q, want empty", recorder.Body.String())
	}
	expectError(t, tc.request(http.MethodGet, "/api/v1/jobs/"+id, nil), http.StatusNotFound, "job_not_found")
	expectError(t, tc.request(http.MethodDelete, "/api/v1/jobs/"+id, nil), http.StatusNotFound, "job_not_found")
	expectError(t, tc.request(http.MethodPatch, "/api/v1/jobs/"+id, nil), http.StatusNotFound, "job_not_found")
	expectError(t, tc.request(http.MethodGet, "/api/v1/jobs/not-an-id", nil), http.StatusNotFound, "job_not_found")
}

func TestListPendingJobsWindow(t *testing.T) {
	tc, _ := newTestClient(t)

	inWindow := mustCreateJob(t, tc, map[string]any{
		"name":       "jan-yearly",
		"expression": "0 0 1 1 *",
		"timezone":   "UTC",
		"enabled":    true,
	})
	disabled := mustCreateJob(t, tc, map[string]any{
		"name":       "disabled-jan",
		"expression": "0 0 1 1 *",
		"timezone":   "UTC",
		"enabled":    false,
	})
	if disabled["next_run"] != nil {
		t.Fatalf("disabled job next_run = %v", disabled["next_run"])
	}

	janNext, err := time.Parse(time.RFC3339, inWindow["next_run"].(string))
	if err != nil {
		t.Fatalf("parse jan next_run: %v", err)
	}
	marchNext, err := time.Parse(time.RFC3339, mustCreateJob(t, tc, map[string]any{
		"name":       "march-yearly",
		"expression": "0 0 1 3 *",
		"timezone":   "UTC",
		"enabled":    true,
	})["next_run"].(string))
	if err != nil {
		t.Fatalf("parse march next_run: %v", err)
	}
	_ = marchNext

	// Window covering only the January firing excludes the March job and disabled job.
	windowStart := janNext.Add(-time.Minute)
	windowEnd := janNext.Add(time.Minute)
	recorder := tc.request(http.MethodGet,
		"/api/v1/jobs?state=pending&from="+windowStart.Format(time.RFC3339)+
			"&to="+windowEnd.Format(time.RFC3339), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var jobs []map[string]any
	decodeBody(t, recorder, &jobs)
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1: %v", len(jobs), jobs)
	}
	if jobs[0]["name"] != "jan-yearly" {
		t.Fatalf("job name = %v, want jan-yearly", jobs[0]["name"])
	}

	// Both enabled jobs appear together and are ordered by next run ascending.
	nextRun := inWindow["next_run"].(string)
	farEnd := marchNext.Add(time.Minute)
	recorder = tc.request(http.MethodGet,
		"/api/v1/jobs?from="+nextRun+"&to="+farEnd.Format(time.RFC3339), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ordered status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	decodeBody(t, recorder, &jobs)
	if len(jobs) != 2 || jobs[0]["name"] != "jan-yearly" || jobs[1]["name"] != "march-yearly" {
		t.Fatalf("ordered result = %v", jobs)
	}

	// Left-closed: a boundary hit at from is included; ending before March keeps one row.
	recorder = tc.request(http.MethodGet,
		"/api/v1/jobs?from="+nextRun+"&to="+marchNext.Format(time.RFC3339), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("closed-left status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	decodeBody(t, recorder, &jobs)
	if len(jobs) != 1 || jobs[0]["name"] != "jan-yearly" {
		t.Fatalf("closed-left result = %v", jobs)
	}

	// Right-open: a boundary hit at to is excluded.
	recorder = tc.request(http.MethodGet,
		"/api/v1/jobs?from=2020-01-01T00:00:00Z&to="+nextRun, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("open-right status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	decodeBody(t, recorder, &jobs)
	if len(jobs) != 0 {
		t.Fatalf("open-right result = %v, want empty", jobs)
	}

	// No match returns an empty array, never null.
	if recorder.Body.String() != "[]" {
		t.Fatalf("empty body = %s, want []", recorder.Body.String())
	}
}

func TestListPendingJobsWindowErrors(t *testing.T) {
	tc, _ := newTestClient(t)
	cases := map[string]string{
		"/api/v1/jobs":                                                   "time_window_required",
		"/api/v1/jobs?from=2026-01-01T00:00:00Z":                         "time_window_required",
		"/api/v1/jobs?to=2026-01-01T00:00:00Z":                           "time_window_required",
		"/api/v1/jobs?from=nope&to=2026-01-01T00:00:00Z":                 "time_window_invalid",
		"/api/v1/jobs?from=2026-01-01T00:00:00Z&to=2026-01-01T00:00:00Z": "time_window_invalid",
		"/api/v1/jobs?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z": "time_window_invalid",
		"/api/v1/jobs?from=&to=":                                         "time_window_required",
	}
	for path, code := range cases {
		expectError(t, tc.request(http.MethodGet, path, nil), http.StatusBadRequest, code)
	}
}

func TestDeletedJobDropsOutOfWindow(t *testing.T) {
	tc, _ := newTestClient(t)
	job := mustCreateJob(t, tc, map[string]any{
		"name":       "delete-me",
		"expression": "0 0 1 1 *",
		"timezone":   "UTC",
		"enabled":    true,
	})
	id := job["id"].(string)
	if recorder := tc.request(http.MethodDelete, "/api/v1/jobs/"+id, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", recorder.Code)
	}
	recorder := tc.request(http.MethodGet,
		"/api/v1/jobs?from=2026-01-01T00:00:00Z&to=2030-01-01T00:00:00Z", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "[]" {
		t.Fatalf("body = %s, want []", recorder.Body.String())
	}
}
