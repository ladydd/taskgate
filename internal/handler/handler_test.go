package handler

import (
	"context"
	"encoding/json"
	"errors"
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

func (stubValidator) Validate(input json.RawMessage) []string { return nil }

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
	if body := recorder.Body.String(); !strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("unexpected response body: %s", body)
	}
}
