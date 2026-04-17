package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ladydd/taskgate/internal/model"

	"github.com/redis/go-redis/v9"
)

// ErrTaskNotFound is returned when a task with the given UUID does not exist in the store.
var ErrTaskNotFound = errors.New("task not found")

// taskTTL is the time-to-live for task entries in Redis.
const taskTTL = 7 * 24 * time.Hour

// InputSanitizer is an optional hook that strips sensitive fields from the task input
// before it is persisted. If nil, the input is stored as-is.
type InputSanitizer func(input json.RawMessage) json.RawMessage

// TaskStore defines the interface for task persistence operations.
type TaskStore interface {
	CreateTask(ctx context.Context, task *model.Task) error
	GetTask(ctx context.Context, uuid string) (*model.Task, error)
	UpdateTask(ctx context.Context, task *model.Task) error
}

// RedisStore implements TaskStore using a Redis backend.
type RedisStore struct {
	client    *redis.Client
	sanitizer InputSanitizer
}

// NewRedisStore creates a new RedisStore with the given go-redis client.
// The optional sanitizer is called before persisting task input to strip sensitive fields.
func NewRedisStore(client *redis.Client, sanitizer InputSanitizer) *RedisStore {
	return &RedisStore{client: client, sanitizer: sanitizer}
}

// taskKey returns the Redis key for a given task UUID.
func taskKey(uuid string) string {
	return fmt.Sprintf("task:%s", uuid)
}

// CreateTask stores a new Task in Redis.
// If a sanitizer is configured, the input is sanitized before serialization.
func (s *RedisStore) CreateTask(ctx context.Context, task *model.Task) error {
	stored := *task
	if s.sanitizer != nil {
		stored.Input = s.sanitizer(stored.Input)
	}

	data, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("failed to marshal task: %w", err)
	}

	return s.client.Set(ctx, taskKey(task.UUID), data, taskTTL).Err()
}

// GetTask retrieves a Task by UUID.
// Returns ErrTaskNotFound if the key does not exist.
func (s *RedisStore) GetTask(ctx context.Context, uuid string) (*model.Task, error) {
	data, err := s.client.Get(ctx, taskKey(uuid)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("failed to get task: %w", err)
	}

	var task model.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("failed to unmarshal task: %w", err)
	}

	return &task, nil
}

// UpdateTask merges the provided fields into the existing Task in Redis and resets the TTL.
// This avoids overwriting fields (e.g. Input, CreatedAt) that are not set on the update payload.
func (s *RedisStore) UpdateTask(ctx context.Context, task *model.Task) error {
	key := taskKey(task.UUID)

	// Read the existing task so we can merge rather than blindly overwrite.
	existing, err := s.GetTask(ctx, task.UUID)
	if err != nil {
		// If the task doesn't exist (e.g. TTL expired), fall back to a plain write.
		if errors.Is(err, ErrTaskNotFound) {
			data, marshalErr := json.Marshal(task)
			if marshalErr != nil {
				return fmt.Errorf("failed to marshal task: %w", marshalErr)
			}
			return s.client.Set(ctx, key, data, taskTTL).Err()
		}
		return fmt.Errorf("failed to read existing task for merge: %w", err)
	}

	// Merge: only overwrite fields that the caller explicitly set.
	if task.Status != "" {
		existing.Status = task.Status
	}
	if len(task.Output) > 0 {
		existing.Output = task.Output
	}
	if task.Error != "" {
		existing.Error = task.Error
	}
	if task.StartedAt != 0 {
		existing.StartedAt = task.StartedAt
	}
	if task.UpdatedAt != 0 {
		existing.UpdatedAt = task.UpdatedAt
	}
	// EnqueueUncertainAt is always merged: it can be set (>0) or explicitly
	// cleared (0) when a worker starts processing. We use ClearEnqueueUncertain
	// as a flag to distinguish "caller wants to clear" from "caller didn't set it".
	if task.EnqueueUncertainAt != 0 || task.ClearEnqueueUncertain {
		existing.EnqueueUncertainAt = task.EnqueueUncertainAt
	}

	data, err := json.Marshal(existing)
	if err != nil {
		return fmt.Errorf("failed to marshal task: %w", err)
	}

	return s.client.Set(ctx, key, data, taskTTL).Err()
}
