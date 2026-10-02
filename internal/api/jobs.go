package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/gin-gonic/gin"

	cronschedule "github.com/luwa07832/cron-job-manager/internal/cron"
	"github.com/luwa07832/cron-job-manager/internal/store"
)

type createJobRequest struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
	Timezone   string `json:"timezone"`
	Enabled    bool   `json:"enabled"`
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

func registerJobRoutes(router *gin.Engine, st *store.Store) {
	router.POST("/api/v1/jobs", createJob(st))
	router.GET("/api/v1/jobs", listPendingJobs(st))
	router.GET("/api/v1/jobs/:id", getJob(st))
	router.PATCH("/api/v1/jobs/:id", patchJob(st))
	router.DELETE("/api/v1/jobs/:id", deleteJob(st))
}

func createJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request createJobRequest
		if err := decodeJSONBody(c, &request); err != nil {
			writeError(c, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
			return
		}
		if strings.TrimSpace(request.Name) == "" {
			writeError(c, http.StatusUnprocessableEntity, "name_required", "job name is required")
			return
		}
		schedule, ok := validateSchedule(c, request.Expression)
		if !ok {
			return
		}
		location, ok := validateTimezone(c, request.Timezone)
		if !ok {
			return
		}

		job := &store.Job{
			Name:       request.Name,
			Expression: request.Expression,
			Timezone:   request.Timezone,
			Enabled:    request.Enabled,
		}
		if request.Enabled {
			nextRun, err := schedule.NextAfter(time.Now().In(location))
			if err != nil {
				writeError(c, http.StatusUnprocessableEntity, "schedule_invalid", "cron expression is not valid")
				return
			}
			job.NextRun = &nextRun
		}

		if err := st.CreateJob(job); err != nil {
			writeStoreError(c, err)
			return
		}
		c.JSON(http.StatusCreated, toJobResponse(job))
	}
}

func listPendingJobs(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		fromRaw := c.Query("from")
		toRaw := c.Query("to")
		if fromRaw == "" || toRaw == "" {
			writeError(c, http.StatusBadRequest, "time_window_required", "from and to query parameters are required")
			return
		}
		from, err := time.Parse(time.RFC3339, fromRaw)
		if err != nil {
			writeError(c, http.StatusBadRequest, "time_window_invalid", "time window is not valid RFC3339")
			return
		}
		to, err := time.Parse(time.RFC3339, toRaw)
		if err != nil {
			writeError(c, http.StatusBadRequest, "time_window_invalid", "time window is not valid RFC3339")
			return
		}
		if !from.Before(to) {
			writeError(c, http.StatusBadRequest, "time_window_invalid", "time window start must be before its end")
			return
		}

		jobs, err := st.ListPendingJobs(from, to)
		if err != nil {
			writeStoreError(c, err)
			return
		}
		response := make([]jobResponse, 0, len(jobs))
		for _, job := range jobs {
			response = append(response, toJobResponse(job))
		}
		c.JSON(http.StatusOK, response)
	}
}

func getJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := st.GetJob(c.Param("id"))
		if err != nil {
			writeStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, toJobResponse(job))
	}
}

func patchJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := st.GetJob(c.Param("id"))
		if err != nil {
			writeStoreError(c, err)
			return
		}

		var request patchJobRequest
		if err := decodeJSONBody(c, &request); err != nil {
			writeError(c, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
			return
		}

		if request.Name != nil {
			if strings.TrimSpace(*request.Name) == "" {
				writeError(c, http.StatusUnprocessableEntity, "name_required", "job name is required")
				return
			}
			job.Name = *request.Name
		}

		expression := job.Expression
		if request.Expression != nil {
			expression = *request.Expression
		}
		schedule, ok := validateSchedule(c, expression)
		if !ok {
			return
		}
		job.Expression = expression

		timezone := job.Timezone
		if request.Timezone != nil {
			timezone = *request.Timezone
		}
		location, ok := validateTimezone(c, timezone)
		if !ok {
			return
		}
		job.Timezone = timezone

		if request.Enabled != nil {
			job.Enabled = *request.Enabled
		}

		switch {
		case !job.Enabled:
			job.NextRun = nil
		case request.Enabled != nil || request.Expression != nil || request.Timezone != nil:
			nextRun, err := schedule.NextAfter(time.Now().In(location))
			if err != nil {
				writeError(c, http.StatusUnprocessableEntity, "schedule_invalid", "cron expression is not valid")
				return
			}
			job.NextRun = &nextRun
		}

		if err := st.UpdateJob(job); err != nil {
			writeStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, toJobResponse(job))
	}
}

func deleteJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := st.DeleteJob(c.Param("id")); err != nil {
			writeStoreError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func validateSchedule(c *gin.Context, expression string) (*cronschedule.Schedule, bool) {
	schedule, err := cronschedule.Parse(expression)
	if err != nil {
		writeError(c, http.StatusUnprocessableEntity, "schedule_invalid", "cron expression is not valid")
		return nil, false
	}
	return schedule, true
}

func validateTimezone(c *gin.Context, timezone string) (*time.Location, bool) {
	if strings.TrimSpace(timezone) == "" {
		writeError(c, http.StatusUnprocessableEntity, "timezone_invalid", "timezone is not a valid IANA timezone")
		return nil, false
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		writeError(c, http.StatusUnprocessableEntity, "timezone_invalid", "timezone is not a valid IANA timezone")
		return nil, false
	}
	return location, true
}

func decodeJSONBody(c *gin.Context, target any) error {
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain a single JSON value")
		}
		return err
	}
	return nil
}

func writeStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(c, http.StatusNotFound, "job_not_found", "job not found")
	case errors.Is(err, store.ErrNameConflict):
		writeError(c, http.StatusUnprocessableEntity, "name_conflict", "job name already exists")
	default:
		writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "storage is not available")
	}
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func toJobResponse(job *store.Job) jobResponse {
	response := jobResponse{
		ID:         job.ID,
		Name:       job.Name,
		Expression: job.Expression,
		Timezone:   job.Timezone,
		Enabled:    job.Enabled,
		CreatedAt:  job.CreatedAt.UTC().Format(time.RFC3339),
	}
	if job.NextRun != nil {
		formatted := job.NextRun.UTC().Format(time.RFC3339)
		response.NextRun = &formatted
	}
	return response
}
