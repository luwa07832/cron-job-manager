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
	ExpectedNextRun string `json:"expected_next_run"`
	Before          string `json:"before"`
}

type dispatchResponse struct {
	JobID       string   `json:"job_id"`
	Occurrences []string `json:"occurrences"`
	NextRun     *string  `json:"next_run"`
}

func registerDispatches(router *gin.Engine, st *store.Store) {
	router.POST("/api/v1/jobs/:id/dispatches", createDispatch(st))
}

// createDispatch claims every due firing instant up to before and advances
// the scheduling cursor in one atomic step. It only manages schedule state:
// no runs or attempts are recorded and no external command is started.
func createDispatch(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := st.GetJob(c.Param("id"))
		if err != nil {
			writeLookupError(c, err)
			return
		}
		if !job.Enabled {
			writeJobError(c, errJobNotFound)
			return
		}

		var request dispatchRequest
		if !decodeJobBody(c, &request) {
			return
		}
		expected, err := time.Parse(time.RFC3339, request.ExpectedNextRun)
		if err != nil {
			writeJobError(c, errDispatchTimeInvalid)
			return
		}
		before, err := time.Parse(time.RFC3339, request.Before)
		if err != nil {
			writeJobError(c, errDispatchTimeInvalid)
			return
		}
		expected = expected.UTC()
		before = before.UTC()

		if job.NextRun == nil || !expected.Equal(*job.NextRun) {
			writeJobError(c, errNextRunConflict)
			return
		}
		if job.NextRun.After(before) {
			writeJobError(c, errDispatchNotDue)
			return
		}

		// Stored expressions and time zones were validated when the job was
		// written, so a failure here means the stored state is unreadable.
		loc, err := time.LoadLocation(job.Timezone)
		if err != nil {
			writeJobError(c, errStorageUnavailable)
			return
		}
		spec, err := schedule.Parse(job.Expression)
		if err != nil {
			writeJobError(c, errStorageUnavailable)
			return
		}

		occurrences, following := spec.Through(*job.NextRun, before, loc)
		var nextRun *time.Time
		if !following.IsZero() {
			following = following.UTC()
			nextRun = &following
		}

		if err := st.AdvanceNextRun(job.ID, *job.NextRun, nextRun, time.Now().UTC()); err != nil {
			writeDispatchAdvanceError(c, err)
			return
		}

		instants := make([]string, 0, len(occurrences))
		for _, instant := range occurrences {
			instants = append(instants, formatTime(instant))
		}
		response := dispatchResponse{
			JobID:       job.ID,
			Occurrences: instants,
		}
		if nextRun != nil {
			formatted := formatTime(*nextRun)
			response.NextRun = &formatted
		}
		c.JSON(http.StatusOK, response)
	}
}

func writeDispatchAdvanceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJobError(c, errJobNotFound)
	case errors.Is(err, store.ErrNextRunConflict):
		writeJobError(c, errNextRunConflict)
	default:
		writeJobError(c, errStorageUnavailable)
	}
}
