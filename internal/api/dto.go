package api

import (
	"database/sql"
	"time"

	"github.com/luwa07832/cron-job-manager/internal/store"
)

// All wire times are rendered in UTC with an explicit offset, so a service
// relocated to another timezone keeps producing identical responses.
func timeJSON(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func nullTimeJSON(t sql.NullTime) *string {
	if !t.Valid {
		return nil
	}
	value := timeJSON(t.Time)
	return &value
}

type jobResponse struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	CronExpression   string `json:"cron_expression"`
	NextScheduledFor string `json:"next_scheduled_for"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type runResponse struct {
	Seq              int64   `json:"seq"`
	JobID            string  `json:"job_id"`
	JobName          string  `json:"job_name"`
	CronExpression   string  `json:"cron_expression"`
	RecordType       string  `json:"record_type"`
	TriggerType      string  `json:"trigger_type"`
	RetryOfSeq       *int64  `json:"retry_of_seq,omitempty"`
	RetryNumber      int     `json:"retry_number"`
	RetryCount       int     `json:"retry_count"`
	ScheduledFor     string  `json:"scheduled_for"`
	TriggeredAt      string  `json:"triggered_at"`
	StartedAt        string  `json:"started_at"`
	FinishedAt       string  `json:"finished_at"`
	Result           string  `json:"result"`
	FailureReason    *string `json:"failure_reason"`
	NextScheduledFor *string `json:"next_scheduled_for"`
}

type pendingResponse struct {
	JobID          string `json:"job_id"`
	JobName        string `json:"job_name"`
	CronExpression string `json:"cron_expression"`
	RecordType     string `json:"record_type"`
	ScheduledFor   string `json:"scheduled_for"`
}

type windowResponse struct {
	WindowStart string            `json:"window_start"`
	WindowEnd   string            `json:"window_end"`
	Pending     []pendingResponse `json:"pending"`
	Executed    []runResponse     `json:"executed"`
}

func toRunResponse(r store.RunRecord) runResponse {
	resp := runResponse{
		Seq:              r.Seq,
		JobID:            r.JobID,
		JobName:          r.JobName,
		CronExpression:   r.Expression,
		RecordType:       r.RecordType,
		TriggerType:      r.TriggerType,
		RetryNumber:      r.RetryNumber,
		RetryCount:       r.RetryCount,
		ScheduledFor:     timeJSON(r.ScheduledFor),
		TriggeredAt:      timeJSON(r.TriggeredAt),
		StartedAt:        timeJSON(r.StartedAt),
		FinishedAt:       timeJSON(r.FinishedAt),
		Result:           r.Result,
		NextScheduledFor: nullTimeJSON(r.NextScheduledFor),
	}
	if r.RetryOfSeq.Valid {
		seq := r.RetryOfSeq.Int64
		resp.RetryOfSeq = &seq
	}
	if r.FailureReason.Valid {
		reason := r.FailureReason.String
		resp.FailureReason = &reason
	}
	return resp
}
