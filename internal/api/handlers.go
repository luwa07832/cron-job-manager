package api

import (
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

// MaxWindowSpan bounds how wide one time-window query may be (one year plus a
// day, enough to include a leap-day while staying finite).
const MaxWindowSpan = 366 * 24 * time.Hour

const maxJobNameLength = 256
const maxFailureReasonLength = 4096

type jobPayload struct {
	Name           *string `json:"name"`
	CronExpression *string `json:"cron_expression"`
}

type runPayload struct {
	ScheduledFor  *string `json:"scheduled_for"`
	TriggeredAt   *string `json:"triggered_at"`
	StartedAt     *string `json:"started_at"`
	FinishedAt    *string `json:"finished_at"`
	Result        *string `json:"result"`
	FailureReason *string `json:"failure_reason"`
}

type retryPayload struct {
	ScheduledFor  *string `json:"scheduled_for"`
	TriggeredAt   *string `json:"triggered_at"`
	StartedAt     *string `json:"started_at"`
	FinishedAt    *string `json:"finished_at"`
	Result        *string `json:"result"`
	FailureReason *string `json:"failure_reason"`
}

// parseBody decodes exactly one JSON object from the body and rejects
// trailing data and non-object values.
func parseBody(c *gin.Context, dst any) *apiError {
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return badRequest(codeInvalidJSON, "request body must be a JSON object")
		}
		return badRequest(codeInvalidJSON, "request body is not valid JSON")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return badRequest(codeInvalidJSON, "request body must contain a single JSON object")
	}
	return nil
}

func requireString(value *string, code, message string) (string, *apiError) {
	if value == nil {
		return "", badRequest(code, message)
	}
	return *value, nil
}

func validateJob(payload jobPayload) (string, string, *apiError) {
	name, apiErr := requireString(payload.Name, codeInvalidName, "job name is required")
	if apiErr != nil {
		return "", "", apiErr
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxJobNameLength {
		return "", "", badRequest(codeInvalidName, "job name must be between 1 and 256 characters")
	}
	expression, apiErr := requireString(payload.CronExpression, codeInvalidExpression, "cron_expression is required")
	if apiErr != nil {
		return "", "", apiErr
	}
	expression = strings.TrimSpace(expression)
	if _, err := schedule.Parse(expression); err != nil {
		return "", "", badRequest(codeInvalidExpression, "cron_expression is not a valid five-field cron expression")
	}
	return name, expression, nil
}

// validateOutcome parses the execution outcome shared by trigger and retry
// entries. Success never carries a reason; failure always carries one.
func validateOutcome(resultPtr *string, reasonPtr *string) (string, string, *apiError) {
	result, apiErr := requireString(resultPtr, codeInvalidResult, `result is required and must be "success" or "failure"`)
	if apiErr != nil {
		return "", "", apiErr
	}
	result = strings.ToLower(strings.TrimSpace(result))
	if result != store.ResultSuccess && result != store.ResultFailure {
		return "", "", badRequest(codeInvalidResult, `result must be "success" or "failure"`)
	}
	reason := ""
	if reasonPtr != nil {
		reason = strings.TrimSpace(*reasonPtr)
	}
	switch result {
	case store.ResultFailure:
		if reason == "" {
			return "", "", badRequest(codeInvalidFailureReason, "failure_reason is required when result is failure")
		}
		if len(reason) > maxFailureReasonLength {
			return "", "", badRequest(codeInvalidFailureReason, "failure_reason must be at most 4096 characters")
		}
	case store.ResultSuccess:
		if reasonPtr != nil && reason != "" {
			return "", "", badRequest(codeInvalidFailureReason, "failure_reason must be absent when result is success")
		}
	}
	return result, reason, nil
}

func parseTimeField(name string, raw *string, required bool) (time.Time, *apiError) {
	if raw == nil {
		if required {
			return time.Time{}, badRequest(codeInvalidTime, name+" is required and must be an RFC3339 timestamp")
		}
		return time.Time{}, nil
	}
	value, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*raw))
	if err != nil {
		return time.Time{}, badRequest(codeInvalidTime, name+" must be an RFC3339 timestamp")
	}
	return value.UTC(), nil
}

func nextScheduled(expression string, after time.Time) (time.Time, error) {
	cron, err := schedule.Parse(expression)
	if err != nil {
		return time.Time{}, err
	}
	return cron.Next(after.UTC())
}

// jobView assembles a job response with the current next trigger time.
func jobView(job store.Job, now time.Time) (jobResponse, *apiError) {
	next, err := nextScheduled(job.Expression, now)
	if err != nil {
		return jobResponse{}, badRequest(codeInvalidExpression, "cron_expression produces no future trigger time")
	}
	return jobResponse{
		ID:               job.ID,
		Name:             job.Name,
		CronExpression:   job.Expression,
		NextScheduledFor: timeJSON(next),
		CreatedAt:        timeJSON(job.CreatedAt),
		UpdatedAt:        timeJSON(job.UpdatedAt),
	}, nil
}

func (h *Handlers) createJob(c *gin.Context) {
	var payload jobPayload
	if apiErr := parseBody(c, &payload); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	name, expression, apiErr := validateJob(payload)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	now := h.now().UTC()
	job, err := h.store.CreateJob(store.NewJob{Name: name, Expression: expression}, now)
	if err != nil {
		writeError(c, mapStoreError(err))
		return
	}
	view, apiErr := jobView(job, now)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	c.JSON(http.StatusCreated, view)
}

