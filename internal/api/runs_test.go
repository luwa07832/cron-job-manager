package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func createTestJob(t *testing.T, f *apiFixture, name string) string {
	t.Helper()
	body := validCreateBody()
	body["name"] = name
	recorder := f.request(http.MethodPost, "/api/v1/jobs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create job = %d %s", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)["id"].(string)
}

func runInput(scheduled, started, finished, outcome string, errText any) map[string]any {
	body := map[string]any{
		"scheduled_for": scheduled,
		"started_at":    started,
		"finished_at":   finished,
		"outcome":       outcome,
	}
	if errText != nil {
		body["error"] = errText
	}
	return body
}

func retryInput(started, finished, outcome string, errText any) map[string]any {
	body := map[string]any{
		"started_at":  started,
		"finished_at": finished,
		"outcome":     outcome,
	}
	if errText != nil {
		body["error"] = errText
	}
	return body
}

func TestCreateRunLifecycleAndRetries(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "nightly")

	scheduled := "2026-10-02T01:30:00Z"
	started := "2026-10-02T01:30:05Z"
	finished := "2026-10-02T01:31:00Z"

	created := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runInput(scheduled, started, finished, "failed", "exit status 1"))
	if created.Code != http.StatusCreated {
		t.Fatalf("create run = %d %s", created.Code, created.Body.String())
	}
	payload := decodeBody(t, created)
	runID, _ := payload["run_id"].(string)
	if runID == "" || payload["job_id"] != jobID || payload["scheduled_for"] != scheduled {
		t.Fatalf("unexpected payload: %s", created.Body.String())
	}
	results, _ := payload["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %s", created.Body.String())
	}
	first := results[0].(map[string]any)
	if first["attempt"].(float64) != 1 || first["outcome"] != "failed" ||
		first["started_at"] != started || first["finished_at"] != finished ||
		first["error"] != "exit status 1" {
		t.Fatalf("unexpected first result: %s", created.Body.String())
	}

	// Retry the failure; attempt increments and schedule stays on the run.
	retried := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		retryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z", "failed", "still broken"))
	if retried.Code != http.StatusCreated {
		t.Fatalf("retry = %d %s", retried.Code, retried.Body.String())
	}
	retryBody := decodeBody(t, retried)
	if retryBody["scheduled_for"] != scheduled {
		t.Fatalf("scheduled_for changed: %s", retried.Body.String())
	}
	results, _ = retryBody["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %s", retried.Body.String())
	}
	if results[0].(map[string]any)["attempt"].(float64) != 1 ||
		results[1].(map[string]any)["attempt"].(float64) != 2 ||
		results[1].(map[string]any)["error"] != "still broken" {
		t.Fatalf("retry results = %s", retried.Body.String())
	}

	// Final retry succeeds; error is null and further retries are forbidden.
	succeeded := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		retryInput("2026-10-02T03:30:00Z", "2026-10-02T03:31:00Z", "succeeded", nil))
	if succeeded.Code != http.StatusCreated {
		t.Fatalf("succeed retry = %d %s", succeeded.Code, succeeded.Body.String())
	}
	succeededBody := decodeBody(t, succeeded)
	results, _ = succeededBody["results"].([]any)
	last := results[2].(map[string]any)
	if value, exists := last["error"]; !exists || value != nil {
		t.Fatalf("success error = %v", value)
	}

	denied := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		retryInput("2026-10-02T04:30:00Z", "2026-10-02T04:31:00Z", "failed", "nope"))
	expectError(t, denied, http.StatusConflict, "retry_not_allowed")

	// GET returns the same ordered history.
	fetched := f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/"+runID, nil)
	if fetched.Code != http.StatusOK {
		t.Fatalf("get run = %d %s", fetched.Code, fetched.Body.String())
	}
	fetchedBody := decodeBody(t, fetched)
	if fetchedBody["scheduled_for"] != scheduled {
		t.Fatalf("get scheduled_for = %v", fetchedBody["scheduled_for"])
	}
	results, _ = fetchedBody["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("get results = %s", fetched.Body.String())
	}
	for i, result := range results {
		if result.(map[string]any)["attempt"].(float64) != float64(i+1) {
			t.Fatalf("attempts not ascending: %s", fetched.Body.String())
		}
	}
}

func TestCreateRunSuccessOmitsError(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "ok")
	body := runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
		"2026-10-02T01:31:00Z", "succeeded", nil)
	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeBody(t, recorder)
	result := payload["results"].([]any)[0].(map[string]any)
	if value, exists := result["error"]; !exists || value != nil {
		t.Fatalf("error = %v, want null", value)
	}
}

