package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func withKey(body map[string]any, key any) map[string]any {
	copied := make(map[string]any, len(body)+1)
	for k, v := range body {
		copied[k] = v
	}
	copied["idempotency_key"] = key
	return copied
}

func createFailedRunWithKey(t *testing.T, f *apiFixture, jobID, key string) string {
	t.Helper()
	first := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
			"2026-10-02T01:31:00Z", "failed", "exit status 1"), key))
	if first.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", first.Code, first.Body.String())
	}
	return decodeBody(t, first)["run_id"].(string)
}

func TestCreateRunIdempotentReplay(t *testing.T) {
	f := newAPIFixture(t)
	jobA := createTestJob(t, f, "idem-a")
	jobB := createTestJob(t, f, "idem-b")

	body := runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
		"2026-10-02T01:31:00Z", "failed", "exit status 1")

	first := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs", withKey(body, "event-1"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	replay := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs", withKey(body, "event-1"))
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body.String())
	}
	if first.Body.String() != replay.Body.String() {
		t.Fatalf("bodies differ:\nfirst  %s\nreplay %s", first.Body.String(), replay.Body.String())
	}
	if decodeBody(t, first)["run_id"] != decodeBody(t, replay)["run_id"] {
		t.Fatalf("run_id changed on replay")
	}

	// The same key on another job is an independent reservation.
	otherJob := f.request(http.MethodPost, "/api/v1/jobs/"+jobB+"/runs", withKey(body, "event-1"))
	if otherJob.Code != http.StatusCreated {
		t.Fatalf("other job = %d %s", otherJob.Code, otherJob.Body.String())
	}
	if decodeBody(t, otherJob)["run_id"] == decodeBody(t, first)["run_id"] {
		t.Fatalf("key leaked across jobs")
	}

	// Different keys never collide with each other.
	secondKey := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs", withKey(body, "event-2"))
	if secondKey.Code != http.StatusCreated {
		t.Fatalf("second key = %d %s", secondKey.Code, secondKey.Body.String())
	}
	if decodeBody(t, secondKey)["run_id"] == decodeBody(t, first)["run_id"] {
		t.Fatalf("different key reused run_id")
	}

	listed := f.request(http.MethodGet,
		"/api/v1/jobs/"+jobA+"/runs?from=2026-10-02T00:00:00Z&to=2026-10-03T00:00:00Z", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d %s", listed.Code, listed.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(listed.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("run count = %d, want 2: %s", len(rows), listed.Body.String())
	}
}

func TestCreateRunIdempotentNormalizesSemantics(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-norm")

	first := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		withKey(runInput("2026-10-02T09:30:00+08:00", "2026-10-02T09:30:05+08:00",
			"2026-10-02T09:31:00+08:00", "succeeded", nil), "norm-1"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	// Same instants expressed in Zulu replay identically.
	replay := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
			"2026-10-02T01:31:00Z", "succeeded", nil), "norm-1"))
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body.String())
	}
	if first.Body.String() != replay.Body.String() {
		t.Fatalf("normalized replay differs:\n%s\n%s", first.Body.String(), replay.Body.String())
	}
}

func TestCreateRunIdempotentConflict(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-conflict")

	first := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
			"2026-10-02T01:31:00Z", "succeeded", nil), "k"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}

	conflicts := []map[string]any{
		withKey(runInput("2026-10-02T01:31:00Z", "2026-10-02T01:31:05Z",
			"2026-10-02T01:32:00Z", "succeeded", nil), "k"),
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:06Z",
			"2026-10-02T01:31:00Z", "succeeded", nil), "k"),
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
			"2026-10-02T01:31:01Z", "succeeded", nil), "k"),
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
			"2026-10-02T01:31:00Z", "failed", "boom"), "k"),
	}
	for _, body := range conflicts {
		expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body),
			http.StatusConflict, "idempotency_conflict")
	}

	// The original response is still served and no extra run exists.
	replay := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
			"2026-10-02T01:31:00Z", "succeeded", nil), "k"))
	if replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay after conflicts = %d %s", replay.Code, replay.Body.String())
	}
	listed := f.request(http.MethodGet,
		"/api/v1/jobs/"+jobID+"/runs?from=2026-10-02T00:00:00Z&to=2026-10-03T00:00:00Z", nil)
	var rows []map[string]any
	json.Unmarshal(listed.Body.Bytes(), &rows)
	if len(rows) != 1 {
		t.Fatalf("conflict created runs: %s", listed.Body.String())
	}
}

