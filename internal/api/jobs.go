package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/cron-job-manager/internal/schedule"
	"github.com/luwa07832/cron-job-manager/internal/store"
)

type createJobRequest struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
	Timezone   string `json:"timezone"`
	Enabled    *bool  `json:"enabled"`
}

type patchJobRequest struct {
	Name       *string `json:"name"`
	Expression *string `json:"expression"`
	Timezone   *string `json:"timezone"`
	Enabled    *bool   `json:"enabled"`
}

type jobResponse struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Expression string  `json:"expression"`
	Timezone   string  `json:"timezone"`
	Enabled    bool    `json:"enabled"`
	CreatedAt  string  `json:"created_at"`
	NextRun    *string `json:"next_run"`
}

func registerJobs(router *gin.Engine, st *store.Store) {
	router.POST("/api/v1/jobs", createJob(st))
	router.GET("/api/v1/jobs", listPendingJobs(st))
	router.GET("/api/v1/jobs/:id", getJob(st))
	router.PATCH("/api/v1/jobs/:id", patchJob(st))
	router.DELETE("/api/v1/jobs/:id", deleteJob(st))
}

func createJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request createJobRequest
		if !decodeJobBody(c, &request) {
			return
		}
		if err := validateName(request.Name); err != nil {
			writeJobError(c, err)
			return
		}
		loc, tzErr := validateTimezone(request.Timezone)
		if tzErr != nil {
			writeJobError(c, tzErr)
			return
		}
		spec, specErr := validateSchedule(request.Expression)
		if specErr != nil {
			writeJobError(c, specErr)
			return
		}

		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		now := time.Now().UTC()
		var nextRun *time.Time
		if enabled {
			firing := spec.Next(now, loc)
			if firing.IsZero() {
				writeJobError(c, errScheduleInvalid)
				return
			}
			firing = firing.UTC()
			nextRun = &firing
		}

		job := &store.Job{
			ID:         newID(),
			Name:       request.Name,
			Expression: request.Expression,
			Timezone:   request.Timezone,
			Enabled:    enabled,
			CreatedAt:  now,
			UpdatedAt:  now,
			NextRun:    nextRun,
		}
		saved, err := st.CreateJob(job)
		if err != nil {
			writeStorageError(c, err)
			return
		}
		c.JSON(http.StatusCreated, toJobResponse(saved))
	}
}

func getJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := st.GetJob(c.Param("id"))
		if err != nil {
			writeLookupError(c, err)
			return
		}
		c.JSON(http.StatusOK, toJobResponse(job))
	}
}

func patchJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := st.GetJob(c.Param("id"))
		if err != nil {
			writeLookupError(c, err)
			return
		}

		var request patchJobRequest
		if !decodeJobBody(c, &request) {
			return
		}

		if request.Name != nil {
			if err := validateName(*request.Name); err != nil {
				writeJobError(c, err)
				return
			}
			job.Name = *request.Name
		}

		expression := job.Expression
		if request.Expression != nil {
			expression = *request.Expression
		}
		timezoneName := job.Timezone
		if request.Timezone != nil {
			timezoneName = *request.Timezone
		}
		enabled := job.Enabled
		if request.Enabled != nil {
			enabled = *request.Enabled
		}

		loc, err := time.LoadLocation(timezoneName)
		if err != nil {
			writeJobError(c, errTimezoneInvalid)
			return
		}
		spec, err := schedule.Parse(expression)
		if err != nil {
			writeJobError(c, errScheduleInvalid)
			return
		}

		scheduleChanged := request.Expression != nil
		timezoneChanged := request.Timezone != nil
		enabledChanged := request.Enabled != nil && *request.Enabled != job.Enabled

		job.Expression = expression
		job.Timezone = timezoneName
		job.Enabled = enabled
		job.UpdatedAt = time.Now().UTC()

		switch {
		case !enabled:
			job.NextRun = nil
		case scheduleChanged || timezoneChanged || enabledChanged:
			firing := spec.Next(job.UpdatedAt, loc)
			if firing.IsZero() {
				writeJobError(c, errScheduleInvalid)
				return
			}
			firing = firing.UTC()
			job.NextRun = &firing
		}

		saved, err := st.UpdateJob(job)
		if err != nil {
			writeStorageError(c, err)
			return
		}
		c.JSON(http.StatusOK, toJobResponse(saved))
	}
}

func deleteJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := st.DeleteJob(c.Param("id"), time.Now().UTC()); err != nil {
			writeLookupError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func listPendingJobs(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		state := c.Query("state")
		if state == "" {
			state = "pending"
		}
		if state != "pending" && state != "executed" {
			writeJobError(c, errTimeWindowInvalid)
			return
		}
		from, to, ok := parseTimeWindow(c)
		if !ok {
			return
		}

		if state == "executed" {
			attempts, err := st.ExecutedAttempts(from, to)
			if err != nil {
				writeJobError(c, errStorageUnavailable)
				return
			}
			responses := make([]executedRunResponse, 0, len(attempts))
			for _, attempt := range attempts {
				responses = append(responses, executedRunResponse{
					JobID:        attempt.JobID,
					RunID:        attempt.RunID,
					ScheduledFor: formatTime(attempt.ScheduledFor),
					Attempt:      attempt.Attempt,
					StartedAt:    formatTime(attempt.StartedAt),
					FinishedAt:   formatTime(attempt.FinishedAt),
					Outcome:      attempt.Outcome,
					Error:        attempt.Error,
				})
			}
			c.JSON(http.StatusOK, responses)
			return
		}

		jobs, err := st.PendingJobs(from, to)
		if err != nil {
			writeJobError(c, errStorageUnavailable)
			return
		}
		responses := make([]jobResponse, 0, len(jobs))
		for _, job := range jobs {
			responses = append(responses, toJobResponse(job))
		}
		c.JSON(http.StatusOK, responses)
	}
}

// parseTimeWindow validates the shared from/to query contract: both
// parameters are required RFC3339 timestamps and from must precede to. The
// returned instants are normalized to UTC.
func parseTimeWindow(c *gin.Context) (time.Time, time.Time, bool) {
	fromText, hasFrom := c.GetQuery("from")
	toText, hasTo := c.GetQuery("to")
	if !hasFrom || !hasTo {
		writeJobError(c, errTimeWindowRequired)
		return time.Time{}, time.Time{}, false
	}
	from, err := time.Parse(time.RFC3339, fromText)
	if err != nil {
		writeJobError(c, errTimeWindowInvalid)
		return time.Time{}, time.Time{}, false
	}
	to, err := time.Parse(time.RFC3339, toText)
	if err != nil {
		writeJobError(c, errTimeWindowInvalid)
		return time.Time{}, time.Time{}, false
	}
	if !from.Before(to) {
		writeJobError(c, errTimeWindowInvalid)
		return time.Time{}, time.Time{}, false
	}
	return from.UTC(), to.UTC(), true
}

func decodeJobBody(c *gin.Context, target any) bool {
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(target); err != nil {
		writeJobError(c, errInvalidJSON)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJobError(c, errInvalidJSON)
		return false
	}
	return true
}

func validateName(name string) *jobError {
	if strings.TrimSpace(name) == "" {
		return errNameRequired
	}
	return nil
}

func validateTimezone(name string) (*time.Location, *jobError) {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, errTimezoneInvalid
	}
	return loc, nil
}

func validateSchedule(expr string) (schedule.Spec, *jobError) {
	spec, err := schedule.Parse(expr)
	if err != nil {
		return schedule.Spec{}, errScheduleInvalid
	}
	return spec, nil
}

func toJobResponse(job *store.Job) jobResponse {
	response := jobResponse{
		ID:         job.ID,
		Name:       job.Name,
		Expression: job.Expression,
		Timezone:   job.Timezone,
		Enabled:    job.Enabled,
		CreatedAt:  formatTime(job.CreatedAt),
	}
	if job.NextRun != nil {
		formatted := formatTime(*job.NextRun)
		response.NextRun = &formatted
	}
	return response
}

func formatTime(instant time.Time) string {
	return instant.UTC().Format(time.RFC3339)
}