func TestCreateRunValidation(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "validated")

	scheduled := "2026-10-02T01:30:00Z"

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/unknown/runs",
		runInput(scheduled, scheduled, scheduled, "succeeded", nil)),
		http.StatusNotFound, "job_not_found")

	base := runInput(scheduled, "2026-10-02T01:30:05Z", "2026-10-02T01:31:00Z", "succeeded", nil)

	missingSchedule := map[string]any{}
	for k, v := range base {
		if k != "scheduled_for" {
			missingSchedule[k] = v
		}
	}
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", missingSchedule),
		http.StatusUnprocessableEntity, "run_time_invalid")

	badStart := runInput(scheduled, "2026-10-02T01:29:59Z", "2026-10-02T01:31:00Z", "succeeded", nil)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", badStart),
		http.StatusUnprocessableEntity, "run_time_invalid")

	badFinish := runInput(scheduled, scheduled, "2026-10-02T01:29:00Z", "succeeded", nil)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", badFinish),
		http.StatusUnprocessableEntity, "run_time_invalid")

	badTime := runInput("not-a-time", scheduled, scheduled, "succeeded", nil)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", badTime),
		http.StatusUnprocessableEntity, "run_time_invalid")

	equalTimes := runInput(scheduled, scheduled, scheduled, "succeeded", nil)
	if recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", equalTimes); recorder.Code != http.StatusCreated {
		t.Fatalf("equal timestamps = %d %s", recorder.Code, recorder.Body.String())
	}

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", "{bad"),
		http.StatusBadRequest, "invalid_json")

	unknownOutcome := runInput(scheduled, scheduled, scheduled, "crashed", nil)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", unknownOutcome),
		http.StatusUnprocessableEntity, "run_outcome_invalid")

	successWithError := runInput(scheduled, scheduled, scheduled, "succeeded", "unexpected")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", successWithError),
		http.StatusUnprocessableEntity, "run_outcome_invalid")

	successWithBlank := runInput(scheduled, scheduled, scheduled, "succeeded", "  ")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", successWithBlank),
		http.StatusUnprocessableEntity, "run_outcome_invalid")

	failedWithoutError := runInput(scheduled, scheduled, scheduled, "failed", nil)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", failedWithoutError),
		http.StatusUnprocessableEntity, "run_outcome_invalid")

	failedBlank := runInput(scheduled, scheduled, scheduled, "failed", "   ")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", failedBlank),
		http.StatusUnprocessableEntity, "run_outcome_invalid")

	// Offset-bearing RFC3339 is accepted and rendered back as UTC.
	offset := runInput("2026-10-02T09:30:00+08:00", "2026-10-02T09:30:05+08:00",
		"2026-10-02T09:31:00+08:00", "succeeded", nil)
	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", offset)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("offset run = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := decodeBody(t, recorder)["scheduled_for"]; got != "2026-10-02T01:30:00Z" {
		t.Fatalf("scheduled_for = %v", got)
	}
}

func TestRetryValidation(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "retry-job")
	scheduled := "2026-10-02T01:30:00Z"

	created := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runInput(scheduled, scheduled, "2026-10-02T01:31:00Z", "failed", "boom"))
	runID := decodeBody(t, created)["run_id"].(string)

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/nope/runs/"+runID+"/retries",
		retryInput(scheduled, scheduled, "succeeded", nil)),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/ghost/retries",
		retryInput(scheduled, scheduled, "succeeded", nil)),
		http.StatusNotFound, "run_not_found")

	tooEarly := retryInput("2026-10-02T01:29:00Z", "2026-10-02T01:30:00Z", "succeeded", nil)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries", tooEarly),
		http.StatusUnprocessableEntity, "run_time_invalid")
	badOutcome := retryInput(scheduled, scheduled, "weird", nil)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries", badOutcome),
		http.StatusUnprocessableEntity, "run_outcome_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries", "nope"),
		http.StatusBadRequest, "invalid_json")
}

func TestGetRunUnknownJobRunAndSoftDelete(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "history")
	scheduled := "2026-10-02T01:30:00Z"
	created := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runInput(scheduled, scheduled, scheduled, "succeeded", nil))
	runID := decodeBody(t, created)["run_id"].(string)

	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/ghost/runs/"+runID, nil),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/ghost", nil),
		http.StatusNotFound, "run_not_found")

	deleted := f.request(http.MethodDelete, "/api/v1/jobs/"+jobID, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", deleted.Code)
	}

	// New records are rejected after soft delete...
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		runInput(scheduled, scheduled, scheduled, "succeeded", nil)),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		retryInput(scheduled, scheduled, "succeeded", nil)),
		http.StatusNotFound, "job_not_found")

	// ...but the history itself remains readable.
	fetched := f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/"+runID, nil)
	if fetched.Code != http.StatusOK {
		t.Fatalf("history after delete = %d %s", fetched.Code, fetched.Body.String())
	}
}

