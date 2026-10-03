package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

func keyedRunInput(scheduled, started, finished, outcome string, errText any, key any) map[string]any {
	body := runInput(scheduled, started, finished, outcome, errText)
	if key != nil {
		body["idempotency_key"] = key
	}
	return body
}

func keyedRetryInput(started, finished, outcome string, errText any, key any) map[string]any {
	body := retryInput(started, finished, outcome, errText)
	if key != nil {
		body["idempotency_key"] = key
	}
	return body
}

func createFailedRunWithKey(t *testing.T, f *apiFixture, jobID, scheduled, errText, key string) string {
	t.Helper()
	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		keyedRunInput(scheduled, scheduled, scheduled, "failed", errText, key))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create run = %d %s", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)["run_id"].(string)
}

func TestCreateRunIdempotencyReplay(t *testing.T) {
	f := newAPIFixture(t)
	jobA := createTestJob(t, f, "idem-a")
	jobB := createTestJob(t, f, "idem-b")

	scheduled := "2026-10-02T01:30:00Z"
	body := keyedRunInput(scheduled, "2026-10-02T01:30:05Z", "2026-10-02T01:31:00Z",
		"failed", "exit status 1", "event-1")

	first := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	second := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs", body)
	if second.Code != http.StatusCreated {
		t.Fatalf("replay = %d %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay body differs:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	if decodeBody(t, second)["run_id"] != decodeBody(t, first)["run_id"] {
		t.Fatalf("run id changed on replay")
	}

	// Offset-bearing but identical instant parses to the same semantics.
	offsetBody := keyedRunInput("2026-10-02T09:30:00+08:00", "2026-10-02T09:30:05+08:00",
		"2026-10-02T09:31:00+08:00", "failed", "exit status 1", "event-1")
	offset := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs", offsetBody)
	if offset.Code != http.StatusCreated || offset.Body.String() != first.Body.String() {
		t.Fatalf("offset replay = %d %s", offset.Code, offset.Body.String())
	}

	// Same key text on another job is independent.
	other := f.request(http.MethodPost, "/api/v1/jobs/"+jobB+"/runs", body)
	if other.Code != http.StatusCreated ||
		decodeBody(t, other)["run_id"] == decodeBody(t, first)["run_id"] {
		t.Fatalf("cross-job replay = %d %s", other.Code, other.Body.String())
	}

	// Exactly one run exists for job A.
	listing := f.request(http.MethodGet,
		"/api/v1/jobs/"+jobA+"/runs?from=2026-01-01T00:00:00Z&to=2027-01-01T00:00:00Z", nil)
	var runs []map[string]any
	if err := json.Unmarshal(listing.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1: %s", len(runs), listing.Body.String())
	}
}

func TestCreateRunIdempotencyConflict(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-conflict")
	scheduled := "2026-10-02T01:30:00Z"

	first := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		keyedRunInput(scheduled, scheduled, "2026-10-02T01:31:00Z", "failed", "x", "evt"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	firstID := decodeBody(t, first)["run_id"]

	variants := []map[string]any{
		keyedRunInput("2026-10-02T01:31:00Z", "2026-10-02T01:31:00Z", "2026-10-02T01:32:00Z", "failed", "x", "evt"),
		keyedRunInput(scheduled, "2026-10-02T01:30:06Z", "2026-10-02T01:31:00Z", "failed", "x", "evt"),
		keyedRunInput(scheduled, scheduled, "2026-10-02T01:32:00Z", "failed", "x", "evt"),
		keyedRunInput(scheduled, scheduled, "2026-10-02T01:31:00Z", "succeeded", nil, "evt"),
		keyedRunInput(scheduled, scheduled, "2026-10-02T01:31:00Z", "failed", "different", "evt"),
	}
	for _, variant := range variants {
		recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", variant)
		expectError(t, recorder, http.StatusConflict, "idempotency_conflict")
	}

	listing := f.request(http.MethodGet,
		"/api/v1/jobs/"+jobID+"/runs?from=2026-01-01T00:00:00Z&to=2027-01-01T00:00:00Z", nil)
	var runs []map[string]any
	if err := json.Unmarshal(listing.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 || runs[0]["run_id"] != firstID {
		t.Fatalf("conflict added runs: %s", listing.Body.String())
	}
}

func TestIdempotencyKeyValidation(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-validation")
	scheduled := "2026-10-02T01:30:00Z"

	for _, key := range []any{"", "   ", "\t\n", strings129()} {
		body := keyedRunInput(scheduled, scheduled, scheduled, "succeeded", nil, key)
		expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body),
			http.StatusUnprocessableEntity, "idempotency_key_invalid")
	}

	// A valid key on the boundary length is accepted.
	boundary := stringsRepeat("k", 128)
	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		keyedRunInput(scheduled, scheduled, scheduled, "succeeded", nil, boundary))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("boundary key = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestIdempotencyKeyValidationFailsBeforeReservation(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-reserve")
	scheduled := "2026-10-02T01:30:00Z"

	// Malformed semantics keep their original error codes even with a key...
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		keyedRunInput("bad-time", scheduled, scheduled, "succeeded", nil, "k")),
		http.StatusUnprocessableEntity, "run_time_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		keyedRunInput(scheduled, scheduled, scheduled, "weird", nil, "k")),
		http.StatusUnprocessableEntity, "run_outcome_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/unknown/runs",
		keyedRunInput(scheduled, scheduled, scheduled, "succeeded", nil, "k")),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", "{bad"),
		http.StatusBadRequest, "invalid_json")

	// ...so the key is still free for a successful first registration.
	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		keyedRunInput(scheduled, scheduled, scheduled, "succeeded", nil, "k"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("first use of key = %d %s", recorder.Code, recorder.Body.String())
	}

	// Retry endpoint: run lookup and semantics fail before reservation.
	runID := createFailedRunWithKey(t, f, jobID, "2026-10-03T01:30:00Z", "boom", "run-k")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/ghost/retries",
		keyedRetryInput(scheduled, scheduled, "succeeded", nil, "rk")),
		http.StatusNotFound, "run_not_found")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		keyedRetryInput("2026-10-03T01:29:00Z", scheduled, "succeeded", nil, "rk")),
		http.StatusUnprocessableEntity, "run_time_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		keyedRetryInput("2026-10-03T01:30:00Z", "2026-10-03T01:31:00Z", "weird", nil, "rk")),
		http.StatusUnprocessableEntity, "run_outcome_invalid")
	for _, key := range []any{"", "  "} {
		expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
			keyedRetryInput("2026-10-03T01:30:00Z", "2026-10-03T01:31:00Z", "succeeded", nil, key)),
			http.StatusUnprocessableEntity, "idempotency_key_invalid")
	}
	recorder = f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		keyedRetryInput("2026-10-03T01:30:00Z", "2026-10-03T01:31:00Z", "succeeded", nil, "rk"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("first use of retry key = %d %s", recorder.Code, recorder.Body.String())
	}
}

