// Package api wires the public HTTP surface onto the service layer.
package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/cron-job-manager/internal/service"
	"github.com/luwa07832/cron-job-manager/internal/store"
)

// NewRouter wires the public HTTP surface. Every error response keeps the
// single top-level error object with code and message fields described in
// README.md.
func NewRouter(st *store.Store) *gin.Engine {
	return NewRouterWithService(service.New(st))
}

// NewRouterWithService allows tests to inject a configured service.
func NewRouterWithService(svc *service.Service) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	api := &handler{service: svc}

	router.GET("/healthz", api.healthz)

	v1 := router.Group("/api/v1")
	{
		v1.POST("/tasks/schedules/validate", api.validateSchedule)
		v1.POST("/tasks", api.createTask)
		v1.GET("/tasks", api.listTasks)
		v1.GET("/tasks/:id", api.getTask)
		v1.PUT("/tasks/:id", api.updateTask)
		v1.DELETE("/tasks/:id", api.deleteTask)

		v1.POST("/tasks/:id/trigger", api.triggerTask)
		v1.POST("/tasks/:id/runs", api.manualRun)
		v1.POST("/runs/:runId/retry", api.retryRun)
		v1.GET("/runs/:runId", api.getRun)

		v1.GET("/runs", api.queryWindow)
	}

	router.NoRoute(func(c *gin.Context) {
		writeError(c, http.StatusNotFound, "route_not_found", "no route matches this path")
	})
	return router
}

type handler struct {
	service *service.Service
}

func (h *handler) healthz(c *gin.Context) {
	if err := h.service.Ping(); err != nil {
		writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// mapServiceError translates service sentinel errors into the unique error
// codes promised by the API contract.
func mapServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrTaskNotFound):
		writeError(c, http.StatusNotFound, "task_not_found", "task does not exist")
	case errors.Is(err, service.ErrRunNotFound):
		writeError(c, http.StatusNotFound, "run_not_found", "run record does not exist")
	case errors.Is(err, service.ErrTaskIDDuplicate):
		writeError(c, http.StatusConflict, "task_id_conflict", "task id already exists")
	case errors.Is(err, service.ErrInvalidSchedule):
		writeError(c, http.StatusBadRequest, "invalid_schedule", "schedule expression is invalid")
	case errors.Is(err, service.ErrInvalidTask):
		writeError(c, http.StatusBadRequest, "invalid_task", "task definition is invalid")
	case errors.Is(err, service.ErrInvalidAction):
		writeError(c, http.StatusBadRequest, "invalid_action", "task action is invalid")
	case errors.Is(err, service.ErrRunNotRetriable):
		writeError(c, http.StatusConflict, "run_not_retriable", "only failed run records can be retried")
	case errors.Is(err, service.ErrInvalidWindow):
		writeError(c, http.StatusBadRequest, "invalid_time_window", "window start must not be later than window end")
	case errors.Is(err, service.ErrWindowTooLarge):
		writeError(c, http.StatusBadRequest, "query_range_too_large", "query window exceeds the maximum allowed span")
	case errors.Is(err, service.ErrInvalidTimeInput):
		writeError(c, http.StatusBadRequest, "invalid_time_format", "time must be an RFC3339 timestamp with offset")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "request failed due to an internal error")
	}
}
