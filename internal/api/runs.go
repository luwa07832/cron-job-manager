package api

import (
	"crypto/sha256"
	"encoding/hex"
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
		key, ok := validateIdempotencyKey(c, request.IdempotencyKey)
		if !ok {
			return
		}

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
		if key == "" {
			if err := st.CreateRun(attempt); err != nil {
				writeJobError(c, errStorageUnavailable)
				return
			}
			c.JSON(http.StatusCreated, toRunResponse(&store.Run{
				JobID:        attempt.JobID,
				RunID:        attempt.RunID,
				ScheduledFor: attempt.ScheduledFor,
				Attempts:     []*store.Attempt{attempt},
			}))
			return
		}

		fingerprint := runFingerprint(scheduledFor, startedAt, finishedAt, outcome, failure)
		run, err := st.CreateRunIdempotent(attempt, key, fingerprint)
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

		// Without a key the historical ordering is preserved exactly: the
		// retry condition is rejected before request fields are parsed.
		if request.IdempotencyKey == nil {
			latest := run.Attempts[len(run.Attempts)-1]
			if latest.Outcome != "failed" {
				writeJobError(c, errRetryNotAllowed)
				return
			}
			startedAt, finishedAt, ok := parseRunTimes(c, run.ScheduledFor, request.StartedAt, request.FinishedAt)
			if !ok {
				return
			}
			outcome, failure, ok := validateOutcome(c, request.Outcome, request.Error)
			if !ok {
				return
			}
			if _, err := st.AppendRetry(jobID, runID, startedAt, finishedAt, outcome, failure); err != nil {
				writeRunWriteError(c, err)
				return
			}
			fresh, err := st.GetRun(jobID, runID)
			if err != nil {
				writeRunLookupError(c, err)
				return
			}
			c.JSON(http.StatusCreated, toRunResponse(fresh))
			return
		}

		key, ok := validateIdempotencyKey(c, request.IdempotencyKey)
		if !ok {
			return
		}
		startedAt, finishedAt, ok := parseRunTimes(c, run.ScheduledFor, request.StartedAt, request.FinishedAt)
		if !ok {
			return
		}
		outcome, failure, ok := validateOutcome(c, request.Outcome, request.Error)
		if !ok {
			return
		}

		// The atomic store call decides replay versus conflict versus
		// retry_not_allowed, so a matching replay of a request whose run is
		// now terminal still returns the original response.
		fingerprint := retryFingerprint(startedAt, finishedAt, outcome, failure)
		fresh, err := st.AppendRetryIdempotent(jobID, runID, key, fingerprint, startedAt, finishedAt, outcome, failure)
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

const maxIdempotencyKeyLength = 128

// validateIdempotencyKey normalizes the optional field: an absent or null key
// means "no idempotency" and keeps the legacy behavior, while a present key
// must be non-blank and no longer than 128 Unicode characters.
func validateIdempotencyKey(c *gin.Context, value *string) (string, bool) {
	if value == nil {
		return "", true
	}
	key := *value
	if strings.TrimSpace(key) == "" || utf8.RuneCountInString(key) > maxIdempotencyKeyLength {
		writeJobError(c, errIdempotencyKeyInvalid)
		return "", false
	}
	return key, true
}

// requestFingerprint is the canonical form of the fields that define request
// semantics; times are normalized to UTC and stored at nanosecond precision so
// that offset-bearing but equivalent timestamps replay identically.
type requestFingerprint struct {
	ScheduledFor int64   `json:"scheduled_for"`
	StartedAt    int64   `json:"started_at"`
	FinishedAt   int64   `json:"finished_at"`
	Outcome      string  `json:"outcome"`
	Error        *string `json:"error"`
}

// retryFingerprintShape omits scheduled_for, which retries never carry.
type retryFingerprintShape struct {
	StartedAt  int64   `json:"started_at"`
	FinishedAt int64   `json:"finished_at"`
	Outcome    string  `json:"outcome"`
	Error      *string `json:"error"`
}

func runFingerprint(scheduledFor, startedAt, finishedAt time.Time, outcome string, failure *string) string {
	return fingerprintOf(requestFingerprint{
		ScheduledFor: scheduledFor.UnixNano(),
		StartedAt:    startedAt.UnixNano(),
		FinishedAt:   finishedAt.UnixNano(),
		Outcome:      outcome,
		Error:        failure,
	})
}

func retryFingerprint(startedAt, finishedAt time.Time, outcome string, failure *string) string {
	return fingerprintOf(retryFingerprintShape{
		StartedAt:  startedAt.UnixNano(),
		FinishedAt: finishedAt.UnixNano(),
		Outcome:    outcome,
		Error:      failure,
	})
}

func fingerprintOf(shape any) string {
	encoded, err := json.Marshal(shape)
	if err != nil {
		// All shapes are plain structs of strings and numbers; marshalling
		// cannot realistically fail.
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
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