func strings129() string { return stringsRepeat("é", 129) }
func stringsRepeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestRetryIdempotencyReplayAndConflict(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-retry")
	scheduled := "2026-10-02T01:30:00Z"
	runID := createFailedRunWithKey(t, f, jobID, scheduled, "boom", "run-1")

	retryBody := keyedRetryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z",
		"failed", "still broken", "retry-1")
	first := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries", retryBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("retry = %d %s", first.Code, first.Body.String())
	}
	firstBody := decodeBody(t, first)
	if len(firstBody["results"].([]any)) != 2 {
		t.Fatalf("results = %s", first.Body.String())
	}

	// A non-idempotent follow-up moves the chain to success.
	succeeded := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		retryInput("2026-10-02T03:30:00Z", "2026-10-02T03:31:00Z", "succeeded", nil))
	if succeeded.Code != http.StatusCreated {
		t.Fatalf("succeed = %d %s", succeeded.Code, succeeded.Body.String())
	}

	// Replaying the keyed retry no longer appends, stays 201, and returns the
	// stored two-result list even though retries are now disallowed.
	replay := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries", retryBody)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body differs:\n%s\n%s", replay.Body.String(), first.Body.String())
	}
	fetched := f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/"+runID, nil)
	if fetched.Code != http.StatusOK {
		t.Fatalf("get run = %d %s", fetched.Code, fetched.Body.String())
	}
	if len(decodeBody(t, fetched)["results"].([]any)) != 3 {
		t.Fatalf("replay appended an attempt: %s", fetched.Body.String())
	}

	// Different semantics with the same key conflict.
	conflict := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		keyedRetryInput("2026-10-02T02:30:00Z", "2026-10-02T02:32:00Z",
			"failed", "still broken", "retry-1"))
	expectError(t, conflict, http.StatusConflict, "idempotency_conflict")
}

func TestRetryIdempotencyKeyScopedToRun(t *testing.T) {
	f := newAPIFixture(t)
	jobA := createTestJob(t, f, "retry-scope-a")
	jobB := createTestJob(t, f, "retry-scope-b")
	scheduled := "2026-10-02T01:30:00Z"
	runA := createFailedRunWithKey(t, f, jobA, scheduled, "a", "ra")
	runB := createFailedRunWithKey(t, f, jobB, scheduled, "b", "rb")

	body := keyedRetryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z", "succeeded", nil, "shared")
	if recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs/"+runA+"/retries", body); recorder.Code != http.StatusCreated {
		t.Fatalf("job A retry = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobB+"/runs/"+runB+"/retries", body); recorder.Code != http.StatusCreated {
		t.Fatalf("job B retry = %d %s", recorder.Code, recorder.Body.String())
	}

	// Replaying returns each run's own response.
	replayA := f.request(http.MethodPost, "/api/v1/jobs/"+jobA+"/runs/"+runA+"/retries", body)
	if decodeBody(t, replayA)["run_id"] != runA {
		t.Fatalf("replay A run = %s", replayA.Body.String())
	}
}

