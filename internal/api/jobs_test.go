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

func newTestRouter(t *testing.T, fixed time.Time) (*ginTest, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	clock := time.Now
	if !fixed.IsZero() {
		clock = func() time.Time { return fixed }
	}
	h := &Handlers{store: st, clock: clock}
	return &ginTest{router: NewRouterWithHandlers(h)}, st
}

type ginTest struct {
	router http.Handler
}

func (g *ginTest) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	g.router.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, rec, &body)
	if body.Error.Code == "" {
		t.Fatalf("missing error code in %q", rec.Body.String())
	}
	return body.Error.Code
}

func createJob(t *testing.T, g *ginTest, name, expr string) string {
	t.Helper()
	rec := g.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{"name": name, "cron_expression": expr})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create job status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decode(t, rec, &body)
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("missing job id: %s", rec.Body.String())
	}
	return id
}

func TestCreateJobReturnsScheduleInfo(t *testing.T) {
	now := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	g, _ := newTestRouter(t, now)
	rec := g.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{
		"name": "hourly", "cron_expression": "0 * * * *",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decode(t, rec, &body)
	if body["next_scheduled_for"] != "2026-10-01T09:00:00Z" {
		t.Fatalf("next_scheduled_for = %v", body["next_scheduled_for"])
	}
	if _, ok := body["cron_expression"]; !ok {
		t.Fatalf("cron_expression missing: %v", body)
	}
}

func TestCreateAndUpdateValidation(t *testing.T) {
	now := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	g, _ := newTestRouter(t, now)

	rec := g.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{"name": "", "cron_expression": "0 * * * *"})
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != codeInvalidName {
		t.Fatalf("empty name: %d %s", rec.Code, rec.Body.String())
	}
	rec = g.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{"name": "x", "cron_expression": "bogus"})
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != codeInvalidExpression {
		t.Fatalf("bad expression: %d %s", rec.Code, rec.Body.String())
	}
	rec = g.do(t, http.MethodPost, "/api/v1/jobs", map[string]any{"name": "x", "cron_expression": "0 0 31 2 *"})
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != codeInvalidExpression {
		t.Fatalf("impossible expression should be rejected: %d %s", rec.Code, rec.Body.String())
	}

	id := createJob(t, g, "n", "0 * * * *")
	rec = g.do(t, http.MethodPut, "/api/v1/jobs/"+id, map[string]any{"name": "n2", "cron_expression": "30 * * * *"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decode(t, rec, &body)
	if body["name"] != "n2" || body["next_scheduled_for"] != "2026-10-01T09:30:00Z" {
		t.Fatalf("unexpected update response: %v", body)
	}
}

func TestJobNotFoundUnique(t *testing.T) {
	g, _ := newTestRouter(t, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC))
	rec := g.do(t, http.MethodGet, "/api/v1/jobs/missing-id", nil)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != codeJobNotFound {
		t.Fatalf("get missing: %d %s", rec.Code, rec.Body.String())
	}
	rec = g.do(t, http.MethodDelete, "/api/v1/jobs/missing-id", nil)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != codeJobNotFound {
		t.Fatalf("delete missing: %d %s", rec.Code, rec.Body.String())
	}
	rec = g.do(t, http.MethodPost, "/api/v1/jobs/missing-id/runs", map[string]any{"result": "success"})
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != codeJobNotFound {
		t.Fatalf("trigger missing: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTriggerRunSuccessAndFailure(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	g, _ := newTestRouter(t, now)
	id := createJob(t, g, "j", "0 * * * *")

	rec := g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{"result": "success"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("success run: %d %s", rec.Code, rec.Body.String())
	}
	var okRun map[string]any
	decode(t, rec, &okRun)
	if okRun["failure_reason"] != nil {
		t.Fatalf("success run must not fabricate a failure reason: %v", okRun["failure_reason"])
	}
	if okRun["record_type"] != "original" || okRun["trigger_type"] != "scheduled" {
		t.Fatalf("unexpected typing: %v", okRun)
	}
	if okRun["scheduled_for"] != "2026-10-01T09:00:00Z" || okRun["triggered_at"] != "2026-10-01T09:00:00Z" {
		t.Fatalf("default timing wrong: %v", okRun)
	}
	if okRun["next_scheduled_for"] != "2026-10-01T10:00:00Z" {
		t.Fatalf("next_scheduled_for = %v", okRun["next_scheduled_for"])
	}

	rec = g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{"result": "failure"})
	if errorCode(t, rec) != codeInvalidFailureReason {
		t.Fatalf("failure without reason: %d %s", rec.Code, rec.Body.String())
	}
	rec = g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{"result": "weird"})
	if errorCode(t, rec) != codeInvalidResult {
		t.Fatalf("bad result: %d %s", rec.Code, rec.Body.String())
	}
	rec = g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{"result": "success", "failure_reason": "nope"})
	if errorCode(t, rec) != codeInvalidFailureReason {
		t.Fatalf("success with reason: %d %s", rec.Code, rec.Body.String())
	}

	rec = g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{
		"scheduled_for": "2026-10-01T09:00:00Z", "result": "failure", "failure_reason": "disk full",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("failure run: %d %s", rec.Code, rec.Body.String())
	}
	var failRun map[string]any
	decode(t, rec, &failRun)
	if failRun["failure_reason"] != "disk full" {
		t.Fatalf("failure reason = %v", failRun["failure_reason"])
	}
}
