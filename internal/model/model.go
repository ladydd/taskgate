package model

import "encoding/json"

// Task represents an asynchronous task stored in the primary or fallback store.
// Input and Output are opaque JSON payloads — the framework does not interpret them;
// the concrete TaskProcessor and RequestValidator give them meaning.
type Task struct {
	UUID      string          `json:"uuid"`
	Status    string          `json:"status"` // "pending" | "completed" | "failed"
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
}

// ErrorResponse is the unified error response format returned by the HTTP handler.
type ErrorResponse struct {
	Error   string   `json:"error"`
	Details []string `json:"details"`
}
