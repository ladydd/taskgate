package model

import "encoding/json"

// Task represents an asynchronous task stored in the primary or fallback store.
// Input and Output are opaque JSON payloads — the framework does not interpret them;
// the concrete TaskProcessor and RequestValidator give them meaning.
type Task struct {
	UUID      string          `json:"uuid"`
	Status    string          `json:"status"` // "pending" | "processing" | "completed" | "failed"
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
	CreatedAt int64           `json:"created_at"`
	StartedAt int64           `json:"started_at,omitempty"` // set when a worker begins processing
	UpdatedAt int64           `json:"updated_at"`

	// EnqueueUncertainAt is set when a publisher confirm timed out or was cancelled,
	// meaning the message may or may not have been accepted by the broker.
	// If non-zero and the task is still pending after a threshold, GetTaskResult
	// will mark it as failed. Cleared to 0 when a worker starts processing.
	EnqueueUncertainAt int64 `json:"enqueue_uncertain_at,omitempty"`

	// ClearEnqueueUncertain is a transient flag (not persisted) that tells the
	// store to explicitly set EnqueueUncertainAt to 0 during a merge update.
	// Without this, the merge logic can't distinguish "caller didn't touch this field"
	// from "caller wants to clear it".
	ClearEnqueueUncertain bool `json:"-"`
}

// ErrorResponse is the unified error response format returned by the HTTP handler.
type ErrorResponse struct {
	Error   string   `json:"error"`
	Details []string `json:"details"`
}