func TestListExecutedWindow(t *testing.T) {
	f := newAPIFixture(t)
	jobA := createTestJob(t, f, "a")
	jobB := createTestJob(t, f, "b")

	create := func(jobID, runID, scheduled, outcome string, errText any) {
		t.Helper()
		recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
			runInput(scheduled, scheduled, scheduled, outcome, errText))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("create run = %d %s", recorder.Code, recorder.Body.String())
		}
		_ = runID
	}
	// Same scheduled_for ties broken by job_id.
	create(jobA, "ignored-a", "2026-10-02T01:30:00Z", "succeeded", nil)
	create(jobB, "ignored-b", "2026-10-02T01:30:00Z", "failed", "x")

	// Capture a generated run id for run_id ordering checks.
	created := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs",
		runInput("2026-10-03T01:30:00Z", "2026-10-03T01:30:00Z", "2026-10-03T01:30:00Z", "failed", "first"))
	multiRunID := decodeBody(t, created)["run_id"].(string)
	retried := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs/"+multiRunID+"/retries",
		retryInput("2026-10-03T01:30:00Z", "2026-10-03T01:30:00Z", "succeeded", nil))
	if retried.Code != http.StatusCreated {
		t.Fatalf("retry = %d %s", retried.Code, retried.Body.String())
	}

	target := "/api/v1/jobs?state=executed&from=2026-10-02T00:00:00Z&to=2026-10-03T00:00:00Z"
	recorder := f.request(http.MethodGet, target, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2: %s", len(rows), recorder.Body.String())
	}
	firstJob, secondJob := jobA, jobB
	if firstJob > secondJob {
		firstJob, secondJob = secondJob, firstJob
	}
	if rows[0]["job_id"] != firstJob || rows[1]["job_id"] != secondJob {
		t.Fatalf("order = %v, %v", rows[0]["job_id"], rows[1]["job_id"])
	}
	for _, row := range rows {
		for _, field := range []string{"scheduled_for", "run_id", "attempt", "started_at", "finished_at", "outcome"} {
			if _, ok := row[field]; !ok {
				t.Fatalf("missing %s in %s", field, recorder.Body.String())
			}
		}
		if _, ok := row["error"]; !ok {
			t.Fatalf("missing error in %s", recorder.Body.String())
		}
	}

	// Window covering the retried run: both attempts returned, attempt asc.
	target = "/api/v1/jobs?state=executed&from=2026-10-03T00:00:00Z&to=2026-10-04T00:00:00Z"
	recorder = f.request(http.MethodGet, target, nil)
	_ = json.Unmarshal(recorder.Body.Bytes(), &rows)
	if len(rows) != 2 || rows[0]["attempt"].(float64) != 1 || rows[1]["attempt"].(float64) != 2 {
		t.Fatalf("retry rows = %s", recorder.Body.String())
	}
	if rows[0]["run_id"] != multiRunID || rows[1]["run_id"] != multiRunID {
		t.Fatalf("run ids = %s", recorder.Body.String())
	}

	// No match yields an empty array, not null.
	empty := f.request(http.MethodGet, "/api/v1/jobs?state=executed&from=2030-01-01T00:00:00Z&to=2030-01-02T00:00:00Z", nil)
	if empty.Code != http.StatusOK || empty.Body.String() != "[]" {
		t.Fatalf("empty = %d %s", empty.Code, empty.Body.String())
	}
}

