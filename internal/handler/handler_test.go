package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ladydd/taskgate/internal/model"
	"github.com/ladydd/taskgate/internal/queue"
	"github.com/ladydd/taskgate/internal/service"
	"github.com/ladydd/taskgate/internal/store"

	"github.com/gin-gonic/gin"
)

// TestMain sets gin to test mode once, avoiding data races from parallel tests
// each calling gin.SetMode on the global variable.
func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// --- stubs ---

type stubTaskStore struct {
	getTask    *model.Task
	getTaskErr error
}

func (s stubTaskStore) CreateTask(ctx context.Context, task *model.Task) error { return nil }

func (s stubTaskStore) GetTask(ctx context.Context, uuid string) (*model.Task, error) {
	if s.getTaskErr != nil {
		return nil, s.getTaskErr
	}
	return s.getTask, nil
}

func (s stubTaskStore) UpdateTask(ctx context.Context, task *model.Task) error { return nil }

type stubFallbackTaskStore struct {
	task *model.Task
	err  error
}

func (s stubFallbackTaskStore) SaveTask(ctx context.Context, task *model.Task) error { return nil }

func (s stubFallbackTaskStore) GetTask(ctx context.Context, uuid string) (*model.Task, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.task, nil
}

type stubProcessor struct{}

func (stubProcessor) Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("not implemented")
}

type stubLogger struct{}

func (stubLogger) LogStep(ctx context.Context, uuid string, step string, data map[string]interface{}) error {
	return nil
}

func (stubLogger) LogError(ctx context.Context, uuid string, errType string, err error) error {
	return nil
}

type stubValidator struct{}

func (stubValidator) Validate(_ context.Context, input json.RawMessage) []string { return nil }

// --- helper ---

func newTestHandler(
	taskStore store.TaskStore,
	fallback store.FallbackTaskStore,
	taskTimeout int,
) *Handler {
	q := queue.NewStubQueue(100)
	svc := service.NewTaskService(taskStore, fallback, stubValidator{}, stubProcessor{}, stubLogger{}, q, taskTimeout, 1, 0, nil, nil)
	// Don't start workers in tests — we only test the HTTP/result retrieval layer.
	return NewHandler(svc)
}

// --- tests ---

// Test UUIDs in valid format.
const (
	testUUID1 = "00000000-0000-0000-0000-000000000001"
	testUUID2 = "00000000-0000-0000-0000-000000000002"
	testUUID3 = "00000000-0000-0000-0000-000000000003"
	testUUID4 = "00000000-0000-0000-0000-000000000004"
	testUUID5 = "00000000-0000-0000-0000-000000000005"
	testUUID6 = "00000000-0000-0000-0000-000000000006"
	testUUID7 = "00000000-0000-0000-0000-000000000007"
)

func TestGetResultReturnsFallbackCompletedTask(t *testing.T) {
	t.Parallel()

	h := newTestHandler(
		stubTaskStore{
			getTask: &model.Task{
				UUID:      testUUID1,
				Status:    "pending",
				CreatedAt: time.Now().Add(-10 * time.Minute).Unix(),
			},
		},
		stubFallbackTaskStore{
			task: &model.Task{
				UUID:   testUUID1,
				Status: "completed",
				Output: json.RawMessage(`{"script":"test output"}`),
			},
		},
		120,
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/result/"+testUUID1, nil)
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "uuid", Value: testUUID1}}

	h.GetResult(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"completed"`) || !strings.Contains(body, `"output"`) {
		t.Fatalf("unexpected response body: %s", body)
	}
}

func TestGetResultReturnsFallbackWhenRedisLookupFails(t *testing.T) {
	t.Parallel()

	h := newTestHandler(
		stubTaskStore{getTaskErr: errors.New("redis unavailable")},
		stubFallbackTaskStore{
			task: &model.Task{
				UUID:   testUUID2,
				Status: "failed",
				Error:  "fallback error",
			},
		},
		120,
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/result/"+testUUID2, nil)
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "uuid", Value: testUUID2}}

	h.GetResult(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"failed"`) || !strings.Contains(body, `"error":"fallback error"`) {
		t.Fatalf("unexpected response body: %s", body)
	}
}

func TestGetResultReturnsTimeoutFailureWhenNoFallbackExists(t *testing.T) {
	t.Parallel()

	// A pending task (not yet picked up by a worker) should remain pending
	// even if it has been queued for a long time. Stuck detection only applies
	// to "processing" tasks.
	h := newTestHandler(
		stubTaskStore{
			getTask: &model.Task{
				UUID:      testUUID3,
				Status:    "pending",
				CreatedAt: time.Now().Add(-10 * time.Minute).Unix(),
			},
		},
		stubFallbackTaskStore{err: store.ErrFallbackTaskNotFound},
		60,
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/result/"+testUUID3, nil)
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "uuid", Value: testUUID3}}

	h.GetResult(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("expected pending task to stay pending, got: %s", body)
	}
}

// --- Regression tests for P1, P2-a, P2-b fixes ---

