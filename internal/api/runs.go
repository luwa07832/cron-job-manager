package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/cron-job-manager/internal/store"
)

type createRunRequest struct {
	ScheduledFor   *string `json:"scheduled_for"`
	StartedAt      string  `json:"started_at"`
	FinishedAt     string  `json:"finished_at"`
	Outcome        string  `json:"outcome"`
	Error          *string `json:"error"`
	IdempotencyKey *string `json:"idempotency_key"`
}

type retryRunRequest struct {
	StartedAt      string  `json:"started_at"`
	FinishedAt     string  `json:"finished_at"`
	Outcome        string  `json:"outcome"`
	Error          *string `json:"error"`
	IdempotencyKey *string `json:"idempotency_key"`
}

type attemptResponse struct {
	Attempt    int     `json:"attempt"`
	StartedAt  string  `json:"started_at"`
	FinishedAt string  `json:"finished_at"`
	Outcome    string  `json:"outcome"`
	Error      *string `json:"error"`
}

type runResponse struct {
	RunID        string            `json:"run_id"`
	JobID        string            `json:"job_id"`
	ScheduledFor string            `json:"scheduled_for"`
	Results      []attemptResponse `json:"results"`
}

type executedRunResponse struct {
	JobID        string  `json:"job_id"`
	RunID        string  `json:"run_id"`
	ScheduledFor string  `json:"scheduled_for"`
	Attempt      int     `json:"attempt"`
	StartedAt    string  `json:"started_at"`
	FinishedAt   string  `json:"finished_at"`
	Outcome      string  `json:"outcome"`
	Error        *string `json:"error"`
}

func registerRuns(router *gin.Engine, st *store.Store) {
	router.POST("/api/v1/jobs/:id/runs", createRun(st))
	router.POST("/api/v1/jobs/:id/runs/:run_id/retries", retryRun(st))
	router.GET("/api/v1/jobs/:id/runs", listJobRuns(st))
	router.GET("/api/v1/jobs/:id/runs/:run_id", getRun(st))
}

func createRun(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		jobID := c.Param("id")
		if exists, err := st.JobExists(jobID); err != nil {
			writeJobError(c, errStorageUnavailable)
			return
		} else if !exists {
			writeJobError(c, errJobNotFound)
			return
		}

		var request createRunRequest
		if !decodeJobBody(c, &request) {
			return
		}
		if request.ScheduledFor == nil {
			writeJobError(c, errRunTimeInvalid)
			return
		}
		scheduledFor, startedAt, finishedAt, ok := parseRunWindow(c, *request.ScheduledFor, request.StartedAt, request.FinishedAt)
		if !ok {
			return
		}
		outcome, failure, ok := validateOutcome(c, request.Outcome, request.Error)
		if !ok {
			return
		}
		idem, ok := parseIdempotencyKey(c, request.IdempotencyKey)
		if !ok {
			return
		}
		fingerprint := runFingerprint(scheduledFor, startedAt, finishedAt, outcome, failure)

		attempt := &store.Attempt{
			JobID:        jobID,
			RunID:        newID(),
			Attempt:      1,
			ScheduledFor: scheduledFor,
			StartedAt:    startedAt,
			FinishedAt:   finishedAt,
			Outcome:      outcome,
			Error:        failure,
		}
		if idem != nil {
			idem.Fingerprint = fingerprint
		}
		run, err := st.CreateRunWithIdempotency(attempt, idem)
		if err != nil {
			writeRunWriteError(c, err)
			return
		}
		c.JSON(http.StatusCreated, toRunResponse(run))
	}
}

