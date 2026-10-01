package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestManualRunEntry(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 15, 0, 0, time.UTC)
	g, _ := newTestRouter(t, now)
	id := createJob(t, g, "j", "0 * * * *")
	rec := g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/manual-runs", map[string]any{"result": "success"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("manual run: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decode(t, rec, &body)
	if body["trigger_type"] != "manual" || body["record_type"] != "original" {
		t.Fatalf("unexpected typing: %v", body)
	}
}

func TestRetryFlowAndVisibility(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	g, _ := newTestRouter(t, now)
	id := createJob(t, g, "j", "0 * * * *")

	rec := g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{
		"scheduled_for": "2026-10-01T09:00:00Z", "result": "failure", "failure_reason": "boom",
	})
	var original map[string]any
	decode(t, rec, &original)
	seq := int64(original["seq"].(float64))

	rec = g.do(t, http.MethodPost, "/api/v1/runs/999999/retries", map[string]any{"result": "success"})
	if errorCode(t, rec) != codeRunNotFound {
		t.Fatalf("retry missing: %d %s", rec.Code, rec.Body.String())
	}
	rec = g.do(t, http.MethodPost, "/api/v1/runs/abc/retries", map[string]any{"result": "success"})
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != codeRunNotFound {
		t.Fatalf("bad seq: %d %s", rec.Code, rec.Body.String())
	}

	rec = g.do(t, http.MethodPost, "/api/v1/runs/"+itoa(seq)+"/retries", map[string]any{
		"scheduled_for": "2026-10-01T09:05:00Z", "result": "failure", "failure_reason": "again",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry 1: %d %s", rec.Code, rec.Body.String())
	}
	var retry1 map[string]any
	decode(t, rec, &retry1)
	if retry1["record_type"] != "retry" || retry1["retry_number"].(float64) != 1 {
		t.Fatalf("retry 1 typing: %v", retry1)
	}
	if retry1["retry_of_seq"].(float64) != float64(seq) {
		t.Fatalf("retry link: %v", retry1["retry_of_seq"])
	}
	if retry1["failure_reason"] != "again" {
		t.Fatalf("retry reason: %v", retry1["failure_reason"])
	}

	rec = g.do(t, http.MethodPost, "/api/v1/runs/"+itoa(seq)+"/retries", map[string]any{
		"scheduled_for": "2026-10-01T09:10:00Z", "result": "success",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry 2: %d %s", rec.Code, rec.Body.String())
	}
	var retry2 map[string]any
	decode(t, rec, &retry2)
	if retry2["retry_number"].(float64) != 2 {
		t.Fatalf("retry 2 number: %v", retry2["retry_number"])
	}

	// Records are immediately queryable and keep time order; retries are not merged away.
	rec = g.do(t, http.MethodGet,
		"/api/v1/run-records?start="+url.QueryEscape("2026-10-01T09:00:00Z")+
			"&end="+url.QueryEscape("2026-10-01T09:30:00Z"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("window: %d %s", rec.Code, rec.Body.String())
	}
	var window windowResponse
	decode(t, rec, &window)
	if len(window.Executed) != 3 {
		t.Fatalf("executed len = %d, want 3: %s", len(window.Executed), rec.Body.String())
	}
	if window.Executed[0].ScheduledFor != "2026-10-01T09:00:00Z" ||
		window.Executed[1].ScheduledFor != "2026-10-01T09:05:00Z" ||
		window.Executed[2].ScheduledFor != "2026-10-01T09:10:00Z" {
		t.Fatalf("wrong order: %+v", window.Executed)
	}
	if window.Executed[0].RecordType != "original" ||
		window.Executed[1].RecordType != "retry" ||
		window.Executed[2].RecordType != "retry" {
		t.Fatalf("types not distinguishable in order: %+v", window.Executed)
	}
	if window.Executed[0].RetryCount != 2 {
		t.Fatalf("original retry_count = %d, want 2", window.Executed[0].RetryCount)
	}
}

func TestRetryRejectsSuccessAndNestedRetry(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	g, _ := newTestRouter(t, now)
	id := createJob(t, g, "j", "0 * * * *")
	rec := g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{"result": "success"})
	var successRun map[string]any
	decode(t, rec, &successRun)
	seq := int64(successRun["seq"].(float64))
	rec = g.do(t, http.MethodPost, "/api/v1/runs/"+itoa(seq)+"/retries", map[string]any{"result": "success"})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != codeRetryNotRetryable {
		t.Fatalf("retry success: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWindowPendingAndExecutedSplit(t *testing.T) {
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	g, _ := newTestRouter(t, now)
	id := createJob(t, g, "hourly", "0 * * * *")

	rec := g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{
		"scheduled_for": "2026-10-01T10:00:00Z", "result": "success",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("run: %s", rec.Body.String())
	}

	rec = g.do(t, http.MethodGet,
		"/api/v1/run-records?start=2026-10-01T09:00:00Z&end=2026-10-01T12:00:00Z", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("window: %d %s", rec.Code, rec.Body.String())
	}
	var window windowResponse
	decode(t, rec, &window)
	wantPending := []string{
		"2026-10-01T09:00:00Z", "2026-10-01T11:00:00Z", "2026-10-01T12:00:00Z",
	}
	if len(window.Pending) != len(wantPending) {
		t.Fatalf("pending len = %d, want %d: %s", len(window.Pending), len(wantPending), rec.Body.String())
	}
	for i, item := range window.Pending {
		if item.RecordType != "pending" || item.ScheduledFor != wantPending[i] {
			t.Fatalf("pending[%d] = %+v, want %s", i, item, wantPending[i])
		}
	}
	if len(window.Executed) != 1 || window.Executed[0].ScheduledFor != "2026-10-01T10:00:00Z" {
		t.Fatalf("executed = %+v", window.Executed)
	}
}

func TestWindowClosedIntervalAndTimezoneEquivalence(t *testing.T) {
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	g, _ := newTestRouter(t, now)
	id := createJob(t, g, "hourly", "0 * * * *")
	g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{
		"scheduled_for": "2026-10-01T10:00:00Z", "result": "success",
	})

	// Same instant expressed with +08:00 must produce the same executed set.
	path := "/api/v1/run-records?start=" + url.QueryEscape("2026-10-01T18:00:00+08:00") +
		"&end=" + url.QueryEscape("2026-10-01T18:00:00+08:00")
	rec := g.do(t, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("window: %d %s", rec.Code, rec.Body.String())
	}
	var window windowResponse
	decode(t, rec, &window)
	if window.WindowStart != "2026-10-01T10:00:00Z" || window.WindowEnd != "2026-10-01T10:00:00Z" {
		t.Fatalf("bounds not normalised to UTC: %+v", window)
	}
	if len(window.Executed) != 1 {
		t.Fatalf("closed interval boundary hit len = %d: %s", len(window.Executed), rec.Body.String())
	}
}

func TestWindowErrors(t *testing.T) {
	g, _ := newTestRouter(t, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC))

	cases := []struct {
		name string
		path string
		code string
	}{
		{"start after end", "/api/v1/run-records?start=2026-10-02T00:00:00Z&end=2026-10-01T00:00:00Z", codeInvalidWindow},
		{"bad start", "/api/v1/run-records?start=nope&end=2026-10-01T00:00:00Z", codeInvalidTime},
		{"missing end", "/api/v1/run-records?start=2026-10-01T00:00:00Z", codeInvalidWindow},
		{"too wide", "/api/v1/run-records?start=2026-01-01T00:00:00Z&end=2027-02-02T00:00:00Z", codeWindowTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := g.do(t, http.MethodGet, tc.path, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if got := errorCode(t, rec); got != tc.code {
				t.Fatalf("code = %s, want %s", got, tc.code)
			}
		})
	}
}

func TestWindowEmptyIsSuccessful(t *testing.T) {
	g, _ := newTestRouter(t, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC))
	rec := g.do(t, http.MethodGet, "/api/v1/run-records?start=2030-01-01T00:00:00Z&end=2030-01-02T00:00:00Z", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var window windowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &window); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if window.Pending == nil || window.Executed == nil {
		t.Fatalf("empty lists must be present, not null: %s", rec.Body.String())
	}
	if len(window.Pending) != 0 || len(window.Executed) != 0 {
		t.Fatalf("expected empty lists: %+v", window)
	}
}

func TestDuplicateTriggerCreatesIndependentRecords(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	g, _ := newTestRouter(t, now)
	id := createJob(t, g, "j", "0 * * * *")
	for i := 0; i < 2; i++ {
		rec := g.do(t, http.MethodPost, "/api/v1/jobs/"+id+"/runs", map[string]any{
			"scheduled_for": "2026-10-01T09:00:00Z", "result": "success",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("trigger %d: %s", i, rec.Body.String())
		}
	}
	rec := g.do(t, http.MethodGet, "/api/v1/run-records?start=2026-10-01T09:00:00Z&end=2026-10-01T09:00:00Z", nil)
	var window windowResponse
	decode(t, rec, &window)
	if len(window.Executed) != 2 {
		t.Fatalf("executed len = %d, want 2 independent records", len(window.Executed))
	}
	if len(window.Pending) != 0 {
		t.Fatalf("executed grid time must not be pending, got %+v", window.Pending)
	}
}

func TestInvalidJSONShape(t *testing.T) {
	g, _ := newTestRouter(t, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	g.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != codeInvalidJSON {
		t.Fatalf("invalid json: %d %s", rec.Code, rec.Body.String())
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
