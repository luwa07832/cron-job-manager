package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func createTestJob(t *testing.T, f *apiFixture, name string) string {
	t.Helper()
	body := validCreateBody()
	body["name"] = name
	recorder := f.request(http.MethodPost, "/api/v1/jobs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create job: %d %s", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)["id"].(string)
}

func runBody(scheduled, started, finished, outcome string, withError bool) map[string]any {
	body := map[string]any{
		"started_at":  started,
		"finished_at": finished,
		"outcome":     outcome,
	}
	if scheduled != "" {
		body["scheduled_for"] = scheduled
	}
	if withError {
		body["error"] = "boom"
	}
	return body
}

func recordRun(t *testing.T, f *apiFixture, jobID string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body)
	return recorder, decodeBody(t, recorder)
}

func attemptsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["attempts"].([]any)
	if !ok {
		t.Fatalf("missing attempts: %v", body)
	}
	attempts := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("attempt not an object: %v", item)
		}
		attempts = append(attempts, entry)
	}
	return attempts
}

func TestCreateRunAndRetryChain(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "chain")

	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:05Z", "2026-10-01T09:00:10Z", "failed", true))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create run = %d %s", recorder.Code, recorder.Body.String())
	}
	created := decodeBody(t, recorder)
	runID, _ := created["run_id"].(string)
	if runID == "" || created["job_id"] != jobID {
		t.Fatalf("bad run identity: %s", recorder.Body.String())
	}
	if created["scheduled_for"] != "2026-10-01T09:00:00Z" {
		t.Fatalf("scheduled_for = %v", created["scheduled_for"])
	}
	first := attemptsOf(t, created)
	if len(first) != 1 {
		t.Fatalf("attempts = %d", len(first))
	}
	if first[0]["attempt"].(float64) != 1 || first[0]["outcome"] != "failed" || first[0]["error"] != "boom" {
		t.Fatalf("first attempt = %v", first[0])
	}

	retry := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		runBody("", "2026-10-01T09:05:00Z", "2026-10-01T09:05:02Z", "succeeded", false))
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry = %d %s", retry.Code, retry.Body.String())
	}
	retried := decodeBody(t, retry)
	chain := attemptsOf(t, retried)
	if len(chain) != 2 {
		t.Fatalf("attempts after retry = %d", len(chain))
	}
	if chain[0]["attempt"].(float64) != 1 || chain[1]["attempt"].(float64) != 2 {
		t.Fatalf("attempt order = %v, %v", chain[0]["attempt"], chain[1]["attempt"])
	}
	if chain[1]["outcome"] != "succeeded" || chain[1]["error"] != nil {
		t.Fatalf("retry result = %v", chain[1])
	}
	if chain[0]["scheduled_for"] != chain[1]["scheduled_for"] {
		t.Fatalf("retry scheduled_for drifted: %v vs %v", chain[0]["scheduled_for"], chain[1]["scheduled_for"])
	}

	got := f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/"+runID, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("get run = %d %s", got.Code, got.Body.String())
	}
	fetched := attemptsOf(t, decodeBody(t, got))
	if len(fetched) != 2 || fetched[1]["outcome"] != "succeeded" {
		t.Fatalf("fetched chain = %s", got.Body.String())
	}
}

func TestRetryOnlyLatestFailed(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "retry gate")

	_, body := recordRun(t, f, jobID,
		runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:00Z", "2026-10-01T09:00:01Z", "succeeded", false))
	runID := body["run_id"].(string)

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		runBody("", "2026-10-01T09:05:00Z", "2026-10-01T09:05:01Z", "failed", true)),
		http.StatusConflict, "retry_not_allowed")

	// Failure chain: failed -> failed retry allowed, then success closes it.
	_, body = recordRun(t, f, jobID,
		runBody("2026-10-02T09:00:00Z", "2026-10-02T09:00:00Z", "2026-10-02T09:00:01Z", "failed", true))
	failingRun := body["run_id"].(string)
	retry := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+failingRun+"/retries",
		runBody("", "2026-10-02T09:05:00Z", "2026-10-02T09:05:01Z", "failed", true))
	if retry.Code != http.StatusCreated {
		t.Fatalf("failed retry = %d %s", retry.Code, retry.Body.String())
	}
	recovered := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+failingRun+"/retries",
		runBody("", "2026-10-02T09:10:00Z", "2026-10-02T09:10:01Z", "succeeded", false))
	if recovered.Code != http.StatusCreated {
		t.Fatalf("recover retry = %d %s", recovered.Code, recovered.Body.String())
	}
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+failingRun+"/retries",
		runBody("", "2026-10-02T09:15:00Z", "2026-10-02T09:15:01Z", "failed", true)),
		http.StatusConflict, "retry_not_allowed")
}