func (h *Handlers) getJob(c *gin.Context) {
	job, err := h.store.GetJob(c.Param("jobID"))
	if err != nil {
		writeError(c, mapStoreError(err))
		return
	}
	view, apiErr := jobView(job, h.now().UTC())
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *Handlers) updateJob(c *gin.Context) {
	var payload jobPayload
	if apiErr := parseBody(c, &payload); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	name, expression, apiErr := validateJob(payload)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	now := h.now().UTC()
	job, err := h.store.UpdateJob(c.Param("jobID"), store.NewJob{Name: name, Expression: expression}, now)
	if err != nil {
		writeError(c, mapStoreError(err))
		return
	}
	view, apiErr := jobView(job, now)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *Handlers) deleteJob(c *gin.Context) {
	if err := h.store.DeleteJob(c.Param("jobID")); err != nil {
		writeError(c, mapStoreError(err))
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handlers) triggerRun(c *gin.Context) {
	h.recordRun(c, store.TriggerScheduled)
}

func (h *Handlers) manualRun(c *gin.Context) {
	h.recordRun(c, store.TriggerManual)
}

func (h *Handlers) recordRun(c *gin.Context, triggerType string) {
	var payload runPayload
	if apiErr := parseBody(c, &payload); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	result, reason, apiErr := validateOutcome(payload.Result, payload.FailureReason)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	now := h.now().UTC()
	timing, apiErr := resolveRunTiming(payload.ScheduledFor, payload.TriggeredAt, payload.StartedAt, payload.FinishedAt, now)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	job, err := h.store.GetJob(c.Param("jobID"))
	if err != nil {
		writeError(c, mapStoreError(err))
		return
	}
	next, err := nextScheduled(job.Expression, timing.finishedAt)
	if err != nil {
		writeError(c, badRequest(codeInvalidExpression, "cron_expression produces no future trigger time"))
		return
	}
	record, err := h.store.RecordRun(store.NewRun{
		JobID:           job.ID,
		RecordType:      store.RecordOriginal,
		TriggerType:     triggerType,
		ScheduledFor:    timing.scheduledFor,
		TriggeredAt:     timing.triggeredAt,
		StartedAt:       timing.startedAt,
		FinishedAt:      timing.finishedAt,
		Result:          result,
		FailureReason:   reason,
		NextScheduledAt: next,
	})
	if err != nil {
		writeError(c, mapStoreError(err))
		return
	}
	c.JSON(http.StatusCreated, toRunResponse(record))
}

func (h *Handlers) retryRun(c *gin.Context) {
	var payload retryPayload
	if apiErr := parseBody(c, &payload); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	result, reason, apiErr := validateOutcome(payload.Result, payload.FailureReason)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	originalSeq, apiErr := parseSeq(c.Param("seq"))
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	now := h.now().UTC()
	timing, apiErr := resolveRunTiming(payload.ScheduledFor, payload.TriggeredAt, payload.StartedAt, payload.FinishedAt, now)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	original, err := h.store.GetRun(originalSeq)
	if err != nil {
		writeError(c, mapStoreError(err))
		return
	}
	next, err := nextScheduled(original.Expression, timing.finishedAt)
	if err != nil {
		writeError(c, badRequest(codeInvalidExpression, "cron_expression produces no future trigger time"))
		return
	}
	record, err := h.store.RecordRetry(store.NewRun{
		JobID:           original.JobID,
		RetryOfSeq:      originalSeq,
		ScheduledFor:    timing.scheduledFor,
		TriggeredAt:     timing.triggeredAt,
		StartedAt:       timing.startedAt,
		FinishedAt:      timing.finishedAt,
		Result:          result,
		FailureReason:   reason,
		NextScheduledAt: next,
	})
	if err != nil {
		writeError(c, mapStoreError(err))
		return
	}
	c.JSON(http.StatusCreated, toRunResponse(record))
}

type runTimingResult struct {
	scheduledFor time.Time
	triggeredAt  time.Time
	startedAt    time.Time
	finishedAt   time.Time
}

func resolveRunTiming(scheduledPtr, triggeredPtr, startedPtr, finishedPtr *string, now time.Time) (runTimingResult, *apiError) {
	scheduledFor, apiErr := parseTimeField("scheduled_for", scheduledPtr, false)
	if apiErr != nil {
		return runTimingResult{}, apiErr
	}
	if scheduledFor.IsZero() {
		scheduledFor = now
	}
	triggeredAt, apiErr := parseTimeField("triggered_at", triggeredPtr, false)
	if apiErr != nil {
		return runTimingResult{}, apiErr
	}
	if triggeredAt.IsZero() {
		triggeredAt = latest(now, scheduledFor)
	}
	startedAt, apiErr := parseTimeField("started_at", startedPtr, false)
	if apiErr != nil {
		return runTimingResult{}, apiErr
	}
	if startedAt.IsZero() {
		startedAt = triggeredAt
	}
	finishedAt, apiErr := parseTimeField("finished_at", finishedPtr, false)
	if apiErr != nil {
		return runTimingResult{}, apiErr
	}
	if finishedAt.IsZero() {
		finishedAt = startedAt
	}
	if triggeredAt.Before(scheduledFor) || startedAt.Before(triggeredAt) || finishedAt.Before(startedAt) {
		return runTimingResult{}, badRequest(codeInvalidTime, "timing fields must satisfy scheduled_for <= triggered_at <= started_at <= finished_at")
	}
	return runTimingResult{
		scheduledFor: scheduledFor,
		triggeredAt:  triggeredAt,
		startedAt:    startedAt,
		finishedAt:   finishedAt,
	}, nil
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
