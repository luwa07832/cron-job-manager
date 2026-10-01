package service

import (
	"fmt"
	"strings"
)

// BuiltinExecutor runs the task actions shipped with the service:
//
//   - "succeed" always succeeds.
//   - "fail" fails with a generic reason.
//   - "fail:<reason>" fails with the supplied non-empty reason.
type BuiltinExecutor struct{}

// NewBuiltinExecutor returns the default action runner.
func NewBuiltinExecutor() BuiltinExecutor { return BuiltinExecutor{} }

// Supports reports whether action is a recognised built-in action spec.
func (BuiltinExecutor) Supports(action string) bool {
	action = strings.TrimSpace(action)
	if action == "succeed" || action == "fail" {
		return true
	}
	if strings.HasPrefix(action, "fail:") {
		return strings.TrimSpace(strings.TrimPrefix(action, "fail:")) != ""
	}
	return false
}

// Execute implements Executor.
func (BuiltinExecutor) Execute(_, action string) error {
	switch {
	case action == "succeed":
		return nil
	case action == "fail":
		return fmt.Errorf("task action failed")
	case strings.HasPrefix(action, "fail:"):
		reason := strings.TrimSpace(strings.TrimPrefix(action, "fail:"))
		return fmt.Errorf("%s", reason)
	default:
		return fmt.Errorf("unsupported task action %q", action)
	}
}

// failureText extracts a non-empty failure reason from an execution error.
func failureText(err error) string {
	reason := strings.TrimSpace(err.Error())
	if reason == "" {
		return "task execution failed"
	}
	return reason
}