func TestIdempotencyKeyValidation(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-invalid")
	runID := createFailedRunWithKey(t, f, jobID, "seed")

	runURL := "/api/v1/jobs/" + jobID + "/runs"
	retryURL := runURL + "/" + runID + "/retries"
	runBody := runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:00Z",
		"2026-10-02T01:30:00Z", "succeeded", nil)
	retryBody := retryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z", "succeeded", nil)

	blank := withKey(runBody, "   ")
	expectError(t, f.request(http.MethodPost, runURL, blank),
		http.StatusUnprocessableEntity, "idempotency_key_invalid")
	expectError(t, f.request(http.MethodPost, retryURL, withKey(retryBody, "  ")),
		http.StatusUnprocessableEntity, "idempotency_key_invalid")
	expectError(t, f.request(http.MethodPost, runURL, withKey(runBody, 123)),
		http.StatusBadRequest, "invalid_json")
	longKey := strings.Repeat("界", 129)
	expectError(t, f.request(http.MethodPost, runURL, withKey(runBody, longKey)),
		http.StatusUnprocessableEntity, "idempotency_key_invalid")
	exactLimit := strings.Repeat("x", 128)
	if recorder := f.request(http.MethodPost, runURL, withKey(runBody, exactLimit)); recorder.Code != http.StatusCreated {
		t.Fatalf("128-char key = %d %s", recorder.Code, recorder.Body.String())
	}
	unicodeLimit := strings.Repeat("界", 128)
	if recorder := f.request(http.MethodPost, runURL, withKey(runBody, unicodeLimit)); recorder.Code != http.StatusCreated {
		t.Fatalf("128-rune key = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestRetryIdempotentReplay(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-retry")
	runID := createFailedRunWithKey(t, f, jobID, "seed")

	retryBody := withKey(retryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z",
		"succeeded", nil), "retry-1")
	url := "/api/v1/jobs/" + jobID + "/runs/" + runID + "/retries"

	first := f.request(http.MethodPost, url, retryBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("first retry = %d %s", first.Code, first.Body.String())
	}
	if got := len(decodeBody(t, first)["results"].([]any)); got != 2 {
		t.Fatalf("results = %s", first.Body.String())
	}
	replay := f.request(http.MethodPost, url, retryBody)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay retry = %d %s", replay.Code, replay.Body.String())
	}
	if first.Body.String() != replay.Body.String() {
		t.Fatalf("retry replay differs:\n%s\n%s", first.Body.String(), replay.Body.String())
	}

	// A matching replay still returns the stored response once the run is
	// terminal; further different retries are blocked by the state too.
	expectError(t, f.request(http.MethodPost, url,
		withKey(retryInput("2026-10-02T03:30:00Z", "2026-10-02T03:31:00Z",
			"failed", "nope"), "retry-2")),
		http.StatusConflict, "retry_not_allowed")

	// Same key with different semantics is an idempotency conflict even
	// though the run is no longer failed.
	expectError(t, f.request(http.MethodPost, url,
		withKey(retryInput("2026-10-02T02:31:00Z", "2026-10-02T02:32:00Z",
			"succeeded", nil), "retry-1")),
		http.StatusConflict, "idempotency_conflict")

	// A failed retry can itself be replayed, and its key is per-run scoped.
	runID2 := createFailedRunWithKey(t, f, jobID, "seed-2")
	url2 := "/api/v1/jobs/" + jobID + "/runs/" + runID2 + "/retries"
	failedRetry := withKey(retryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z",
		"failed", "still broken"), "retry-1")
	a := f.request(http.MethodPost, url2, failedRetry)
	if a.Code != http.StatusCreated {
		t.Fatalf("failed retry = %d %s", a.Code, a.Body.String())
	}
	b := f.request(http.MethodPost, url2, failedRetry)
	if b.Code != http.StatusCreated || a.Body.String() != b.Body.String() {
		t.Fatalf("failed retry replay = %d %s", b.Code, b.Body.String())
	}
	if got := len(decodeBody(t, b)["results"].([]any)); got != 2 {
		t.Fatalf("replay appended another attempt: %s", b.Body.String())
	}

	// Final success reuses another key independently.
	final := f.request(http.MethodPost, url2,
		withKey(retryInput("2026-10-02T03:30:00Z", "2026-10-02T03:31:00Z",
			"succeeded", nil), "retry-2"))
	if final.Code != http.StatusCreated || len(decodeBody(t, final)["results"].([]any)) != 3 {
		t.Fatalf("final retry = %s", final.Body.String())
	}

	// Replaying the earlier failed-retry key replays its original response,
	// not the now three-attempt chain, and byte-for-byte matches the first
	// response recorded for that key.
	snapshot := f.request(http.MethodPost, url2, failedRetry)
	if snapshot.Code != http.StatusCreated {
		t.Fatalf("snapshot replay = %d %s", snapshot.Code, snapshot.Body.String())
	}
	if snapshot.Body.String() != a.Body.String() {
		t.Fatalf("snapshot differs:\nfirst %s\nnow   %s", a.Body.String(), snapshot.Body.String())
	}
	if got := len(decodeBody(t, snapshot)["results"].([]any)); got != 2 {
		t.Fatalf("snapshot result count = %d, want 2", got)
	}
}

