package apperror

import "fmt"

// Code represents a machine-readable error code.
type Code string

// Framework-level error codes. Business-specific codes should be defined
// by the concrete TaskProcessor / RequestValidator implementations.
const (
	CodeValidation  Code = "validation_error"
	CodeNotFound    Code = "not_found"
	CodeInternal    Code = "internal_error"
	CodeInvalidJSON Code = "invalid_json"
	CodeTimeout     Code = "timeout"
	CodeStoreError  Code = "store_error"
	CodeStuckTask   Code = "stuck_task"
)

// Error is the unified application error type.
// It carries a machine-readable Code, a human-readable Message,
// and an optional underlying Cause for unwrapping.
type Error struct {
	Code    Code
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

// New creates a new Error without a cause.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap creates a new Error wrapping an underlying cause.
func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}