func TestCreateRunIdempotencyConcurrent(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-concurrent")
	scheduled := "2026-10-02T01:30:00Z"
	body := keyedRunInput(scheduled, scheduled, "2026-10-02T01:31:00Z", "failed", "x", "race")

	const count = 12
	var wg sync.WaitGroup
	responses := make(chan string, count)
	statuses := make(chan int, count)
	wg.Add(count)
	for i := 0; i < count; i++ {
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

	var bodies []string
	for status := range statuses {
		if status != http.StatusCreated {
			t.Fatalf("concurrent status = %d", status)
		}
	}
	for body := range responses {
		bodies = append(bodies, body)
	}
	for _, body := range bodies {
		if body != bodies[0] {
			t.Fatalf("concurrent bodies differ:\n%s\n%s", body, bodies[0])
		}
	}

	listing := f.request(http.MethodGet,
		"/api/v1/jobs/"+jobID+"/runs?from=2026-01-01T00:00:00Z&to=2027-01-01T00:00:00Z", nil)
	var runs []map[string]any
	if err := json.Unmarshal(listing.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("concurrent creates = %d runs", len(runs))
	}
}

func TestRetryIdempotencyConcurrent(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-retry-race")
	scheduled := "2026-10-02T01:30:00Z"
	runID := createFailedRunWithKey(t, f, jobID, scheduled, "x", "race-run")
	body := keyedRetryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z", "succeeded", nil, "race-retry")

	const count = 12
	var wg sync.WaitGroup
	bodies := make(chan string, count)
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			recorder := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries", body)
			if recorder.Code != http.StatusCreated {
				t.Errorf("status = %d %s", recorder.Code, recorder.Body.String())
				return
			}
			bodies <- recorder.Body.String()
		}()
	}
	wg.Wait()
	close(bodies)

	first := ""
	for body := range bodies {
		if first == "" {
			first = body
		} else if body != first {
			t.Fatalf("bodies differ:\n%s\n%s", body, first)
		}
	}
	fetched := f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/"+runID, nil)
	if len(decodeBody(t, fetched)["results"].([]any)) != 2 {
		t.Fatalf("concurrent retries appended extra attempts")
	}
}

func TestIdempotencyOmitsKeyKeepsLegacyBehavior(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-legacy")
	scheduled := "2026-10-02T01:30:00Z"
	body := keyedRunInput(scheduled, scheduled, "2026-10-02T01:31:00Z", "succeeded", nil, nil)
	first := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body)
	second := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", body)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated ||
		decodeBody(t, first)["run_id"] == decodeBody(t, second)["run_id"] {
		t.Fatalf("legacy create behavior changed: %d %d", first.Code, second.Code)
	}

	// Explicit null is the same as omitted.
	nullBody := `{"scheduled_for":"2026-10-02T01:32:00Z","started_at":"2026-10-02T01:32:00Z","finished_at":"2026-10-02T01:33:00Z","outcome":"succeeded","error":null,"idempotency_key":null}`
	one := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", nullBody)
	two := f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs", nullBody)
	if one.Code != http.StatusCreated || two.Code != http.StatusCreated ||
		decodeBody(t, one)["run_id"] == decodeBody(t, two)["run_id"] {
		t.Fatalf("null key behavior changed: %d %d", one.Code, two.Code)
	}
}

func TestIdempotencySoftDeletedJobStillRejectsWrites(t *testing.T) {
	f := newAPIFixture(t)
	jobID := createTestJob(t, f, "idem-deleted")
	scheduled := "2026-10-02T01:30:00Z"
	runID := createFailedRunWithKey(t, f, jobID, scheduled, "x", "before-delete")

	deleted := f.request(http.MethodDelete, "/api/v1/jobs/"+jobID, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", deleted.Code)
	}
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs",
		keyedRunInput("2026-10-02T05:30:00Z", "2026-10-02T05:30:00Z", "2026-10-02T05:31:00Z",
			"succeeded", nil, "after-delete")),
		http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+jobID+"/runs/"+runID+"/retries",
		keyedRetryInput("2026-10-02T02:30:00Z", "2026-10-02T02:31:00Z", "succeeded", nil, "after-delete")),
		http.StatusNotFound, "job_not_found")

	history := f.request(http.MethodGet, "/api/v1/jobs/"+jobID+"/runs/"+runID, nil)
	if history.Code != http.StatusOK {
		t.Fatalf("history = %d %s", history.Code, history.Body.String())
	}
}
