package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ladydd/taskgate/internal/model"
	"github.com/ladydd/taskgate/internal/service"
	"github.com/ladydd/taskgate/internal/store"

	"github.com/gin-gonic/gin"
)

// Handler handles HTTP requests for async task submission and result retrieval.
// It delegates all business logic to the TaskService.
type Handler struct {
	svc *service.TaskService
}

// NewHandler creates a new Handler backed by the given TaskService.
func NewHandler(svc *service.TaskService) *Handler {
	return &Handler{svc: svc}
}

// HealthCheck handles GET /health requests.
func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// maxBodySize is the maximum allowed request body size (10 MB).
const maxBodySize = 10 << 20

// Submit handles POST /submit requests to create an async task.
func (h *Handler) Submit(c *gin.Context) {
	// Limit body size to prevent abuse.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize)

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, model.ErrorResponse{
			Error:   "invalid_json",
			Details: []string{"failed to read request body"},
		})
		return
	}

	// Verify it's valid JSON.
	if !json.Valid(body) {
		c.JSON(http.StatusBadRequest, model.ErrorResponse{
			Error:   "invalid_json",
			Details: []string{"request body is not valid JSON"},
		})
		return
	}

	input := json.RawMessage(body)

	if errs := h.svc.ValidateRequest(input); len(errs) > 0 {
		c.JSON(http.StatusBadRequest, model.ErrorResponse{
			Error:   "validation_error",
			Details: errs,
		})
		return
	}

	taskID, err := h.svc.CreateTask(c.Request.Context(), input)
	if err != nil {
		c.JSON(http.StatusInternalServerError, model.ErrorResponse{
			Error:   "internal_error",
			Details: []string{"failed to create task"},
		})
		return
	}

	// Enqueue the task for async processing by the worker pool.
	// If enqueue fails, mark the task as failed so it doesn't stay pending forever.
	if err := h.svc.EnqueueTask(c.Request.Context(), taskID, input); err != nil {
		h.svc.MarkTaskFailed(c.Request.Context(), taskID, "failed to enqueue task")
		c.JSON(http.StatusInternalServerError, model.ErrorResponse{
			Error:   "internal_error",
			Details: []string{"failed to enqueue task"},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"uuid":   taskID,
		"status": "pending",
	})
}

// GetResult handles GET /result/:uuid requests to retrieve task status and results.
func (h *Handler) GetResult(c *gin.Context) {
	taskUUID := c.Param("uuid")

	task, err := h.svc.GetTaskResult(c.Request.Context(), taskUUID)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			c.JSON(http.StatusNotFound, model.ErrorResponse{
				Error:   "not_found",
				Details: []string{"task not found"},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, model.ErrorResponse{
			Error:   "internal_error",
			Details: []string{"failed to retrieve task"},
		})
		return
	}

	respondTask(c, task)
}

func respondTask(c *gin.Context, task *model.Task) {
	resp := gin.H{
		"uuid":   task.UUID,
		"status": task.Status,
	}

	switch task.Status {
	case "completed":
		// Output is raw JSON — embed it directly so the caller gets structured data.
		if len(task.Output) > 0 {
			resp["output"] = json.RawMessage(task.Output)
		}
	case "failed":
		resp["error"] = task.Error
	}

	c.JSON(http.StatusOK, resp)
}