// newID returns a random 128-bit hex identifier; no external UUID library is
// needed for the documented contract.
func newID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand only fails in environments where the process cannot
		// run safely at all; keep identifiers unique via the clock fallback.
		return time.Now().UTC().Format("20060102T150405.000000000") + "-fallback"
	}
	// Set version 4 and variant bits so the identifier reads as a UUID.
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// jobError carries the published error code and HTTP status for validation
// failures; messages intentionally never expose storage details.
type jobError struct {
	status  int
	code    string
	message string
}

func (e *jobError) Error() string { return e.code }

var (
	errInvalidJSON = &jobError{
		status: http.StatusBadRequest, code: "invalid_json",
		message: "request body must be a single valid JSON object",
	}
	errNameRequired = &jobError{
		status: http.StatusUnprocessableEntity, code: "name_required",
		message: "name is required",
	}
	errNameConflict = &jobError{
		status: http.StatusUnprocessableEntity, code: "name_conflict",
		message: "a job with this name already exists",
	}
	errScheduleInvalid = &jobError{
		status: http.StatusUnprocessableEntity, code: "schedule_invalid",
		message: "cron expression must be five fields: minute hour day-of-month month day-of-week",
	}
	errTimezoneInvalid = &jobError{
		status: http.StatusUnprocessableEntity, code: "timezone_invalid",
		message: "timezone must be a valid IANA time zone name",
	}
	errTimeWindowRequired = &jobError{
		status: http.StatusBadRequest, code: "time_window_required",
		message: "both from and to query parameters are required",
	}
	errTimeWindowInvalid = &jobError{
		status: http.StatusBadRequest, code: "time_window_invalid",
		message: "from and to must be RFC3339 timestamps with from earlier than to",
	}
	errJobNotFound = &jobError{
		status: http.StatusNotFound, code: "job_not_found",
		message: "the requested job does not exist",
	}
	errRunNotFound = &jobError{
		status: http.StatusNotFound, code: "run_not_found",
		message: "the requested run does not exist",
	}
	errRetryNotAllowed = &jobError{
		status: http.StatusConflict, code: "retry_not_allowed",
		message: "only the latest result of a failed run can be retried",
	}
	errDispatchTimeInvalid = &jobError{
		status: http.StatusUnprocessableEntity, code: "dispatch_time_invalid",
		message: "expected_next_run and before must be RFC3339 timestamps",
	}
	errNextRunConflict = &jobError{
		status: http.StatusConflict, code: "next_run_conflict",
		message: "expected_next_run does not match the current next_run",
	}
	errDispatchNotDue = &jobError{
		status: http.StatusConflict, code: "dispatch_not_due",
		message: "the current next_run is later than before",
	}
	errRunTimeInvalid = &jobError{
		status: http.StatusUnprocessableEntity, code: "run_time_invalid",
		message: "started_at must not precede scheduled_for and finished_at must not precede started_at",
	}
	errRunOutcomeInvalid = &jobError{
		status: http.StatusUnprocessableEntity, code: "run_outcome_invalid",
		message: "outcome must be succeeded or failed, with error omitted on success and non-blank on failure",
	}
	errIdempotencyKeyInvalid = &jobError{
		status: http.StatusUnprocessableEntity, code: "idempotency_key_invalid",
		message: "idempotency_key must be a non-empty string of at most 128 characters",
	}
	errIdempotencyConflict = &jobError{
		status: http.StatusConflict, code: "idempotency_conflict",
		message: "the idempotency key was already used with different request semantics",
	}
	errStorageUnavailable = &jobError{
		status: http.StatusServiceUnavailable, code: "storage_unavailable",
		message: "database is not available",
	}
	errRouteNotFound = &jobError{
		status: http.StatusNotFound, code: "route_not_found",
		message: "no route matches this path",
	}
)

func writeJobError(c *gin.Context, err *jobError) {
	c.AbortWithStatusJSON(err.status, gin.H{
		"error": gin.H{"code": err.code, "message": err.message},
	})
}

func writeLookupError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJobError(c, errJobNotFound)
	default:
		writeJobError(c, errStorageUnavailable)
	}
}

func writeStorageError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJobError(c, errJobNotFound)
	case errors.Is(err, store.ErrNameConflict):
		writeJobError(c, errNameConflict)
	case errors.Is(err, sql.ErrConnDone):
		writeJobError(c, errStorageUnavailable)
	default:
		writeJobError(c, errStorageUnavailable)
	}
}
