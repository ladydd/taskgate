package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ladydd/taskgate/internal/model"
)

func TestFileFallbackTaskStoreRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewFileFallbackTaskStore(filepath.Join(dir, "fallback"), nil)
	if err != nil {
		t.Fatalf("NewFileFallbackTaskStore() error = %v", err)
	}

	task := &model.Task{
		UUID:      "task-1",
		Status:    "completed",
		Input:     json.RawMessage(`{"key":"value"}`),
		Output:    json.RawMessage(`{"result":"done"}`),
		CreatedAt: 1,
		UpdatedAt: 2,
	}

	if err := store.SaveTask(context.Background(), task); err != nil {
		t.Fatalf("SaveTask() error = %v", err)
	}

	got, err := store.GetTask(context.Background(), task.UUID)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}

	if got.Status != task.Status {
		t.Fatalf("expected status %q, got %q", task.Status, got.Status)
	}
	if string(got.Output) != string(task.Output) {
		t.Fatalf("expected output to round-trip, got %q", string(got.Output))
	}
}

func TestFileFallbackTaskStoreWithSanitizer(t *testing.T) {
	t.Parallel()

	sanitizer := func(input json.RawMessage) json.RawMessage {
		var m map[string]interface{}
		if err := json.Unmarshal(input, &m); err != nil {
			return input
		}
		m["secret"] = "***"
		out, _ := json.Marshal(m)
		return out
	}

	dir := t.TempDir()
	store, err := NewFileFallbackTaskStore(dir, sanitizer)
	if err != nil {
		t.Fatalf("NewFileFallbackTaskStore() error = %v", err)
	}

	task := &model.Task{
		UUID:   "task-sanitize",
		Status: "completed",
		Input:  json.RawMessage(`{"secret":"my-api-key","data":"hello"}`),
	}

	if err := store.SaveTask(context.Background(), task); err != nil {
		t.Fatalf("SaveTask() error = %v", err)
	}

	got, err := store.GetTask(context.Background(), task.UUID)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(got.Input, &m); err != nil {
		t.Fatalf("failed to unmarshal stored input: %v", err)
	}
	if m["secret"] != "***" {
		t.Fatalf("expected secret to be sanitized, got %q", m["secret"])
	}
	if m["data"] != "hello" {
		t.Fatalf("expected data to be preserved, got %q", m["data"])
	}
}

func TestFileFallbackTaskStoreReturnsNotFound(t *testing.T) {
	t.Parallel()

	store, err := NewFileFallbackTaskStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewFileFallbackTaskStore() error = %v", err)
	}

	_, err = store.GetTask(context.Background(), "missing")
	if !errors.Is(err, ErrFallbackTaskNotFound) {
		t.Fatalf("expected ErrFallbackTaskNotFound, got %v", err)
	}
}
