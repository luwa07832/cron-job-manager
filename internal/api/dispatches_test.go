package api

import (
	"net/http"
	"testing"
	"time"
)

func createDispatchJob(t *testing.T, f *apiFixture, name, expression, timezone string, enabled bool) map[string]any {
	t.Helper()
	body := map[string]any{
		"name":       name,
		"expression": expression,
		"timezone":   timezone,
		"enabled":    enabled,
	}
	recorder := f.request(http.MethodPost, "/api/v1/jobs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)
}

func dispatchBody(expected, before string) map[string]any {
	return map[string]any{"expected_next_run": expected, "before": before}
}

func TestDispatchCollectsOccurrencesAndAdvancesCursor(t *testing.T) {
	f := newAPIFixture(t)
	job := createDispatchJob(t, f, "hourly", "0 * * * *", "UTC", true)
	id, _ := job["id"].(string)
	currentText, _ := job["next_run"].(string)
	current, err := time.Parse(time.RFC3339, currentText)
	if err != nil {
		t.Fatalf("next_run %q: %v", currentText, err)
	}

	before := current.Add(3 * time.Hour)
	recorder := f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		dispatchBody(currentText, before.Format(time.RFC3339)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["job_id"] != id {
		t.Fatalf("job_id = %v", body["job_id"])
	}
	occurrences, ok := body["occurrences"].([]any)
	if !ok {
		t.Fatalf("occurrences is not a list: %s", recorder.Body.String())
	}
	if len(occurrences) != 4 {
		t.Fatalf("occurrences = %v, want 4 entries", occurrences)
	}
	wantTimes := []time.Time{
		current, current.Add(time.Hour), current.Add(2 * time.Hour), current.Add(3 * time.Hour),
	}
	for index, want := range wantTimes {
		gotText, _ := occurrences[index].(string)
		got, err := time.Parse(time.RFC3339, gotText)
		if err != nil {
			t.Fatalf("occurrence %d %q is not RFC3339", index, gotText)
		}
		if !got.Equal(want) {
			t.Fatalf("occurrence %d = %s, want %s", index, got, want)
		}
	}
	nextText, _ := body["next_run"].(string)
	nextRun, err := time.Parse(time.RFC3339, nextText)
	if err != nil || !nextRun.Equal(before.Add(time.Hour)) {
		t.Fatalf("next_run = %q, want %s", nextText, before.Add(time.Hour))
	}

	// The stored cursor matches the response.
	fetched := decodeBody(t, f.request(http.MethodGet, "/api/v1/jobs/"+id, nil))
	if fetched["next_run"] != nextText {
		t.Fatalf("stored next_run = %v, want %s", fetched["next_run"], nextText)
	}

	// No runs or attempts were recorded by the dispatch.
	runs := f.request(http.MethodGet,
		"/api/v1/jobs/"+id+"/runs?from=2000-01-01T00:00:00Z&to=2100-01-01T00:00:00Z", nil)
	if runs.Code != http.StatusOK || runs.Body.String() != "[]" {
		t.Fatalf("runs = %d %s, want empty", runs.Code, runs.Body.String())
	}
}

func TestDispatchBeforeAtNextRunReturnsSingleOccurrence(t *testing.T) {
	f := newAPIFixture(t)
	job := createDispatchJob(t, f, "every five", "*/5 * * * *", "UTC", true)
	currentText, _ := job["next_run"].(string)
	current, _ := time.Parse(time.RFC3339, currentText)

	recorder := f.request(http.MethodPost,
		"/api/v1/jobs/"+job["id"].(string)+"/dispatches",
		dispatchBody(currentText, currentText))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	occurrences, _ := body["occurrences"].([]any)
	if len(occurrences) != 1 || occurrences[0] != currentText {
		t.Fatalf("occurrences = %v, want only %s", occurrences, currentText)
	}
	nextText, _ := body["next_run"].(string)
	nextRun, _ := time.Parse(time.RFC3339, nextText)
	if !nextRun.Equal(current.Add(5 * time.Minute)) {
		t.Fatalf("next_run = %s, want %s", nextRun, current.Add(5*time.Minute))
	}
}

func TestDispatchAcceptsOffsetsAndNormalizesToUTC(t *testing.T) {
	f := newAPIFixture(t)
	job := createDispatchJob(t, f, "offset", "0 * * * *", "UTC", true)
	currentText, _ := job["next_run"].(string)
	current, _ := time.Parse(time.RFC3339, currentText)

	east := time.FixedZone("UTC+8", 8*60*60)
	west := time.FixedZone("UTC-1", -60*60)
	expectedText := current.In(east).Format(time.RFC3339)
	beforeText := current.Add(time.Hour).In(west).Format(time.RFC3339)
	if expectedText[len(expectedText)-6:] != "+08:00" || beforeText[len(beforeText)-6:] != "-01:00" {
		t.Fatalf("test offsets wrong: %s %s", expectedText, beforeText)
	}
	recorder := f.request(http.MethodPost,
		"/api/v1/jobs/"+job["id"].(string)+"/dispatches",
		dispatchBody(expectedText, beforeText))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	occurrences, _ := body["occurrences"].([]any)
	if len(occurrences) != 2 {
		t.Fatalf("occurrences = %v, want 2", occurrences)
	}
	for _, occurrence := range occurrences {
		text, _ := occurrence.(string)
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil || parsed.Location() != time.UTC {
			t.Fatalf("occurrence %q is not UTC RFC3339", text)
		}
	}
}

func TestDispatchValidationAndStateErrors(t *testing.T) {
	f := newAPIFixture(t)
	job := createDispatchJob(t, f, "hourly", "0 * * * *", "UTC", true)
	id := job["id"].(string)
	currentText, _ := job["next_run"].(string)
	current, _ := time.Parse(time.RFC3339, currentText)
	beforeText := current.Add(time.Hour).Format(time.RFC3339)

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches", "{bad"),
		http.StatusBadRequest, "invalid_json")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches", `[1,2]`),
		http.StatusBadRequest, "invalid_json")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		map[string]any{"before": beforeText}), http.StatusUnprocessableEntity, "dispatch_time_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		map[string]any{"expected_next_run": currentText, "before": "not-a-time"}),
		http.StatusUnprocessableEntity, "dispatch_time_invalid")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		map[string]any{"expected_next_run": "yesterday", "before": beforeText}),
		http.StatusUnprocessableEntity, "dispatch_time_invalid")

	// Stale cursor: expected differs from the stored next_run.
	stale := current.Add(-time.Hour).Format(time.RFC3339)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		dispatchBody(stale, beforeText)), http.StatusConflict, "next_run_conflict")

	// Cursor later than before: nothing due.
	early := current.Add(-2 * time.Hour).Format(time.RFC3339)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		dispatchBody(currentText, early)), http.StatusConflict, "dispatch_not_due")

	// Conflicts and not-due responses leave the cursor untouched.
	fetched := decodeBody(t, f.request(http.MethodGet, "/api/v1/jobs/"+id, nil))
	if fetched["next_run"] != currentText {
		t.Fatalf("next_run changed after errors: %v", fetched["next_run"])
	}

	// Unknown, disabled and soft-deleted jobs all read as job_not_found.
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/does-not-exist/dispatches",
		dispatchBody(currentText, beforeText)), http.StatusNotFound, "job_not_found")

	disabled := createDispatchJob(t, f, "paused", "0 * * * *", "UTC", false)
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+disabled["id"].(string)+"/dispatches",
		dispatchBody(currentText, beforeText)), http.StatusNotFound, "job_not_found")

	if recorder := f.request(http.MethodDelete, "/api/v1/jobs/"+id, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", recorder.Code)
	}
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		dispatchBody(currentText, beforeText)), http.StatusNotFound, "job_not_found")
}

func TestDispatchConcurrentCursorRace(t *testing.T) {
	f := newAPIFixture(t)
	job := createDispatchJob(t, f, "hourly", "0 * * * *", "UTC", true)
	id := job["id"].(string)
	currentText, _ := job["next_run"].(string)
	current, _ := time.Parse(time.RFC3339, currentText)
	before := current.Add(time.Hour).Format(time.RFC3339)

	first := f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		dispatchBody(currentText, before))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		dispatchBody(currentText, before)), http.StatusConflict, "next_run_conflict")

	// The winner's new cursor lets the next caller dispatch again.
	fetched := decodeBody(t, f.request(http.MethodGet, "/api/v1/jobs/"+id, nil))
	newCursor, _ := fetched["next_run"].(string)
	second := f.request(http.MethodPost, "/api/v1/jobs/"+id+"/dispatches",
		dispatchBody(newCursor, current.Add(2*time.Hour).Format(time.RFC3339)))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", second.Code, second.Body.String())
	}
}