func TestInvalidKeyedRequestsDoNotReserveKeys(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-reserve")
	runID := createFailedRunWithKey(t, f, jobID, "seed")

	// Failing validations must leave the key free for a later success.
	badBodies := []map[string]any{
		withKey(runInput("not-a-time", "2026-10-02T01:30:00Z",
			"2026-10-02T01:31:00Z", "succeeded", nil), "k1"),
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:29:00Z",
			"2026-10-02T01:31:00Z", "succeeded", nil), "k1"),
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:00Z",
			"2026-10-02T01:31:00Z", "crashed", nil), "k1"),
	}
	for _, body := range badBodies {
		if recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body); recorder.Code < 400 {
			t.Fatalf("expected validation failure: %s", recorder.Body.String())
		}
	}
	good := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:00Z",
			"2026-10-02T01:31:00Z", "succeeded", nil), "k1"))
	if good.Code != http.StatusCreated {
		t.Fatalf("key consumed by failed validation: %d %s", good.Code, good.Body.String())
	}

	// Same for retries: invalid retry body leaves the key free.
	url := "/api/v1/jobs/" + jobID + "/runs/" + runID + "/retries"
	expectError(t, f.request(http.MethodPost, url,
		withKey(retryInput("2026-10-02T01:29:00Z", "2026-10-02T01:30:00Z",
			"succeeded", nil), "rk1")),
		http.StatusUnprocessableEntity, "run_time_invalid")
	expectError(t, f.request(http.MethodPost, url,
		withKey(retryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z",
			"weird", nil), "rk1")),
		http.StatusUnprocessableEntity, "run_outcome_invalid")
	goodRetry := f.request(http.MethodPost, url,
		withKey(retryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z",
			"succeeded", nil), "rk1"))
	if goodRetry.Code != http.StatusCreated {
		t.Fatalf("retry key consumed by failed validation: %d %s", goodRetry.Code, goodRetry.Body.String())
	}
}

func TestKeylessWritesKeepLegacyBehavior(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-legacy")

	body := runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
		"2026-10-02T01:31:00Z", "succeeded", nil)
	first := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body)
	second := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("keyless writes = %d %d", first.Code, second.Code)
	}
	if decodeBody(t, first)["run_id"] == decodeBody(t, second)["run_id"] {
		t.Fatalf("keyless writes were deduplicated")
	}

	// Explicit null behaves like omission.
	nullBody := withKey(body, nil)
	third := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", nullBody)
	if third.Code != http.StatusCreated || decodeBody(t, third)["run_id"] == decodeBody(t, second)["run_id"] {
		t.Fatalf("null key was deduplicated: %s", third.Body.String())
	}
}

func TestCreateRunIdempotentConcurrent(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-concurrent")

	body := withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:05Z",
		"2026-10-02T01:31:00Z", "succeeded", nil), "race-key")

	const clients = 16
	var wg sync.WaitGroup
	responses := make(chan string, clients)
	statuses := make(chan int, clients)
	wg.Add(clients)
	for i := 0; i < clients; i++ {
		go func() {
			defer wg.Done()
			recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body)
			statuses <- recorder.Code
			responses <- recorder.Body.String()
		}()
	}
	wg.Wait()
	close(responses)
	close(statuses)

	for status := range statuses {
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
	}
	var first string
	count := 0
	for body := range responses {
		if first == "" {
			first = body
		}
		if body != first {
			t.Fatalf("concurrent responses differ:\n%s\n%s", first, body)
		}
		count++
	}
	if count != clients {
		t.Fatalf("responses = %d, want %d", count, clients)
	}

	listed := f.request(http.MethodGet,
		"/api/v1/jobs/"+jobID+"/runs?from=2026-10-02T00:00:00Z&to=2026-10-03T00:00:00Z", nil)
	var rows []map[string]any
	json.Unmarshal(listed.Body.Bytes(), &rows)
	if len(rows) != 1 {
		t.Fatalf("concurrent replays created %d runs", len(rows))
	}
}

func TestSoftDeletedJobRejectsKeyedWrite(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-deleted")
	runID := createFailedRunWithKey(t, f, jobID, "seed")

	if recorder := f.request(http.MethodDelete, "/api/v1/jobs/"+jobID, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", recorder.Code)
	}
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		withKey(runInput("2026-10-02T01:30:00Z", "2026-10-02T01:30:00Z",
			"2026-10-02T01:31:00Z", "succeeded", nil), "k")),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		withKey(retryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z",
			"succeeded", nil), "k")),
		http.StatusNotFound, "job_not_found")

	// History remains readable.
	if recorder := f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/"+runID, nil); recorder.Code != http.StatusOK {
		t.Fatalf("history = %d %s", recorder.Code, recorder.Body.String())
	}
}