func TestRunValidationErrors(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "validation")

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("2026-10-01T09:01:00Z", "2026-10-01T09:00:00Z", "2026-10-01T09:00:01Z", "succeeded", false)),
		http.StatusUnprocessableEntity, "run_time_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:05Z", "2026-10-01T09:00:04Z", "succeeded", false)),
		http.StatusUnprocessableEntity, "run_time_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("not-a-time", "2026-10-01T09:00:05Z", "2026-10-01T09:00:06Z", "succeeded", false)),
		http.StatusUnprocessableEntity, "run_time_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("", "2026-10-01T09:00:05Z", "2026-10-01T09:00:06Z", "succeeded", false)),
		http.StatusUnprocessableEntity, "run_time_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:05Z", "2026-10-01T09:00:06Z", "succeeded", true)),
		http.StatusUnprocessableEntity, "run_outcome_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:05Z", "2026-10-01T09:00:06Z", "failed", false)),
		http.StatusUnprocessableEntity, "run_outcome_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:05Z", "2026-10-01T09:00:06Z", "weird", false)),
		http.StatusUnprocessableEntity, "run_outcome_invalid")

	body := map[string]any{
		"scheduled_for": "2026-10-01T09:00:00Z",
		"started_at":    "2026-10-01T09:00:05Z",
		"finished_at":   "2026-10-01T09:00:06Z",
		"outcome":       "failed",
		"error":         "   ",
	}
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body),
		http.StatusUnprocessableEntity, "run_outcome_invalid")

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", "{bad json"),
		http.StatusBadRequest, "invalid_json")
}

func TestRunBoundariesAreAllowed(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "boundaries")

	body := runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:00Z", "2026-10-01T09:00:00Z", "succeeded", false)
	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("equal instants = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestRunOffsetNormalizedToUTC(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "offset")

	body := map[string]any{
		"scheduled_for": "2026-10-01T17:00:00+08:00",
		"started_at":    "2026-10-01T17:00:00+08:00",
		"finished_at":   "2026-10-01T17:00:01+08:00",
		"outcome":       "succeeded",
	}
	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("offset run = %d %s", recorder.Code, recorder.Body.String())
	}
	first := attemptsOf(t, decodeBody(t, recorder))[0]
	if first["scheduled_for"] != "2026-10-01T09:00:00Z" || first["started_at"] != "2026-10-01T09:00:00Z" {
		t.Fatalf("not normalized to UTC: %v", first)
	}
}

func TestRunUnknownJobAndRun(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "known")

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/missing/runs",
		runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:00Z", "2026-10-01T09:00:01Z", "succeeded", false)),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/missing/runs/whatever", nil),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/missing/runs/whatever/retries",
		runBody("", "2026-10-01T09:00:00Z", "2026-10-01T09:00:01Z", "succeeded", false)),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/missing", nil),
		http.StatusNotFound, "run_not_found")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/missing/retries",
		runBody("", "2026-10-01T09:00:00Z", "2026-10-01T09:00:01Z", "failed", true)),
		http.StatusNotFound, "run_not_found")
}

