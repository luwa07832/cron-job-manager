package service

import (
	"time"

	"github.com/luwa07832/cron-job-manager/internal/cron"
)

// QueryWindow returns the pending and executed records inside the closed
// interval [from, to], filtered by planned fire time.
func (s *Service) QueryWindow(from, to time.Time) (*WindowResult, error) {
	from = from.UTC()
	to = to.UTC()
	if from.After(to) {
		return nil, ErrInvalidWindow
	}
	if to.Sub(from) > MaxQuerySpan {
		return nil, ErrWindowTooLarge
	}

	result := &WindowResult{
		WindowStart: from,
		WindowEnd:   to,
		Pending:     []PendingRecord{},
		Executed:    []RunRecord{},
	}

	runs, err := s.store.RunsInWindow(from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		result.Executed = append(result.Executed, toRunRecord(run))
	}

	pending, err := s.pendingSlots(from, to)
	if err != nil {
		return nil, err
	}
	result.Pending = pending
	return result, nil
}

// pendingSlots enumerates cron matches for every task in [from, to] and keeps
// those with no original scheduled execution yet. A slot already executed as a
// retry is still considered executed through its chain root; only a concrete
// original scheduled record marks the slot complete.
func (s *Service) pendingSlots(from, to time.Time) ([]PendingRecord, error) {
	jobs, err := s.store.ListJobs()
	if err != nil {
		return nil, err
	}
	pending := []PendingRecord{}
	for _, job := range jobs {
		parsed, err := cron.Parse(job.Schedule)
		if err != nil {
			continue
		}
		occupied, err := s.store.OriginalScheduledSlots(job.ID, from.UnixNano(), to.UnixNano())
		if err != nil {
			return nil, err
		}
		for _, slot := range parsed.Between(from, to) {
			if _, done := occupied[slot.UnixNano()]; done {
				continue
			}
			next, _ := parsed.Next(slot)
			pending = append(pending, PendingRecord{
				RecordType:      RecordPending,
				Attempt:         1,
				TaskID:          job.ID,
				TaskName:        job.Name,
				Schedule:        job.Schedule,
				TriggerType:     TriggerScheduled,
				PlannedFireTime: slot,
				NextFireTime:    next,
				Status:          "pending",
			})
		}
	}
	return pending, nil
}