func retryRun(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		jobID := c.Param("id")
		runID := c.Param("run_id")
		if exists, err := st.JobExists(jobID); err != nil {
			writeJobError(c, errStorageUnavailable)
			return
		} else if !exists {
			writeJobError(c, errJobNotFound)
			return
		}

		var request retryRunRequest
		if !decodeJobBody(c, &request) {
			return
		}

		run, err := st.GetRun(jobID, runID)
		if err != nil {
			writeRunLookupError(c, err)
			return
		}
		latest := run.Attempts[len(run.Attempts)-1]

		idem, ok := parseIdempotencyKey(c, request.IdempotencyKey)
		if !ok {
			return
		}

		// A keyed replay must return the stored response even when the run
		// has since advanced past failure, so key handling precedes the
		// latest-attempt gate. Semantic validation stays first so that
		// malformed requests never reserve the key.
		if idem != nil {
			startedAt, finishedAt, ok := parseRunTimes(c, latest.ScheduledFor, request.StartedAt, request.FinishedAt)
			if !ok {
				return
			}
			outcome, failure, ok := validateOutcome(c, request.Outcome, request.Error)
			if !ok {
				return
			}
			idem.Fingerprint = runFingerprint(latest.ScheduledFor, startedAt, finishedAt, outcome, failure)
			fresh, err := st.AppendRetryWithIdempotency(jobID, runID, startedAt, finishedAt, outcome, failure, idem)
			if err != nil {
				writeRunWriteError(c, err)
				return
			}
			c.JSON(http.StatusCreated, toRunResponse(fresh))
			return
		}

		if latest.Outcome != "failed" {
			writeJobError(c, errRetryNotAllowed)
			return
		}

		startedAt, finishedAt, ok := parseRunTimes(c, latest.ScheduledFor, request.StartedAt, request.FinishedAt)
		if !ok {
			return
		}
		outcome, failure, ok := validateOutcome(c, request.Outcome, request.Error)
		if !ok {
			return
		}

		fresh, err := st.AppendRetryWithIdempotency(jobID, runID, startedAt, finishedAt, outcome, failure, nil)
		if err != nil {
			writeRunWriteError(c, err)
			return
		}
		c.JSON(http.StatusCreated, toRunResponse(fresh))
	}
}

func getRun(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		run, err := st.GetRun(c.Param("id"), c.Param("run_id"))
		if err != nil {
			writeRunLookupError(c, err)
			return
		}
		c.JSON(http.StatusOK, toRunResponse(run))
	}
}

func listJobRuns(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		from, to, ok := parseTimeWindow(c)
		if !ok {
			return
		}
		runs, err := st.RunsForJob(c.Param("id"), from, to)
		if err != nil {
			writeRunLookupError(c, err)
			return
		}
		responses := make([]runResponse, 0, len(runs))
		for _, run := range runs {
			responses = append(responses, toRunResponse(run))
		}
		c.JSON(http.StatusOK, responses)
	}
}

// maxIdempotencyKeyRunes bounds the key by Unicode code points, not bytes.
const maxIdempotencyKeyRunes = 128

// parseIdempotencyKey validates the optional key. A nil or omitted field
// keeps the legacy behavior; a blank or over-long value is rejected and such
// requests never reserve the key.
func parseIdempotencyKey(c *gin.Context, value *string) (*store.IdempotencyRequest, bool) {
	if value == nil {
		return nil, true
	}
	if strings.TrimSpace(*value) == "" || utf8.RuneCountInString(*value) > maxIdempotencyKeyRunes {
		writeJobError(c, errIdempotencyKeyInvalid)
		return nil, false
	}
	return &store.IdempotencyRequest{Key: *value}, true
}

// runFingerprint serializes the request semantics after normalization. All
// instants are UTC RFC3339; the error pointer distinguishes omitted/null from
// a concrete failure message. Field order is fixed by the struct, making the
// encoding stable for byte comparison.
type runFingerprintPayload struct {
	ScheduledFor string  `json:"scheduled_for"`
	StartedAt    string  `json:"started_at"`
	FinishedAt   string  `json:"finished_at"`
	Outcome      string  `json:"outcome"`
	Error        *string `json:"error"`
}

