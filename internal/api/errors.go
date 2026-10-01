package api

import (
	"errors"
	"net/http"

	"github.com/luwa07832/cron-job-manager/internal/store"
)

// Public error codes. The README documents one code per failure condition;
// codes are stable identifiers callers can branch on.
const (
	codeInvalidJSON          = "invalid_request_body"
	codeInvalidName          = "invalid_job_name"
	codeInvalidExpression    = "invalid_cron_expression"
	codeInvalidResult        = "invalid_run_result"
	codeInvalidFailureReason = "invalid_failure_reason"
	codeInvalidTime          = "invalid_time"
	codeInvalidWindow        = "invalid_time_window"
	codeWindowTooLarge       = "query_range_too_large"
	codeJobNotFound          = "job_not_found"
	codeRunNotFound          = "run_not_found"
	codeRetryNotRetryable    = "retry_not_allowed"
	codeInternal             = "internal_error"
)

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.code + ": " + e.message }

func badRequest(code, message string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: code, message: message}
}

func notFound(code, message string) *apiError {
	return &apiError{status: http.StatusNotFound, code: code, message: message}
}

// mapStoreError converts the single errors shared between store and API into
// their published codes. Unknown errors become a generic 500 so SQL details
// never leave the service.
func mapStoreError(err error) *apiError {
	switch {
	case errors.Is(err, store.ErrJobNotFound):
		return notFound(codeJobNotFound, "the requested job does not exist")
	case errors.Is(err, store.ErrRunNotFound):
		return notFound(codeRunNotFound, "the requested run record does not exist")
	case errors.Is(err, store.ErrRetryNotRetryable):
		return &apiError{
			status:  http.StatusConflict,
			code:    codeRetryNotRetryable,
			message: "only failed original executions can be retried",
		}
	default:
		return &apiError{status: http.StatusInternalServerError, code: codeInternal, message: "the request failed unexpectedly"}
	}
}
