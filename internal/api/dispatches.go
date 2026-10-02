package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/cron-job-manager/internal/schedule"
	"github.com/luwa07832/cron-job-manager/internal/store"
)

type dispatchRequest struct {
	ExpectedNextRun *string `json:"expected_next_run"`
	Before          *string `json:"before"`
}

type dispatchResponse struct {
	JobID       string   `json:"job_id"`
	Occurrences []string `json:"occurrences"`
	NextRun     string   `json:"next_run"`
}

func registerDispatches(router *gin.Engine, st *store.Store) {
	router.POST("/api/v1/jobs/:id/dispatches", dispatchJob(st))
}

func dispatchJob(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dispatchRequest
		if !decodeJobBody(c, &request) {
			return
		}
		if request.ExpectedNextRun == nil || request.Before == nil {
			writeJobError(c, errDispatchTimeInvalid)
			return
		}
		expected, err := time.Parse(time.RFC3339, *request.ExpectedNextRun)
		if err != nil {
			writeJobError(c, errDispatchTimeInvalid)
			return
		}
		before, err := time.Parse(time.RFC3339, *request.Before)
		if err != nil {
			writeJobError(c, errDispatchTimeInvalid)
			return
		}
		expected = expected.UTC()
		before = before.UTC()

		job, err := st.GetJob(c.Param("id"))
		if err != nil {
			writeLookupError(c, err)
			return
		}
		if !job.Enabled || job.NextRun == nil {
			writeJobError(c, errJobNotFound)
			return
		}
		current := job.NextRun.UTC()
		if !current.Equal(expected) {
			writeJobError(c, errNextRunConflict)
			return
		}
		if current.After(before) {
			writeJobError(c, errDispatchNotDue)
			return
		}

		loc, err := time.LoadLocation(job.Timezone)
		if err != nil {
			writeJobError(c, errTimezoneInvalid)
			return
		}
		spec, err := schedule.Parse(job.Expression)
		if err != nil {
			writeJobError(c, errScheduleInvalid)
			return
		}

		occurrences := make([]time.Time, 0)
		candidate := spec.Next(current.Add(-time.Nanosecond), loc).UTC()
		for !candidate.IsZero() && !candidate.After(before) {
			occurrences = append(occurrences, candidate)
			candidate = spec.Next(candidate, loc).UTC()
		}
		next := candidate
		if next.IsZero() {
			writeJobError(c, errScheduleInvalid)
			return
		}

		updatedAt := time.Now().UTC()
		if err := st.AdvanceNextRun(job.ID, current, next, updatedAt); err != nil {
			writeDispatchWriteError(c, err)
			return
		}

		formatted := make([]string, 0, len(occurrences))
		for _, occurrence := range occurrences {
			formatted = append(formatted, formatTime(occurrence))
		}
		c.JSON(http.StatusOK, dispatchResponse{
			JobID:       job.ID,
			Occurrences: formatted,
			NextRun:     formatTime(next),
		})
	}
}

func writeDispatchWriteError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJobError(c, errJobNotFound)
	case errors.Is(err, store.ErrNextRunConflict):
		writeJobError(c, errNextRunConflict)
	default:
		writeJobError(c, errStorageUnavailable)
	}
}
