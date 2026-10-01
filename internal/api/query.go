package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func parseSeq(raw string) (int64, *apiError) {
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq <= 0 {
		return 0, notFound(codeRunNotFound, "the requested run record does not exist")
	}
	return seq, nil
}

// queryWindow serves GET /api/v1/run-records?start=...&end=... with a closed
// interval filtered on the planned trigger time. The bounds are accepted with
// any zone offset but compared as absolute instants, and echoed back in UTC so
// callers observe one timezone-independent coordinate system.
func (h *Handlers) queryWindow(c *gin.Context) {
	start, apiErr := parseWindowBound(c, "start", true)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	end, apiErr := parseWindowBound(c, "end", true)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	if start.After(end) {
		writeError(c, &apiError{
			status:  http.StatusBadRequest,
			code:    codeInvalidWindow,
			message: "window start must not be later than window end",
		})
		return
	}
	if end.Sub(start) > MaxWindowSpan {
		writeError(c, &apiError{
			status:  http.StatusBadRequest,
			code:    codeWindowTooLarge,
			message: "query window must not span more than 366 days",
		})
		return
	}

	result, err := h.store.WindowInclusive(start, end)
	if err != nil {
		writeError(c, mapStoreError(err))
		return
	}

	pending := make([]pendingResponse, 0, len(result.Pending))
	for _, item := range result.Pending {
		pending = append(pending, pendingResponse{
			JobID:          item.JobID,
			JobName:        item.JobName,
			CronExpression: item.Expression,
			RecordType:     "pending",
			ScheduledFor:   timeJSON(item.ScheduledFor),
		})
	}
	executed := make([]runResponse, 0, len(result.Executed))
	for _, record := range result.Executed {
		executed = append(executed, toRunResponse(record))
	}
	c.JSON(http.StatusOK, windowResponse{
		WindowStart: timeJSON(start),
		WindowEnd:   timeJSON(end),
		Pending:     pending,
		Executed:    executed,
	})
}

func parseWindowBound(c *gin.Context, key string, required bool) (time.Time, *apiError) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		if required {
			return time.Time{}, badRequest(codeInvalidWindow, "query parameters start and end are required RFC3339 timestamps")
		}
		return time.Time{}, nil
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, badRequest(codeInvalidTime, key+" must be an RFC3339 timestamp")
	}
	return value.UTC(), nil
}
