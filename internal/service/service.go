package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/ladydd/taskgate/internal/logger"
	"github.com/ladydd/taskgate/internal/model"
	"github.com/ladydd/taskgate/internal/queue"
	"github.com/ladydd/taskgate/internal/store"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// TaskProcessor is the core business logic interface.
// Implementations receive the raw task input and return the raw output.
// The framework treats both as opaque JSON — only the processor knows the schema.
type TaskProcessor interface {
	Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

// RequestValidator validates the raw request body before a task is created.
// It returns a list of human-readable error descriptions; an empty slice means valid.
// The context should be used for any I/O operations (e.g. DNS resolution for SSRF checks).
type RequestValidator interface {
	Validate(ctx context.Context, input json.RawMessage) []string
}

// LogSanitizer is an optional hook that strips sensitive fields from the task input
// before it is written to log files. If nil, the input is logged as-is.
type LogSanitizer func(input json.RawMessage) json.RawMessage

// OutputLogSanitizer is an optional hook that strips sensitive fields from the task output
// before it is written to log files. If nil, the output is logged as-is.
type OutputLogSanitizer func(output json.RawMessage) json.RawMessage

// TaskService orchestrates task creation, queuing, worker dispatch, and result retrieval.
type TaskService struct {
	store              store.TaskStore
	fallback           store.FallbackTaskStore
	validator          RequestValidator
	processor          TaskProcessor
	logger             logger.TaskLogger
	queue              queue.TaskQueue
	limiter            *rate.Limiter      // nil means unlimited
	logSanitizer       LogSanitizer       // nil means no sanitization
	outputLogSanitizer OutputLogSanitizer // nil means no sanitization
	taskTimeout        int                // seconds — used to detect stuck tasks
	workerCount        int
	wg                 sync.WaitGroup
	cancel             context.CancelFunc
}

// NewTaskService creates a new TaskService with the given dependencies.
//   - workerCount controls how many goroutines consume from the queue concurrently (parallel flights).
//   - rateLimit controls the maximum number of tasks per second sent to the processor (0 = unlimited).
//   - logSanitizer optionally strips sensitive fields from input before logging (nil = no sanitization).
//
// WorkerCount and RateLimit work together:
//   - WorkerCount caps how many requests are in-flight at the same time (concurrency).
//   - RateLimit caps how many requests are initiated per second (throughput).
func NewTaskService(
	store store.TaskStore,
	fallback store.FallbackTaskStore,
	validator RequestValidator,
	processor TaskProcessor,
	logger logger.TaskLogger,
	q queue.TaskQueue,
	taskTimeout int,
	workerCount int,
	rateLimit int,
	logSanitizer LogSanitizer,
	outputLogSanitizer OutputLogSanitizer,
) *TaskService {
	if workerCount <= 0 {
		workerCount = 1
	}

	var limiter *rate.Limiter
	if rateLimit > 0 {
		limiter = rate.NewLimiter(rate.Limit(rateLimit), rateLimit)
	}

	return &TaskService{
		store:              store,
		fallback:           fallback,
		validator:          validator,
		processor:          processor,
		logger:             logger,
		queue:              q,
		limiter:            limiter,
		logSanitizer:       logSanitizer,
		outputLogSanitizer: outputLogSanitizer,
		taskTimeout:        taskTimeout,
		workerCount:        workerCount,
	}
}

// Start launches the worker pool. Each worker loops on Dequeue → rate limit → processTask → Ack.
// Call Stop to shut down the workers gracefully.
func (s *TaskService) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	for i := 0; i < s.workerCount; i++ {
		s.wg.Add(1)
		go s.worker(ctx, i)
	}

	rateDesc := "unlimited"
	if s.limiter != nil {
		rateDesc = slog.IntValue(int(s.limiter.Limit())).String() + "/s"
	}
	slog.Info("worker pool started", "workers", s.workerCount, "rate_limit", rateDesc)
}

// Stop signals all workers to stop and waits for them to finish.
func (s *TaskService) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	slog.Info("worker pool stopped")
}

// worker is a single consumer goroutine that dequeues and processes tasks.
func (s *TaskService) worker(ctx context.Context, id int) {
	defer s.wg.Done()

	for {
		delivery, err := s.queue.Dequeue(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("worker dequeue error", "worker", id, "error", err)
			time.Sleep(time.Second)
			continue
		}

		// Wait for rate limiter before processing.
		if s.limiter != nil {
			if err := s.limiter.Wait(ctx); err != nil {
				if ctx.Err() != nil {
					// Shutdown — nack with requeue so the message is not lost.
					_ = delivery.Nack(true)
					return
				}
				slog.Error("rate limiter wait error", "worker", id, "error", err)
				_ = delivery.Nack(true)
				continue
			}
		}

		// Process the task. Ack/Nack is handled after processing completes.
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("processTask panicked", "worker", id, "uuid", delivery.Message.TaskID, "panic", r)
					// Nack without requeue — the panic will likely repeat. Goes to DLQ if configured.
					_ = delivery.Nack(false)
				}
			}()
			s.processTask(delivery.Message.TaskID, delivery.Message.Input)
			// Task processed successfully (or failed gracefully) — ack the message.
			if err := delivery.Ack(); err != nil {
				slog.Error("failed to ack message", "worker", id, "uuid", delivery.Message.TaskID, "error", err)
			}
		}()
	}
}

