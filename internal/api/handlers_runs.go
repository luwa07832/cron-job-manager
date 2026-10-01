package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/cron-job-manager/internal/service"
)

type triggerRequest struct {
	PlannedFireTime string `json:"planned_fire_time"`
}

type retryRequest struct {
	PlannedFireTime string `json:"planned_fire_time"`
}

func (h *handler) triggerTask(c *gin.Context) {
	var request triggerRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			writeError(c, http.StatusBadRequest, "invalid_request_body", "request body must be valid JSON")
			return
		}
	}
	planned, hasPlanned, err := parseOptionalTime(request.PlannedFireTime)
	if err != nil {
		mapServiceError(c, err)
		return
	}
	input := service.TriggerInput{}
	if hasPlanned {
		input.PlannedFireTime = planned
	}
	record, err := h.service.TriggerScheduled(c.Param("id"), input)
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"run": record})
}

func (h *handler) manualRun(c *gin.Context) {
	record, err := h.service.RunManual(c.Param("id"))
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"run": record})
}

func (h *handler) retryRun(c *gin.Context) {
	var request retryRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			writeError(c, http.StatusBadRequest, "invalid_request_body", "request body must be valid JSON")
			return
		}
	}
	var planned time.Time
	if request.PlannedFireTime != "" {
		parsed, err := service.ParseTime(request.PlannedFireTime, h.service.Now)
		if err != nil {
			mapServiceError(c, err)
			return
		}
		planned = parsed
	}
	record, err := h.service.Retry(c.Param("runId"), planned)
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"run": record})
}

func (h *handler) getRun(c *gin.Context) {
	record, err := h.service.GetRun(c.Param("runId"))
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": record})
}

func (h *handler) queryWindow(c *gin.Context) {
	startRaw := c.Query("start")
	endRaw := c.Query("end")
	if startRaw == "" || endRaw == "" {
		writeError(c, http.StatusBadRequest, "invalid_time_window", "both start and end query parameters are required")
		return
	}
	start, err := service.ParseTime(startRaw, h.service.Now)
	if err != nil {
		mapServiceError(c, err)
		return
	}
	end, err := service.ParseTime(endRaw, h.service.Now)
	if err != nil {
		mapServiceError(c, err)
		return
	}
	if start.After(end) {
		writeError(c, http.StatusBadRequest, "invalid_time_window", "window start must not be later than window end")
		return
	}
	if end.Sub(start) > service.MaxQuerySpan {
		writeError(c, http.StatusBadRequest, "query_range_too_large", "query window exceeds the maximum allowed span")
		return
	}
	result, err := h.service.QueryWindow(start, end)
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func parseOptionalTime(raw string) (time.Time, bool, error) {
	if raw == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false, service.ErrInvalidTimeInput
	}
	return parsed.UTC(), true, nil
}