func TestListExecutedValidation(t *testing.T) {
	f := newAPIFixture(t)
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	from := now.Format(time.RFC3339)
	to := now.Add(time.Hour).Format(time.RFC3339)

	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=executed&to="+to, nil),
		http.StatusBadRequest, "time_window_required")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=executed&from="+from, nil),
		http.StatusBadRequest, "time_window_required")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=executed&from=x&to="+to, nil),
		http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=executed&from="+to+"&to="+from, nil),
		http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=wat&from="+from+"&to="+to, nil),
		http.StatusBadRequest, "time_window_invalid")

	// pending remains the default and keeps its own shape.
	recorder := f.request(http.MethodGet, "/api/v1/jobs?from="+from+"&to="+to, nil)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "[]" {
		t.Fatalf("default pending = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestListRunsForJob(t *testing.T) {
	f := newAPIFixture(t)
	jobA := createTestJob(t, f, "chain-a")
	jobB := createTestJob(t, f, "chain-b")

	create := func(jobID, scheduled, outcome string, errText any) string {
		t.Helper()
		recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
			runInput(scheduled, scheduled, scheduled, outcome, errText))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("create run = %d %s", recorder.Code, recorder.Body.String())
		}
		return decodeBody(t, recorder)["run_id"].(string)
	}

	failedRun := create(jobA, "2026-10-02T01:30:00Z", "failed", "exit status 1")
	create(jobA, "2026-10-03T01:30:00Z", "succeeded", nil)
	// A sibling job with runs in the same window must not leak into job A.
	create(jobB, "2026-10-02T01:30:00Z", "succeeded", nil)

	retried := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs/"+failedRun+"/retries",
		retryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z", "succeeded", nil))
	if retried.Code != http.StatusCreated {
		t.Fatalf("retry = %d %s", retried.Code, retried.Body.String())
	}

	target := "/api/v1/jobs/" + jobA + "/runs?from=2026-10-02T00:00:00Z&to=2026-10-04T00:00:00Z"
	recorder := f.request(http.MethodGet, target, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	var runs []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len = %d, want 2: %s", len(runs), recorder.Body.String())
	}
	first, second := runs[0], runs[1]
	if first["run_id"] != failedRun || first["job_id"] != jobA ||
		first["scheduled_for"] != "2026-10-02T01:30:00Z" {
		t.Fatalf("first run = %s", recorder.Body.String())
	}
	if second["job_id"] != jobA || second["scheduled_for"] != "2026-10-03T01:30:00Z" {
		t.Fatalf("second run = %s", recorder.Body.String())
	}

	// The retried run carries both attempts in ascending order.
	results, _ := first["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %s", recorder.Body.String())
	}
	attempt1 := results[0].(map[string]any)
	attempt2 := results[1].(map[string]any)
	if attempt1["attempt"].(float64) != 1 || attempt1["outcome"] != "failed" ||
		attempt1["error"] != "exit status 1" {
		t.Fatalf("attempt 1 = %s", recorder.Body.String())
	}
	if attempt2["attempt"].(float64) != 2 || attempt2["outcome"] != "succeeded" ||
		attempt2["error"] != nil {
		t.Fatalf("attempt 2 = %s", recorder.Body.String())
	}
	for _, field := range []string{"started_at", "finished_at"} {
		if _, ok := attempt1[field].(string); !ok {
			t.Fatalf("attempt 1 missing %s: %s", field, recorder.Body.String())
		}
	}

	// The window is half-open on the right.
	target = "/api/v1/jobs/" + jobA + "/runs?from=2026-10-02T00:00:00Z&to=2026-10-03T01:30:00Z"
	recorder = f.request(http.MethodGet, target, nil)
	if err := json.Unmarshal(recorder.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 || runs[0]["run_id"] != failedRun {
		t.Fatalf("half-open = %s", recorder.Body.String())
	}

	// No matching runs yields an empty array, not null.
	empty := f.request(http.MethodGet,
		"/api/v1/jobs/"+jobA+"/runs?from=2030-01-01T00:00:00Z&to=2030-01-02T00:00:00Z", nil)
	if empty.Code != http.StatusOK || empty.Body.String() != "[]" {
		t.Fatalf("empty = %d %s", empty.Code, empty.Body.String())
	}

	// Soft-deleted jobs keep their history readable through this entry too.
	deleted := f.request(http.MethodDelete, "/api/v1/jobs/"+jobA, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", deleted.Code)
	}
	recorder = f.request(http.MethodGet,
		"/api/v1/jobs/"+jobA+"/runs?from=2026-10-02T00:00:00Z&to=2026-10-04T00:00:00Z", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("history after delete = %d %s", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("history after delete = %s", recorder.Body.String())
	}
}

func TestListRunsForJobValidation(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "chain-validation")
	from := "2026-10-02T00:00:00Z"
	to := "2026-10-03T00:00:00Z"

	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs?to="+to, nil),
		http.StatusBadRequest, "time_window_required")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs?from="+from, nil),
		http.StatusBadRequest, "time_window_required")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs", nil),
		http.StatusBadRequest, "time_window_required")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs?from=x&to="+to, nil),
		http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs?from="+from+"&to=y", nil),
		http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs?from="+to+"&to="+from, nil),
		http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs?from="+from+"&to="+from, nil),
		http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/ghost/runs?from="+from+"&to="+to, nil),
		http.StatusNotFound, "job_not_found")
}