// ValidateRequest validates the raw input and returns validation errors.
func (s *TaskService) ValidateRequest(ctx context.Context, input json.RawMessage) []string {
	return s.validator.Validate(ctx, input)
}

// CreateTask creates a new pending task in the store and returns the task ID.
func (s *TaskService) CreateTask(ctx context.Context, input json.RawMessage) (string, error) {
	taskID := uuid.New().String()
	now := time.Now().Unix()

	task := &model.Task{
		UUID:      taskID,
		Status:    "pending",
		Input:     input,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.CreateTask(ctx, task); err != nil {
		return "", err
	}

	return taskID, nil
}

// EnqueueTask publishes a task to the queue for async processing by the worker pool.
func (s *TaskService) EnqueueTask(ctx context.Context, taskID string, input json.RawMessage) error {
	return s.queue.Enqueue(ctx, queue.TaskMessage{
		TaskID: taskID,
		Input:  input,
	})
}

// MarkTaskFailed updates a task to failed status. Used to clean up when enqueue fails
// after the task has already been created in the store.
func (s *TaskService) MarkTaskFailed(ctx context.Context, taskID string, reason string) {
	task := &model.Task{
		UUID:      taskID,
		Status:    "failed",
		Error:     reason,
		UpdatedAt: time.Now().Unix(),
	}
	if err := s.store.UpdateTask(ctx, task); err != nil {
		slog.Error("failed to mark task as failed", "uuid", taskID, "error", err)
	}
}

// MarkEnqueueUncertain records that the publisher confirm for this task timed out
// or was cancelled. The task stays "pending" but carries a timestamp so that
// GetTaskResult can apply a bounded timeout to this specific subset of pending tasks.
func (s *TaskService) MarkEnqueueUncertain(ctx context.Context, taskID string) {
	now := time.Now().Unix()
	task := &model.Task{
		UUID:               taskID,
		EnqueueUncertainAt: now,
		UpdatedAt:          now,
	}
	if err := s.store.UpdateTask(ctx, task); err != nil {
		slog.Error("failed to mark task as enqueue-uncertain", "uuid", taskID, "error", err)
	}
}

// GetTaskResult retrieves a task by UUID, checking the fallback store when appropriate.
func (s *TaskService) GetTaskResult(ctx context.Context, taskUUID string) (*model.Task, error) {
	task, err := s.store.GetTask(ctx, taskUUID)
	if err != nil {
		if fallbackTask, fallbackErr := s.getFallbackTask(ctx, taskUUID); fallbackErr == nil {
			return fallbackTask, nil
		}
		return nil, err
	}

	if task.Status == "pending" || task.Status == "processing" {
		if fallbackTask, fallbackErr := s.getFallbackTask(ctx, taskUUID); fallbackErr == nil {
			return fallbackTask, nil
		}

		// Stuck detection: only applies to tasks that a worker has started processing.
		// Pending tasks are still queued — they haven't timed out, they're just waiting.
		// Marking a pending task as failed would be wrong under backlog, rate limiting,
		// or low worker counts.
		if task.Status == "processing" && task.StartedAt > 0 {
			stuckThreshold := int64(s.taskTimeout * 2)
			if time.Now().Unix()-task.StartedAt > stuckThreshold {
				task.Status = "failed"
				task.Error = "task processing timed out, possibly due to an internal state update failure; please resubmit"
				task.UpdatedAt = time.Now().Unix()

				if err := s.store.UpdateTask(ctx, task); err != nil {
					slog.Error("failed to persist stuck task status", "uuid", task.UUID, "error", err)
				}
			}
		}

		// Uncertain-enqueue timeout: only applies to pending tasks where the publisher
		// confirm was inconclusive. If the broker never actually accepted the message,
		// the task would sit in pending forever. We give it a generous timeout (3× task
		// timeout) before marking it failed with a distinct error message.
		if task.Status == "pending" && task.EnqueueUncertainAt > 0 {
			uncertainThreshold := int64(s.taskTimeout * 3)
			if time.Now().Unix()-task.EnqueueUncertainAt > uncertainThreshold {
				task.Status = "failed"
				task.Error = "enqueue confirmation timed out and task was never picked up for processing; please resubmit"
				task.UpdatedAt = time.Now().Unix()

				if err := s.store.UpdateTask(ctx, task); err != nil {
					slog.Error("failed to persist uncertain-enqueue timeout", "uuid", task.UUID, "error", err)
				}
			}
		}
	}

	return task, nil
}

func (s *TaskService) getFallbackTask(ctx context.Context, taskID string) (*model.Task, error) {
	if s.fallback == nil {
		return nil, store.ErrFallbackTaskNotFound
	}
	return s.fallback.GetTask(ctx, taskID)
}

// contextKey is an unexported type for context keys to avoid collisions.
type contextKey string

// ContextKeyRequestID is the context key for the task/request ID.
const ContextKeyRequestID contextKey = "request_id"

// sanitizeForLog applies the log sanitizer to input if configured.
func (s *TaskService) sanitizeForLog(input json.RawMessage) string {
	if s.logSanitizer != nil {
		return string(s.logSanitizer(input))
	}
	return string(input)
}

// sanitizeOutputForLog applies the output log sanitizer if configured.
func (s *TaskService) sanitizeOutputForLog(output json.RawMessage) string {
	if s.outputLogSanitizer != nil {
		return string(s.outputLogSanitizer(output))
	}
	return string(output)
}

// processTask calls the TaskProcessor and updates the task state.
func (s *TaskService) processTask(taskID string, input json.RawMessage) {
	// Base context for store operations — no timeout, so retries aren't killed by the process deadline.
	baseCtx := context.WithValue(context.Background(), ContextKeyRequestID, taskID)
	baseCtx = logger.WithLogger(baseCtx, s.logger)
	baseCtx = context.WithValue(baseCtx, logger.RequestIDCtxKey(), taskID)

	// Mark task as "processing" with a start timestamp before calling Process().
	// This lets stuck-task detection measure from actual processing start, not creation time.
	// Also clears EnqueueUncertainAt — the message was delivered, uncertainty is resolved.
	now := time.Now().Unix()
	s.updateTaskBestEffort(baseCtx, taskID, &model.Task{
		UUID:                  taskID,
		Status:                "processing",
		StartedAt:             now,
		UpdatedAt:             now,
		ClearEnqueueUncertain: true,
	})

	s.logger.LogStep(baseCtx, taskID, "request", map[string]interface{}{
		"input": s.sanitizeForLog(input),
	})

	// Process context — has a deadline so the processor can't block forever.
	processCtx, cancel := context.WithTimeout(baseCtx, time.Duration(s.taskTimeout)*time.Second)
	defer cancel()

	output, err := s.processor.Process(processCtx, input)
	if err != nil {
		s.logger.LogError(baseCtx, taskID, "process_error", err)

		// Use baseCtx (no deadline) for the store write so retries aren't killed
		// by the already-expired process deadline.
		s.updateTaskWithRetry(baseCtx, taskID, &model.Task{
			UUID:      taskID,
			Status:    "failed",
			Error:     err.Error(),
			UpdatedAt: time.Now().Unix(),
		})
		return
	}

	s.logger.LogStep(baseCtx, taskID, "result", map[string]interface{}{
		"output": s.sanitizeOutputForLog(output),
	})

	s.updateTaskWithRetry(baseCtx, taskID, &model.Task{
		UUID:      taskID,
		Status:    "completed",
		Output:    output,
		UpdatedAt: time.Now().Unix(),
	})
}

// updateTaskBestEffort attempts a single store update without retries.
// Used for non-critical state transitions (e.g. pending → processing).
func (s *TaskService) updateTaskBestEffort(ctx context.Context, taskID string, task *model.Task) {
	if err := s.store.UpdateTask(ctx, task); err != nil {
		slog.Warn("best-effort task update failed", "uuid", taskID, "target_status", task.Status, "error", err)
	}
}

// updateTaskWithRetry attempts to update a task in the store, retrying up to 3 times.
func (s *TaskService) updateTaskWithRetry(ctx context.Context, taskID string, task *model.Task) {
	const maxRetries = 3
	var err error
	for i := 0; i < maxRetries; i++ {
		if err = s.store.UpdateTask(ctx, task); err == nil {
			return
		}
		slog.Error("failed to update task in store, retrying",
			"uuid", taskID,
			"attempt", i+1,
			"error", err,
		)
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}

	slog.Error("failed to update task after all retries, task stuck in pending",
		"uuid", taskID,
		"target_status", task.Status,
		"error", err,
	)
	s.logger.LogError(ctx, taskID, "store_update_failed", err)

	if s.fallback == nil {
		return
	}

	if fallbackErr := s.fallback.SaveTask(ctx, task); fallbackErr != nil {
		slog.Error("failed to persist final task state to fallback store",
			"uuid", taskID,
			"target_status", task.Status,
			"error", fallbackErr,
		)
		return
	}

	slog.Warn("persisted final task state to fallback store after Redis update failure",
		"uuid", taskID,
		"target_status", task.Status,
	)
}