// Regression: pending task beyond threshold stays pending (not marked failed).
// Stuck detection must only apply to "processing" tasks.
func TestGetResultPendingTaskBeyondThresholdStaysPending(t *testing.T) {
	t.Parallel()

	h := newTestHandler(
		stubTaskStore{
			getTask: &model.Task{
				UUID:      testUUID4,
				Status:    "pending",
				CreatedAt: time.Now().Add(-1 * time.Hour).Unix(), // way beyond any threshold
			},
		},
		stubFallbackTaskStore{err: store.ErrFallbackTaskNotFound},
		30, // 30s timeout → stuck threshold = 60s, but task is pending so irrelevant
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/result/"+testUUID4, nil)
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "uuid", Value: testUUID4}}

	h.GetResult(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("expected pending task to remain pending, got: %s", body)
	}
}

// Regression: processing task with StartedAt beyond threshold is marked failed.
func TestGetResultProcessingTaskBeyondThresholdMarkedFailed(t *testing.T) {
	t.Parallel()

	taskTimeout := 30 // seconds
	stuckThreshold := taskTimeout * 2

	h := newTestHandler(
		stubTaskStore{
			getTask: &model.Task{
				UUID:      testUUID5,
				Status:    "processing",
				CreatedAt: time.Now().Add(-1 * time.Hour).Unix(),
				StartedAt: time.Now().Add(-time.Duration(stuckThreshold+10) * time.Second).Unix(),
			},
		},
		stubFallbackTaskStore{err: store.ErrFallbackTaskNotFound},
		taskTimeout,
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/result/"+testUUID5, nil)
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "uuid", Value: testUUID5}}

	h.GetResult(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("expected stuck processing task to be marked failed, got: %s", body)
	}
}

// Regression: enqueue with cancelled context returns 202 (uncertain), not 500.
func TestSubmitEnqueueUncertainReturns202(t *testing.T) {
	t.Parallel()

	// Use a stub queue that always returns ErrEnqueueUncertain.
	uncertainQueue := &stubUncertainQueue{}
	svc := service.NewTaskService(
		stubTaskStore{},
		stubFallbackTaskStore{err: store.ErrFallbackTaskNotFound},
		stubValidator{},
		stubProcessor{},
		stubLogger{},
		uncertainQueue,
		120, 1, 0, nil, nil,
	)
	h := NewHandler(svc)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/submit", strings.NewReader(`{"test":true}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.Submit(ctx)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("expected pending status in uncertain response, got: %s", body)
	}
	if !strings.Contains(body, `"warning"`) {
		t.Fatalf("expected warning in uncertain response, got: %s", body)
	}
}

// stubUncertainQueue always returns ErrEnqueueUncertain on Enqueue.
type stubUncertainQueue struct{}

func (q *stubUncertainQueue) Enqueue(ctx context.Context, msg queue.TaskMessage) error {
	return fmt.Errorf("%w: simulated timeout", queue.ErrEnqueueUncertain)
}

func (q *stubUncertainQueue) Dequeue(ctx context.Context) (*queue.Delivery, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (q *stubUncertainQueue) Close() error { return nil }

// Regression: uncertain-enqueue task beyond threshold is marked failed.
// Only pending tasks with EnqueueUncertainAt > 0 get this treatment;
// normal pending tasks are never touched.
func TestGetResultUncertainEnqueueTaskBeyondThresholdMarkedFailed(t *testing.T) {
	t.Parallel()

	taskTimeout := 30 // seconds
	uncertainThreshold := taskTimeout * 3

	h := newTestHandler(
		stubTaskStore{
			getTask: &model.Task{
				UUID:               testUUID6,
				Status:             "pending",
				CreatedAt:          time.Now().Add(-1 * time.Hour).Unix(),
				EnqueueUncertainAt: time.Now().Add(-time.Duration(uncertainThreshold+10) * time.Second).Unix(),
			},
		},
		stubFallbackTaskStore{err: store.ErrFallbackTaskNotFound},
		taskTimeout,
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/result/"+testUUID6, nil)
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "uuid", Value: testUUID6}}

	h.GetResult(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("expected uncertain-enqueue task to be marked failed, got: %s", body)
	}
	if !strings.Contains(body, "enqueue confirmation timed out") {
		t.Fatalf("expected distinct error message for uncertain enqueue, got: %s", body)
	}
}

// Regression: uncertain-enqueue task within threshold stays pending.
func TestGetResultUncertainEnqueueTaskWithinThresholdStaysPending(t *testing.T) {
	t.Parallel()

	h := newTestHandler(
		stubTaskStore{
			getTask: &model.Task{
				UUID:               testUUID7,
				Status:             "pending",
				CreatedAt:          time.Now().Add(-1 * time.Minute).Unix(),
				EnqueueUncertainAt: time.Now().Add(-10 * time.Second).Unix(), // just 10s ago
			},
		},
		stubFallbackTaskStore{err: store.ErrFallbackTaskNotFound},
		120, // 120s timeout → uncertain threshold = 360s, well beyond 10s
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/result/"+testUUID7, nil)
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "uuid", Value: testUUID7}}

	h.GetResult(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("expected uncertain task within threshold to stay pending, got: %s", body)
	}
}