func runFingerprint(scheduledFor, startedAt, finishedAt time.Time, outcome string, failure *string) string {
	payload := runFingerprintPayload{
		ScheduledFor: formatTime(scheduledFor),
		StartedAt:    formatTime(startedAt),
		FinishedAt:   formatTime(finishedAt),
		Outcome:      outcome,
		Error:        failure,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// The payload contains only strings and a nilable string, so
		// encoding cannot fail; keep a deterministic fallback anyway.
		return payload.ScheduledFor + "|" + payload.StartedAt + "|" + payload.FinishedAt + "|" + payload.Outcome
	}
	return string(encoded)
}

func parseRunWindow(c *gin.Context, scheduledText, startedText, finishedText string) (time.Time, time.Time, time.Time, bool) {
	scheduledFor, err := time.Parse(time.RFC3339, scheduledText)
	if err != nil {
		writeJobError(c, errRunTimeInvalid)
		return time.Time{}, time.Time{}, time.Time{}, false
	}
	startedAt, finishedAt, ok := parseRunTimes(c, scheduledFor, startedText, finishedText)
	return scheduledFor.UTC(), startedAt, finishedAt, ok
}

func parseRunTimes(c *gin.Context, scheduledFor time.Time, startedText, finishedText string) (time.Time, time.Time, bool) {
	startedAt, err := time.Parse(time.RFC3339, startedText)
	if err != nil {
		writeJobError(c, errRunTimeInvalid)
		return time.Time{}, time.Time{}, false
	}
	finishedAt, err := time.Parse(time.RFC3339, finishedText)
	if err != nil {
		writeJobError(c, errRunTimeInvalid)
		return time.Time{}, time.Time{}, false
	}
	startedAt = startedAt.UTC()
	finishedAt = finishedAt.UTC()
	scheduledFor = scheduledFor.UTC()
	if startedAt.Before(scheduledFor) || finishedAt.Before(startedAt) {
		writeJobError(c, errRunTimeInvalid)
		return time.Time{}, time.Time{}, false
	}
	return startedAt, finishedAt, true
}

func validateOutcome(c *gin.Context, outcome string, failure *string) (string, *string, bool) {
	if outcome != "succeeded" && outcome != "failed" {
		writeJobError(c, errRunOutcomeInvalid)
		return "", nil, false
	}
	switch {
	case outcome == "succeeded":
		if failure != nil {
			writeJobError(c, errRunOutcomeInvalid)
			return "", nil, false
		}
		return outcome, nil, true
	default:
		if failure == nil || strings.TrimSpace(*failure) == "" {
			writeJobError(c, errRunOutcomeInvalid)
			return "", nil, false
		}
		return outcome, failure, true
	}
}

func toRunResponse(run *store.Run) runResponse {
	results := make([]attemptResponse, 0, len(run.Attempts))
	for _, attempt := range run.Attempts {
		results = append(results, attemptResponse{
			Attempt:    attempt.Attempt,
			StartedAt:  formatTime(attempt.StartedAt),
			FinishedAt: formatTime(attempt.FinishedAt),
			Outcome:    attempt.Outcome,
			Error:      attempt.Error,
		})
	}
	return runResponse{
		RunID:        run.RunID,
		JobID:        run.JobID,
		ScheduledFor: formatTime(run.ScheduledFor),
		Results:      results,
	}
}

func writeRunLookupError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJobError(c, errJobNotFound)
	case errors.Is(err, store.ErrRunNotFound):
		writeJobError(c, errRunNotFound)
	default:
		writeJobError(c, errStorageUnavailable)
	}
}

func writeRunWriteError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJobError(c, errJobNotFound)
	case errors.Is(err, store.ErrRunNotFound):
		writeJobError(c, errRunNotFound)
	case errors.Is(err, store.ErrRetryNotAllowed):
		writeJobError(c, errRetryNotAllowed)
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeJobError(c, errIdempotencyConflict)
	default:
		writeJobError(c, errStorageUnavailable)
	}
}
