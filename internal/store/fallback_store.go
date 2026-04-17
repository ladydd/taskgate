package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/ladydd/taskgate/internal/model"
)

// ErrFallbackTaskNotFound is returned when a task is not present in the fallback store.
var ErrFallbackTaskNotFound = errors.New("fallback task not found")

// uuidPattern validates that a string looks like a UUID (hex + hyphens only).
// This prevents path traversal attacks via crafted "uuid" values.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// FallbackTaskStore stores completed or failed tasks outside Redis so they remain
// retrievable if the primary store update fails.
type FallbackTaskStore interface {
	SaveTask(ctx context.Context, task *model.Task) error
	GetTask(ctx context.Context, uuid string) (*model.Task, error)
}

// FileFallbackTaskStore persists final task states as JSON files.
type FileFallbackTaskStore struct {
	dir       string
	sanitizer InputSanitizer
}

// NewFileFallbackTaskStore creates a file-backed fallback store rooted at dir.
// The optional sanitizer strips sensitive fields from the task input before writing to disk.
func NewFileFallbackTaskStore(dir string, sanitizer InputSanitizer) (*FileFallbackTaskStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create fallback task directory %s: %w", dir, err)
	}
	return &FileFallbackTaskStore{dir: dir, sanitizer: sanitizer}, nil
}

// SaveTask writes a final task state atomically to disk.
func (s *FileFallbackTaskStore) SaveTask(ctx context.Context, task *model.Task) error {
	stored := *task
	if s.sanitizer != nil {
		stored.Input = s.sanitizer(stored.Input)
	}

	data, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("failed to marshal fallback task: %w", err)
	}

	path, err := s.taskPath(task.UUID)
	if err != nil {
		return err
	}
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write fallback task temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("failed to finalize fallback task file: %w", err)
	}
	return nil
}

// GetTask reads a final task state from disk.
func (s *FileFallbackTaskStore) GetTask(ctx context.Context, uuid string) (*model.Task, error) {
	path, err := s.taskPath(uuid)
	if err != nil {
		return nil, ErrFallbackTaskNotFound
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrFallbackTaskNotFound
		}
		return nil, fmt.Errorf("failed to read fallback task: %w", err)
	}

	var task model.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("failed to unmarshal fallback task: %w", err)
	}

	return &task, nil
}

func (s *FileFallbackTaskStore) taskPath(uuid string) (string, error) {
	if !uuidPattern.MatchString(uuid) {
		return "", fmt.Errorf("invalid UUID format: %q", uuid)
	}
	return filepath.Join(s.dir, uuid+".json"), nil
}
