package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func createEnabledJob(t *testing.T, f *apiFixture, name, expression, timezone string) (string, time.Time) {
	t.Helper()
	recorder := f.request(http.MethodPost, "/api/v1/jobs", map[string]any{
		"name":       name,
		"expression": expression,
		"timezone":   timezone,
		"enabled":    true,
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	created := decodeBody(t, recorder)
	id, _ := created["id"].(string)
	nextText, _ := created["next_run"].(string)
	nextRun, err := time.Parse(time.RFC3339, nextText)
	if err != nil {
		t.Fatalf("next_run %q is not RFC3339", nextText)
	}
	return id, nextRun
}

func dispatchPath(id string) string {
	return "/api/v1/jobs/" + id + "/dispatches"
}

func TestDispatchReturnsOccurrencesAndAdvancesCursor(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "hourly", "0 * * * *", "UTC")

	recorder := f.request(http.MethodPost, dispatchPath(id), map[string]any{
		"expected_next_run": nextRun.Format(time.RFC3339),
		"before":            nextRun.Add(3 * time.Hour).Format(time.RFC3339),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["job_id"] != id {
		t.Fatalf("job_id = %v, want %s", body["job_id"], id)
	}
	occurrences, ok := body["occurrences"].([]any)
	if !ok || len(occurrences) != 4 {
		t.Fatalf("occurrences = %v, want 4 entries", body["occurrences"])
	}
	for i := 0; i < 4; i++ {
		want := nextRun.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		if occurrences[i] != want {
			t.Fatalf("occurrences[%d] = %v, want %s", i, occurrences[i], want)
		}
	}
	wantNext := nextRun.Add(4 * time.Hour).Format(time.RFC3339)
	if body["next_run"] != wantNext {
		t.Fatalf("next_run = %v, want %s", body["next_run"], wantNext)
	}

	fetched := decodeBody(t, f.request(http.MethodGet, "/api/v1/jobs/"+id, nil))
	if fetched["next_run"] != wantNext {
		t.Fatalf("stored next_run = %v, want %s", fetched["next_run"], wantNext)
	}
}

func TestDispatchSingleOccurrenceWhenBeforeEqualsNextRun(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "daily", "30 9 * * *", "UTC")

	recorder := f.request(http.MethodPost, dispatchPath(id), map[string]any{
		"expected_next_run": nextRun.Format(time.RFC3339),
		"before":            nextRun.Format(time.RFC3339),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	occurrences, _ := body["occurrences"].([]any)
	if len(occurrences) != 1 || occurrences[0] != nextRun.Format(time.RFC3339) {
		t.Fatalf("occurrences = %v, want exactly [%s]", occurrences, nextRun.Format(time.RFC3339))
	}
	wantNext := nextRun.Add(24 * time.Hour).Format(time.RFC3339)
	if body["next_run"] != wantNext {
		t.Fatalf("next_run = %v, want %s", body["next_run"], wantNext)
	}
}

func TestDispatchComputesOccurrencesInJobTimezone(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "shanghai daily", "30 9 * * *", "Asia/Shanghai")

	if got := nextRun.UTC().Format("15:04"); got != "01:30" {
		t.Fatalf("next_run %s is %s UTC, want 01:30", nextRun, got)
	}
	recorder := f.request(http.MethodPost, dispatchPath(id), map[string]any{
		"expected_next_run": nextRun.Format(time.RFC3339),
		"before":            nextRun.Add(24 * time.Hour).Format(time.RFC3339),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	occurrences, _ := body["occurrences"].([]any)
	if len(occurrences) != 2 {
		t.Fatalf("occurrences = %v, want 2 entries", occurrences)
	}
	wantNext := nextRun.Add(48 * time.Hour).Format(time.RFC3339)
	if body["next_run"] != wantNext {
		t.Fatalf("next_run = %v, want %s", body["next_run"], wantNext)
	}
}

func TestDispatchAcceptsOffsets(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "offset hourly", "0 * * * *", "UTC")

	zone := time.FixedZone("UTC+8", 8*60*60)
	before := nextRun.Add(time.Hour)
	recorder := f.request(http.MethodPost, dispatchPath(id), map[string]any{
		"expected_next_run": nextRun.In(zone).Format(time.RFC3339),
		"before":            before.In(zone).Format(time.RFC3339),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	occurrences, _ := body["occurrences"].([]any)
	if len(occurrences) != 2 {
		t.Fatalf("occurrences = %v, want 2 entries", occurrences)
	}
	if occurrences[0] != nextRun.Format(time.RFC3339) {
		t.Fatalf("occurrences[0] = %v, want UTC %s", occurrences[0], nextRun.Format(time.RFC3339))
	}
}

func TestDispatchNotFound(t *testing.T) {
	f := newAPIFixture(t)
	validBody := map[string]any{
		"expected_next_run": "2026-10-02T09:00:00Z",
		"before":            "2026-10-02T10:00:00Z",
	}
	expectError(t, f.request(http.MethodPost, dispatchPath("missing"), validBody), http.StatusNotFound, "job_not_found")

	disabledBody := validCreateBody()
	disabledBody["name"] = "disabled"
	disabledBody["enabled"] = false
	created := decodeBody(t, f.request(http.MethodPost, "/api/v1/jobs", disabledBody))
	disabledID, _ := created["id"].(string)
	expectError(t, f.request(http.MethodPost, dispatchPath(disabledID), validBody), http.StatusNotFound, "job_not_found")

	id, _ := createEnabledJob(t, f, "deleted", "0 * * * *", "UTC")
	f.request(http.MethodDelete, "/api/v1/jobs/"+id, nil)
	expectError(t, f.request(http.MethodPost, dispatchPath(id), validBody), http.StatusNotFound, "job_not_found")
}

func TestDispatchInvalidJSON(t *testing.T) {
	f := newAPIFixture(t)
	id, _ := createEnabledJob(t, f, "json", "0 * * * *", "UTC")

	expectError(t, f.request(http.MethodPost, dispatchPath(id), "{not json"), http.StatusBadRequest, "invalid_json")
	expectError(t, f.request(http.MethodPost, dispatchPath(id), ""), http.StatusBadRequest, "invalid_json")
	expectError(t, f.request(http.MethodPost, dispatchPath(id), `{"a":1} extra`), http.StatusBadRequest, "invalid_json")
	expectError(t, f.request(http.MethodPost, dispatchPath(id), `[1,2]`), http.StatusBadRequest, "invalid_json")
}

func TestDispatchInvalidTimes(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "times", "0 * * * *", "UTC")
	valid := nextRun.Format(time.RFC3339)

	cases := []map[string]any{
		{"expected_next_run": "not-a-time", "before": valid},
		{"expected_next_run": valid, "before": "2026-13-01T00:00:00Z"},
		{"expected_next_run": valid},
		{"before": valid},
		{},
	}
	for _, body := range cases {
		expectError(t, f.request(http.MethodPost, dispatchPath(id), body), http.StatusUnprocessableEntity, "dispatch_time_invalid")
	}
}

func TestDispatchNextRunConflictLeavesState(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "conflict", "0 * * * *", "UTC")

	other := nextRun.Add(time.Hour)
	expectError(t, f.request(http.MethodPost, dispatchPath(id), map[string]any{
		"expected_next_run": other.Format(time.RFC3339),
		"before":            other.Format(time.RFC3339),
	}), http.StatusConflict, "next_run_conflict")

	fetched := decodeBody(t, f.request(http.MethodGet, "/api/v1/jobs/"+id, nil))
	if fetched["next_run"] != nextRun.Format(time.RFC3339) {
		t.Fatalf("next_run changed after conflict: %v", fetched["next_run"])
	}
}

func TestDispatchNotDueLeavesState(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "not due", "0 * * * *", "UTC")

	expectError(t, f.request(http.MethodPost, dispatchPath(id), map[string]any{
		"expected_next_run": nextRun.Format(time.RFC3339),
		"before":            nextRun.Add(-time.Minute).Format(time.RFC3339),
	}), http.StatusConflict, "dispatch_not_due")

	fetched := decodeBody(t, f.request(http.MethodGet, "/api/v1/jobs/"+id, nil))
	if fetched["next_run"] != nextRun.Format(time.RFC3339) {
		t.Fatalf("next_run changed after not-due: %v", fetched["next_run"])
	}
}

func TestDispatchReplayedCursorConflicts(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "replay", "0 * * * *", "UTC")

	body := map[string]any{
		"expected_next_run": nextRun.Format(time.RFC3339),
		"before":            nextRun.Add(2 * time.Hour).Format(time.RFC3339),
	}
	first := f.request(http.MethodPost, dispatchPath(id), body)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	expectError(t, f.request(http.MethodPost, dispatchPath(id), body), http.StatusConflict, "next_run_conflict")
}

func TestDispatchConcurrentClaimants(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "race", "0 * * * *", "UTC")

	body := map[string]any{
		"expected_next_run": nextRun.Format(time.RFC3339),
		"before":            nextRun.Add(2 * time.Hour).Format(time.RFC3339),
	}
	const claimants = 6
	codes := make(chan int, claimants)
	for i := 0; i < claimants; i++ {
		go func() {
			recorder := f.request(http.MethodPost, dispatchPath(id), body)
			codes <- recorder.Code
		}()
	}
	var succeeded, conflicts int
	for i := 0; i < claimants; i++ {
		switch <-codes {
		case http.StatusOK:
			succeeded++
		case http.StatusConflict:
			conflicts++
		}
	}
	if succeeded != 1 || conflicts != claimants-1 {
		t.Fatalf("succeeded = %d, conflicts = %d, want 1 and %d", succeeded, conflicts, claimants-1)
	}
}

func TestDispatchRecordsNoRuns(t *testing.T) {
	f := newAPIFixture(t)
	id, nextRun := createEnabledJob(t, f, "no runs", "0 * * * *", "UTC")

	runBody := map[string]any{
		"scheduled_for": nextRun.Format(time.RFC3339),
		"started_at":    nextRun.Format(time.RFC3339),
		"finished_at":   nextRun.Add(time.Minute).Format(time.RFC3339),
		"outcome":       "succeeded",
	}
	created := f.request(http.MethodPost, "/api/v1/jobs/"+id+"/runs", runBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("run status = %d, body %s", created.Code, created.Body.String())
	}
	runID, _ := decodeBody(t, created)["run_id"].(string)

	dispatched := f.request(http.MethodPost, dispatchPath(id), map[string]any{
		"expected_next_run": nextRun.Format(time.RFC3339),
		"before":            nextRun.Add(2 * time.Hour).Format(time.RFC3339),
	})
	if dispatched.Code != http.StatusOK {
		t.Fatalf("dispatch status = %d, body %s", dispatched.Code, dispatched.Body.String())
	}

	from := nextRun.Add(-time.Hour).Format(time.RFC3339)
	to := nextRun.Add(24 * time.Hour).Format(time.RFC3339)
	listed := f.request(http.MethodGet, "/api/v1/jobs/"+id+"/runs?from="+from+"&to="+to, nil)
	var runs []map[string]any
	if err := json.Unmarshal(listed.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode %q: %v", listed.Body.String(), err)
	}
	if len(runs) != 1 || runs[0]["run_id"] != runID {
		t.Fatalf("runs = %s, want only the pre-registered run", listed.Body.String())
	}
	results, _ := runs[0]["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v, want the single recorded attempt", results)
	}
}