func TestSoftDeletedJobKeepsHistoryButRejectsRecords(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "doomed")
	_, created := recordRun(t, f, jobID,
		runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:00Z", "2026-10-01T09:00:01Z", "failed", true))
	runID := created["run_id"].(string)

	deleted := f.request(http.MethodDelete, "/api/v1/jobs/"+jobID, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", deleted.Code)
	}

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("2026-10-02T09:00:00Z", "2026-10-02T09:00:00Z", "2026-10-02T09:00:01Z", "succeeded", false)),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		runBody("", "2026-10-02T09:00:00Z", "2026-10-02T09:00:01Z", "succeeded", false)),
		http.StatusNotFound, "job_not_found")

	history := f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/"+runID, nil)
	if history.Code != http.StatusOK {
		t.Fatalf("history after delete = %d %s", history.Code, history.Body.String())
	}
	if len(attemptsOf(t, decodeBody(t, history))) != 1 {
		t.Fatalf("history lost after delete: %s", history.Body.String())
	}

	window := "/api/v1/jobs?state=executed&from=2026-10-01T00:00:00Z&to=2026-10-02T00:00:00Z"
	listed := f.request(http.MethodGet, window, nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("executed window = %d %s", listed.Code, listed.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(listed.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0]["job_id"] != jobID {
		t.Fatalf("deleted job run missing from executed window: %s", listed.Body.String())
	}
}

func TestExecutedWindowOrderAndShape(t *testing.T) {
	f := newAPIFixture(t)
	jobA := createTestJob(t, f, "aaa")
	jobB := createTestJob(t, f, "bbb")

	mkRun := func(jobID, scheduled, outcome string, withError bool) string {
		t.Helper()
		started := scheduled
		finished := "2026-10-03T00:00:00Z"
		recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
			runBody(scheduled, started, finished, outcome, withError))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("run %s: %d %s", scheduled, recorder.Code, recorder.Body.String())
		}
		return decodeBody(t, recorder)["run_id"].(string)
	}
	retryOK := func(jobID, runID, started, finished, outcome string) {
		t.Helper()
		recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
			runBody("", started, finished, outcome, outcome == "failed"))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("retry: %d %s", recorder.Code, recorder.Body.String())
		}
	}

	// Job A has one failed run at 10:00 with a retry at 11:00; job B has a
	// successful run at 10:00 and another at 11:00.
	runA := mkRun(jobA, "2026-10-01T10:00:00Z", "failed", true)
	retryOK(jobA, runA, "2026-10-01T10:05:00Z", "2026-10-01T10:05:01Z", "succeeded")
	mkRun(jobB, "2026-10-01T10:00:00Z", "succeeded", false)
	mkRun(jobB, "2026-10-01T11:00:00Z", "succeeded", false)
	// Outside the window.
	mkRun(jobB, "2026-10-02T11:00:00Z", "succeeded", false)

	target := "/api/v1/jobs?state=executed&from=2026-10-01T00:00:00Z&to=2026-10-02T00:00:00Z"
	recorder := f.request(http.MethodGet, target, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("window = %d %s", recorder.Code, recorder.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4: %s", len(rows), recorder.Body.String())
	}

	type key struct {
		scheduled string
		jobID     string
		runID     string
		attempt   int
	}
	got := make([]key, len(rows))
	for i, row := range rows {
		for _, field := range []string{"job_id", "run_id", "scheduled_for", "started_at", "finished_at", "outcome"} {
			if _, ok := row[field]; !ok {
				t.Fatalf("row %d missing %s: %v", i, field, row)
			}
		}
		if _, ok := row["attempt"].(float64); !ok {
			t.Fatalf("row %d missing attempt: %v", i, row)
		}
		if _, ok := row["error"]; !ok {
			t.Fatalf("row %d missing error key: %v", i, row)
		}
		got[i] = key{
			scheduled: row["scheduled_for"].(string),
			jobID:     row["job_id"].(string),
			runID:     row["run_id"].(string),
			attempt:   int(row["attempt"].(float64)),
		}
	}
	// 10:00 window groups A's two attempts, ordered against B by lexicographic
	// job_id (not by creation order).
	first, second := jobA, jobB
	if jobB < jobA {
		first, second = jobB, jobA
	}
	if got[0].scheduled != "2026-10-01T10:00:00Z" || got[0].jobID != first || got[0].attempt != 1 {
		t.Fatalf("row 0 = %+v", got[0])
	}
	if first == jobA {
		if got[1].runID != runA || got[1].attempt != 2 {
			t.Fatalf("row 1 = %+v", got[1])
		}
		if got[2].scheduled != "2026-10-01T10:00:00Z" || got[2].jobID != second {
			t.Fatalf("row 2 = %+v", got[2])
		}
	} else {
		if got[1].scheduled != "2026-10-01T10:00:00Z" || got[1].jobID != second || got[1].attempt != 1 {
			t.Fatalf("row 1 = %+v", got[1])
		}
		if got[2].runID != runA || got[2].attempt != 2 {
			t.Fatalf("row 2 = %+v", got[2])
		}
	}
	if got[3].scheduled != "2026-10-01T11:00:00Z" || got[3].jobID != jobB {
		t.Fatalf("row 3 = %+v", got[3])
	}

	// Half-open right boundary.
	boundary := f.request(http.MethodGet,
		"/api/v1/jobs?state=executed&from=2026-10-01T00:00:00Z&to=2026-10-01T10:00:00Z", nil)
	var earlier []map[string]any
	if err := json.Unmarshal(boundary.Body.Bytes(), &earlier); err != nil {
		t.Fatalf("decode boundary: %v", err)
	}
	if len(earlier) != 0 {
		t.Fatalf("boundary rows = %d", len(earlier))
	}
}

func TestExecutedWindowValidationAndEmpty(t *testing.T) {
	f := newAPIFixture(t)
	from := "2026-10-01T00:00:00Z"
	to := "2026-10-02T00:00:00Z"

	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=executed&to="+to, nil),
		http.StatusBadRequest, "time_window_required")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=executed&from="+from, nil),
		http.StatusBadRequest, "time_window_required")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=executed&from=nope&to="+to, nil),
		http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=executed&from="+to+"&to="+from, nil),
		http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=bogus&from="+from+"&to="+to, nil),
		http.StatusBadRequest, "time_window_invalid")

	empty := f.request(http.MethodGet, "/api/v1/jobs?state=executed&from="+from+"&to="+to, nil)
	if empty.Code != http.StatusOK || empty.Body.String() != "[]" {
		t.Fatalf("empty = %d %s", empty.Code, empty.Body.String())
	}
}

func TestPendingSemanticsUnchanged(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "still pending")

	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runBody("2026-10-01T09:00:00Z", "2026-10-01T09:00:00Z", "2026-10-01T09:00:01Z", "succeeded", false))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("record run = %s", recorder.Body.String())
	}
	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	pending := f.request(http.MethodGet, "/api/v1/jobs?state=pending&from="+from+"&to="+to, nil)
	var rows []map[string]any
	if err := json.Unmarshal(pending.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	if len(rows) != 1 || rows[0]["id"] != jobID {
		t.Fatalf("pending query changed: %s", pending.Body.String())
	}
}
