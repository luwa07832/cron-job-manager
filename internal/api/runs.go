package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/cron-job-manager/internal/store"
)

type createRunRequest struct {
	ScheduledFor *string `json:"scheduled_for"`
	StartedAt    *string `json:"started_at"`
	FinishedAt   *string `json:"finished_at"`
	Outcome      string  `json:"outcome"`
	Error        *string `json:"error"`
}

type retryRequest struct {
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Outcome    string  `json:"outcome"`
	Error      *string `json:"error"`
}

type runAttemptResponse struct {
	JobID        string  `json:"job_id"`
	RunID        string  `json:"run_id"`
	Attempt      int     `json:"attempt"`
	ScheduledFor string  `json:"scheduled_for"`
	StartedAt    string  `json:"started_at"`
	FinishedAt   string  `json:"finished_at"`
	Outcome      string  `json:"outcome"`
	Error        *string `json:"error"`
}

type runResponse struct {
	JobID        string               `json:"job_id"`
	RunID        string               `json:"run_id"`
	ScheduledFor string               `json:"scheduled_for"`
	Attempts     []runAttemptResponse `json:"attempts"`
}

func registerRuns(router *gin.Engine, st *store.Store) {
	router.POST("/api/v1/jobs/:id/runs", createRun(st))
	router.GET("/api/v1/jobs/:id/runs/:run_id", getRun(st))
	router.POST("/api/v1/jobs/:id/runs/:run_id/retries", addRetry(st))
}

func createRun(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		jobID := c.Param("id")
		if _, err := st.GetJob(jobID); err != nil {
			writeRunLookupError(c, err)
			return
		}

		var request createRunRequest
		if !decodeJobBody(c, &request) {
			return
		}

		scheduledFor, ok := parseRunTime(c, request.ScheduledFor)
		if !ok {
			return
		}
		startedAt, ok := parseRunTime(c, request.StartedAt)
		if !ok {
			return
		}
		finishedAt, ok := parseRunTime(c, request.FinishedAt)
		if !ok {
			return
		}
		if startedAt.Before(scheduledFor) || finishedAt.Before(startedAt) {
			writeJobError(c, errRunTimeInvalid)
			return
		}
		message, ok := validateOutcome(c, request.Outcome, request.Error)
		if !ok {
			return
		}

		run, err := st.CreateRun(store.NewRunInput{
			RunID:        newID(),
			JobID:        jobID,
			ScheduledFor: scheduledFor,
			StartedAt:    startedAt,
			FinishedAt:   finishedAt,
			Outcome:      request.Outcome,
			Error:        message,
		})
		if err != nil {
			writeRunWriteError(c, err)
			return
		}
		c.JSON(http.StatusCreated, toRunResponse(run))
	}
}

func addRetry(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		jobID := c.Param("id")
		runID := c.Param("run_id")
		if _, err := st.GetJob(jobID); err != nil {
			writeRunLookupError(c, err)
			return
		}
		existing, err := st.GetRun(jobID, runID)
		if err != nil {
			writeRunLookupError(c, err)
			return
		}
		if existing.Attempts[len(existing.Attempts)-1].Outcome != store.OutcomeFailed {
			writeJobError(c, errRetryNotAllowed)
			return
		}

		var request retryRequest
		if !decodeJobBody(c, &request) {
			return
		}

		startedAt, ok := parseRunTime(c, request.StartedAt)
		if !ok {
			return
		}
		finishedAt, ok := parseRunTime(c, request.FinishedAt)
		if !ok {
			return
		}
		if finishedAt.Before(startedAt) {
			writeJobError(c, errRunTimeInvalid)
			return
		}
		message, ok := validateOutcome(c, request.Outcome, request.Error)
		if !ok {
			return
		}

		run, err := st.AddRetry(jobID, runID, startedAt, finishedAt, request.Outcome, message)
		if err != nil {
			writeRunWriteError(c, err)
			return
		}
		c.JSON(http.StatusCreated, toRunResponse(run))
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

// parseRunTime parses a required RFC3339 timestamp and rejects a missing or
// null field with run_time_invalid.
func parseRunTime(c *gin.Context, raw *string) (time.Time, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		writeJobError(c, errRunTimeInvalid)
		return time.Time{}, false
	}
	instant, err := time.Parse(time.RFC3339, strings.TrimSpace(*raw))
	if err != nil {
		writeJobError(c, errRunTimeInvalid)
		return time.Time{}, false
	}
	return instant.UTC(), true
}

// validateOutcome enforces the succeeded/failed vocabulary and the error
// contract: successes omit or null the error while failures need non-blank
// text.
func validateOutcome(c *gin.Context, outcome string, message *string) (*string, bool) {
	switch outcome {
	case store.OutcomeSucceeded:
		if message != nil && strings.TrimSpace(*message) != "" {
			writeJobError(c, errRunOutcomeInvalid)
			return nil, false
		}
		return nil, true
	case store.OutcomeFailed:
		if message == nil || strings.TrimSpace(*message) == "" {
			writeJobError(c, errRunOutcomeInvalid)
			return nil, false
		}
		return message, true
	default:
		writeJobError(c, errRunOutcomeInvalid)
		return nil, false
	}
}

func toRunResponse(run *store.Run) runResponse {
	response := runResponse{
		JobID:        run.JobID,
		RunID:        run.ID,
		ScheduledFor: formatTime(run.ScheduledFor),
		Attempts:     make([]runAttemptResponse, 0, len(run.Attempts)),
	}
	for _, attempt := range run.Attempts {
		response.Attempts = append(response.Attempts, runAttemptResponse{
			JobID:        attempt.JobID,
			RunID:        attempt.RunID,
			Attempt:      attempt.Attempt,
			ScheduledFor: formatTime(attempt.ScheduledFor),
			StartedAt:    formatTime(attempt.StartedAt),
			FinishedAt:   formatTime(attempt.FinishedAt),
			Outcome:      attempt.Outcome,
			Error:        attempt.Error,
		})
	}
	return response
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
	case errors.Is(err, sql.ErrConnDone):
		writeJobError(c, errStorageUnavailable)
	default:
		writeJobError(c, errStorageUnavailable)
	}
}
