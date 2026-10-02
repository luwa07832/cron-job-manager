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

type apiFixture struct {
	t      *testing.T
	router http.Handler
	dbPath string
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &apiFixture{t: t, router: NewRouter(st), dbPath: path}
}

func (f *apiFixture) request(method, target string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		switch value := body.(type) {
		case string:
			reader = bytes.NewReader([]byte(value))
		case []byte:
			reader = bytes.NewReader(value)
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				f.t.Fatalf("marshal: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body
}

func expectError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %s", recorder.Body.String())
	}
	if errorBody["code"] != code {
		t.Fatalf("code = %v, want %s; body %s", errorBody["code"], code, recorder.Body.String())
	}
	if message, _ := errorBody["message"].(string); message == "" {
		t.Fatalf("empty message: %s", recorder.Body.String())
	}
}

func validCreateBody() map[string]any {
	return map[string]any{
		"name":       "nightly report",
		"expression": "0 9 * * *",
		"timezone":   "Asia/Shanghai",
		"enabled":    true,
	}
}

func TestCreateJobLifecycle(t *testing.T) {
	f := newAPIFixture(t)

	recorder := f.request(http.MethodPost, "/api/v1/jobs", validCreateBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	created := decodeBody(t, recorder)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("missing id: %s", recorder.Body.String())
	}
	if created["name"] != "nightly report" || created["expression"] != "0 9 * * *" ||
		created["timezone"] != "Asia/Shanghai" || created["enabled"] != true {
		t.Fatalf("unexpected payload: %s", recorder.Body.String())
	}
	createdAt, _ := created["created_at"].(string)
	nextRun, _ := created["next_run"].(string)
	if createdAt == "" || nextRun == "" {
		t.Fatalf("missing timestamps: %s", recorder.Body.String())
	}
	createdTime, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t.Fatalf("created_at %q is not RFC3339", createdAt)
	}
	nextTime, err := time.Parse(time.RFC3339, nextRun)
	if err != nil {
		t.Fatalf("next_run %q is not RFC3339", nextRun)
	}
	if !nextTime.After(createdTime) {
		t.Fatalf("next run %s must be after creation %s", nextTime, createdTime)
	}

	got := f.request(http.MethodGet, "/api/v1/jobs/"+id, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("get status = %d, body %s", got.Code, got.Body.String())
	}
	fetched := decodeBody(t, got)
	if fetched["next_run"] != nextRun {
		t.Fatalf("next_run changed on read: %s vs %s", fetched["next_run"], nextRun)
	}
}

func TestCreateDisabledJobHasNullNextRun(t *testing.T) {
	f := newAPIFixture(t)
	body := validCreateBody()
	body["enabled"] = false
	recorder := f.request(http.MethodPost, "/api/v1/jobs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	created := decodeBody(t, recorder)
	if created["enabled"] != false {
		t.Fatalf("enabled = %v", created["enabled"])
	}
	if value, exists := created["next_run"]; !exists || value != nil {
		t.Fatalf("next_run = %v, want null", value)
	}
}

func TestCreateJobValidationErrors(t *testing.T) {
	f := newAPIFixture(t)

	expectError(t, f.request(http.MethodPost, "/api/v1/jobs", "{not json"), http.StatusBadRequest, "invalid_json")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs", ""), http.StatusBadRequest, "invalid_json")
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs", `{"a":1} extra`), http.StatusBadRequest, "invalid_json")

	emptyName := validCreateBody()
	emptyName["name"] = "   "
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs", emptyName), http.StatusUnprocessableEntity, "name_required")

	f.request(http.MethodPost, "/api/v1/jobs", validCreateBody())
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs", validCreateBody()), http.StatusUnprocessableEntity, "name_conflict")

	badExpr := validCreateBody()
	badExpr["name"] = "bad expr"
	badExpr["expression"] = "60 * * * *"
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs", badExpr), http.StatusUnprocessableEntity, "schedule_invalid")

	badTz := validCreateBody()
	badTz["name"] = "bad tz"
	badTz["timezone"] = "Mars/Olympus"
	expectError(t, f.request(http.MethodPost, "/api/v1/jobs", badTz), http.StatusUnprocessableEntity, "timezone_invalid")
}

func TestPatchRecomputesAndNullsNextRun(t *testing.T) {
	f := newAPIFixture(t)

	recorder := f.request(http.MethodPost, "/api/v1/jobs", validCreateBody())
	created := decodeBody(t, recorder)
	id := created["id"].(string)

	disabled := f.request(http.MethodPatch, "/api/v1/jobs/"+id, map[string]any{"enabled": false})
	if disabled.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body %s", disabled.Code, disabled.Body.String())
	}
	disabledBody := decodeBody(t, disabled)
	if disabledBody["enabled"] != false || disabledBody["next_run"] != nil {
		t.Fatalf("disable response = %s", disabled.Body.String())
	}

	renabled := f.request(http.MethodPatch, "/api/v1/jobs/"+id, map[string]any{"enabled": true})
	if renabled.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body %s", renabled.Code, renabled.Body.String())
	}
	enabledBody := decodeBody(t, renabled)
	nextRun, _ := enabledBody["next_run"].(string)
	if nextRun == "" {
		t.Fatalf("re-enable next_run = %v", enabledBody["next_run"])
	}
	nextTime, err := time.Parse(time.RFC3339, nextRun)
	if err != nil {
		t.Fatalf("next_run not RFC3339: %s", nextRun)
	}
	if !nextTime.After(time.Now().Add(-2 * time.Second)) {
		t.Fatalf("recomputed next run %s is not after now", nextTime)
	}

	renamed := f.request(http.MethodPatch, "/api/v1/jobs/"+id, map[string]any{"name": "renamed"})
	if renamed.Code != http.StatusOK || decodeBody(t, renamed)["name"] != "renamed" {
		t.Fatalf("rename response = %s", renamed.Body.String())
	}
	if decodeBody(t, renamed)["next_run"] != nextRun {
		t.Fatalf("name-only patch changed next_run")
	}

	bad := f.request(http.MethodPatch, "/api/v1/jobs/"+id, map[string]any{"expression": "x y z a b"})
	expectError(t, bad, http.StatusUnprocessableEntity, "schedule_invalid")
}

