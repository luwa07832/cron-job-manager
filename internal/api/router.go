package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/cron-job-manager/internal/store"
)

// Handlers carries the dependencies shared by every HTTP entry.
type Handlers struct {
	store *store.Store
	clock func() time.Time
}

// NewHandlers wires a store with the real system clock.
func NewHandlers(st *store.Store) *Handlers {
	return &Handlers{store: st, clock: time.Now}
}

func (h *Handlers) now() time.Time {
	if h.clock != nil {
		return h.clock().UTC()
	}
	return time.Now().UTC()
}

// NewRouter wires the public HTTP surface. Every error response keeps the
// single top-level error object described in README.md.
func NewRouter(st *store.Store) *gin.Engine {
	return NewRouterWithHandlers(NewHandlers(st))
}

// NewRouterWithHandlers is used by tests to inject a deterministic clock.
func NewRouterWithHandlers(h *Handlers) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	router.GET("/healthz", func(c *gin.Context) {
		if err := h.store.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "storage_unavailable", "message": "database is not available"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	v1 := router.Group("/api/v1")
	{
		v1.POST("/jobs", h.createJob)
		v1.GET("/jobs/:jobID", h.getJob)
		v1.PUT("/jobs/:jobID", h.updateJob)
		v1.DELETE("/jobs/:jobID", h.deleteJob)

		v1.POST("/jobs/:jobID/runs", h.triggerRun)
		v1.POST("/jobs/:jobID/manual-runs", h.manualRun)
		v1.POST("/runs/:seq/retries", h.retryRun)

		v1.GET("/run-records", h.queryWindow)
	}

	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
	})
	return router
}

func writeError(c *gin.Context, err *apiError) {
	c.JSON(err.status, gin.H{"error": gin.H{"code": err.code, "message": err.message}})
}