func TestPatchMissingJob(t *testing.T) {
	f := newAPIFixture(t)
	expectError(t, f.request(http.MethodPatch, "/api/v1/jobs/nope", map[string]any{"name": "x"}), http.StatusNotFound, "job_not_found")
}

func TestDeleteJob(t *testing.T) {
	f := newAPIFixture(t)
	created := decodeBody(t, f.request(http.MethodPost, "/api/v1/jobs", validCreateBody()))
	id := created["id"].(string)

	deleted := f.request(http.MethodDelete, "/api/v1/jobs/"+id, nil)
	if deleted.Code != http.StatusNoContent || deleted.Body.Len() != 0 {
		t.Fatalf("delete = %d %q", deleted.Code, deleted.Body.String())
	}
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/"+id, nil), http.StatusNotFound, "job_not_found")
	expectError(t, f.request(http.MethodDelete, "/api/v1/jobs/"+id, nil), http.StatusNotFound, "job_not_found")
}

func TestListPendingWindow(t *testing.T) {
	f := newAPIFixture(t)

	first := validCreateBody()
	first["name"] = "first"
	first["expression"] = "0 9 * * *"
	f.request(http.MethodPost, "/api/v1/jobs", first)

	second := validCreateBody()
	second["name"] = "second"
	second["expression"] = "0 10 * * *"
	f.request(http.MethodPost, "/api/v1/jobs", second)

	off := validCreateBody()
	off["name"] = "off"
	off["enabled"] = false
	f.request(http.MethodPost, "/api/v1/jobs", off)

	from := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	to := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	target := "/api/v1/jobs?state=pending&from=" + from + "&to=" + to
	recorder := f.request(http.MethodGet, target, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var jobs []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len = %d, want 2: %s", len(jobs), recorder.Body.String())
	}
	if jobs[0]["name"] != "first" || jobs[1]["name"] != "second" {
		t.Fatalf("order = %v, %v", jobs[0]["name"], jobs[1]["name"])
	}
}

func TestListPendingWindowValidationAndEmpty(t *testing.T) {
	f := newAPIFixture(t)
	now := time.Now().UTC().Format(time.RFC3339)
	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=pending&to="+later, nil), http.StatusBadRequest, "time_window_required")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=pending&from="+now, nil), http.StatusBadRequest, "time_window_required")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=pending&from=notatime&to="+later, nil), http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=pending&from="+later+"&to="+now, nil), http.StatusBadRequest, "time_window_invalid")
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs?state=done&from="+now+"&to="+later, nil), http.StatusBadRequest, "time_window_invalid")

	recorder := f.request(http.MethodGet, "/api/v1/jobs?state=pending&from="+now+"&to="+later, nil)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "[]" {
		t.Fatalf("empty result = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestJobsSurviveRestart(t *testing.T) {
	f := newAPIFixture(t)
	created := decodeBody(t, f.request(http.MethodPost, "/api/v1/jobs", validCreateBody()))
	id := created["id"].(string)
	nextRun := created["next_run"].(string)

	reopened, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	router := NewRouter(reopened)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+id, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status after restart = %d: %s", recorder.Code, recorder.Body.String())
	}
	if decodeBody(t, recorder)["next_run"] != nextRun {
		t.Fatalf("next run changed after restart: %s", recorder.Body.String())
	}
}

func TestHealthzAndRouteShapeRemainStable(t *testing.T) {
	f := newAPIFixture(t)
	healthz := f.request(http.MethodGet, "/healthz", nil)
	if healthz.Code != http.StatusOK || healthz.Body.String() != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthz = %d %s", healthz.Code, healthz.Body.String())
	}
	expectError(t, f.request(http.MethodGet, "/api/v1/jobs/missing/sub", nil), http.StatusNotFound, "route_not_found")
}
