# TaskGate

**[中文文档 / Chinese Documentation](README_CN.md)**

A production-ready async task framework in Go. Submit a task, get a UUID, poll for the result. The framework handles queuing, concurrency control, rate limiting, persistence, and fault tolerance — you just implement the business logic.

```
POST /submit  →  { "uuid": "...", "status": "pending" }
GET  /result/:uuid  →  { "uuid": "...", "status": "completed", "output": {...} }
```

## Why

Most async task systems share the same infrastructure: accept a request, queue it, process it in the background, store the result, let the caller poll. TaskGate extracts that infrastructure into a reusable framework so you can focus on the processing logic.

You implement two interfaces:

```go
type TaskProcessor interface {
    Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

type RequestValidator interface {
    Validate(input json.RawMessage) []string
}
```

The framework handles everything else.

## Features

- **RabbitMQ-backed queue** — durable, persistent, survives broker and service restarts
- **Priority queue** — higher priority tasks are processed first
- **Dead-letter queue** — failed/malformed messages are routed to a DLQ for inspection or retry
- **Concurrency control** — configurable worker pool caps parallel in-flight requests
- **Rate limiting** — token bucket limiter caps requests per second to protect downstream APIs
- **Redis state store** — task status and results persisted with 7-day TTL
- **File fallback store** — automatic local persistence when Redis writes fail
- **Stuck task detection** — pending tasks exceeding 2x timeout are auto-marked as failed
- **Input sanitization hook** — strip sensitive fields (API keys, tokens) before persistence
- **Structured logging** — unified slog-based JSON logging to daily-rotated files with configurable retention
- **SSRF protection** — DNS resolution validation blocks private/loopback addresses
- **Graceful shutdown** — drains HTTP connections and waits for in-flight tasks to complete

## Quick Start

### Requirements

- Docker & Docker Compose

### Start

```bash
docker compose up -d --build
```

Services:
- API: `http://localhost:9531`
- RabbitMQ Management UI: `http://localhost:15672` (guest/guest)

### Stop

```bash
docker compose down
```

### Run Tests

```bash
make test
```

## Architecture

```
                         ┌──────────────┐
    POST /submit ──────► │   Handler    │ ──► Redis (status=pending)
                         │  (Gin HTTP)  │ ──► RabbitMQ (enqueue)
                         └──────────────┘ ──► Response {uuid, pending}

                         ┌──────────────┐
    RabbitMQ ──────────► │ Worker Pool  │ ──► Rate Limiter
                         │  (N workers) │ ──► TaskProcessor.Process()
                         └──────────────┘ ──► Redis (completed/failed)
                                          ──► File fallback (if Redis fails)

    GET /result/:uuid ─► Redis ─► Fallback ─► Response {uuid, status, output}
```

### Separation of Concerns

| Component | Responsibility |
|-----------|---------------|
| **Handler** | HTTP parsing, validation, response |
| **RabbitMQ** | Task dispatch, priority, dead-letter routing |
| **Redis** | Task state and result storage |
| **Worker Pool** | Concurrency control, task execution |
| **Rate Limiter** | Downstream QPS protection |
| **Fallback Store** | Resilience when Redis is unavailable |

## Configuration

All settings via environment variables with sensible defaults:

### Core

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `9531` | HTTP server port |
| `REDIS_ADDR` | `redis:6379` | Redis address |
| `REDIS_PASS` | (empty) | Redis password |
| `TASK_TIMEOUT` | `120` | Task processing timeout in seconds |
| `LOG_DIR` | `/var/log/taskgate` | Structured log file directory |
| `FALLBACK_DIR` | `/var/log/taskgate/fallback` | Fallback store directory |
| `LOG_RETENTION_DAYS` | `7` | Days to keep log files before auto-cleanup |

### RabbitMQ

| Variable | Default | Description |
|----------|---------|-------------|
| `RABBITMQ_URL` | `amqp://guest:guest@rabbitmq:5672/` | AMQP connection URL |
| `RABBITMQ_QUEUE` | `tasks` | Main task queue name |
| `MAX_PRIORITY` | `10` | Max priority level (0 = disabled) |
| `DEAD_LETTER_QUEUE` | `tasks.dead` | Dead-letter queue name (empty = disabled) |

### Flow Control

| Variable | Default | Description |
|----------|---------|-------------|
| `WORKER_COUNT` | `10` | Number of worker goroutines (concurrency cap) |
| `PREFETCH_COUNT` | `10` | RabbitMQ prefetch per consumer (backpressure) |
| `RATE_LIMIT` | `0` | Max tasks/second to downstream (0 = unlimited) |

### How Flow Control Works

`WORKER_COUNT` and `RATE_LIMIT` control two independent dimensions:

- **WORKER_COUNT** caps how many requests are in-flight simultaneously (concurrency)
- **RATE_LIMIT** caps how many requests are initiated per second (throughput)

Example configurations:

| Downstream | WORKER_COUNT | RATE_LIMIT | Effect |
|-----------|-------------|-----------|--------|
| OpenAI API (50 QPS, 10 concurrent) | 10 | 50 | Matches API limits exactly |
| Internal fast service (no limits) | 50 | 0 | Full speed |
| Slow image processor (5 QPS, 3 concurrent) | 3 | 5 | Protects downstream |

## Project Structure

```
├── cmd/server/                  # Framework entry point (echo processor demo)
├── example/vlm/                 # Complete example: VLM script generation
│   ├── cmd/main.go              #   Entry point
│   ├── processor.go             #   TaskProcessor implementation
│   ├── validator.go             #   RequestValidator implementation
│   └── sanitizer.go             #   InputSanitizer (strips API keys)
├── internal/
│   ├── config/                  # Environment variable configuration
│   ├── handler/                 # HTTP layer (Gin routes)
│   ├── service/                 # Business orchestration (worker pool, rate limiter)
│   ├── queue/                   # RabbitMQ queue (priority, DLX)
│   ├── store/                   # Redis store + file fallback store
│   ├── logger/                  # Daily-rotated JSON structured logging
│   ├── model/                   # Task model (generic json.RawMessage I/O)
│   ├── netsec/                  # SSRF protection (DNS + IP validation)
│   └── apperror/                # Unified error types
├── Dockerfile                   # Multi-stage build
├── docker-compose.yml           # Redis + RabbitMQ + app
└── Makefile                     # build, test, lint, docker-up/down
```

## Building Your Own Service

1. Implement `TaskProcessor` — your business logic
2. Implement `RequestValidator` — your input validation
3. Optionally provide an `InputSanitizer` — strip sensitive fields before persistence
4. Wire them up in `main.go`

Minimal example (already in `cmd/server/main.go`):

```go
type myProcessor struct{}

func (myProcessor) Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
    // Your business logic here.
    // Parse input, call downstream APIs, return output as JSON.
    return input, nil
}

type myValidator struct{}

func (myValidator) Validate(input json.RawMessage) []string {
    // Return error strings for invalid input, empty slice if valid.
    return nil
}
```

For a complete real-world example, see `example/vlm/` — a VLM-based video script generator with SSRF protection, API key sanitization, and system prompt hot-reloading.

### Logging Inside Your Processor

The framework injects a logger into the context passed to `Process()`. Use the helper functions to log intermediate steps — they'll appear in the same daily log file alongside framework logs, tagged with the task UUID:

```go
func (p *MyProcessor) Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
    // Log before calling downstream
    logger.LogStepFromCtx(ctx, "calling_downstream", map[string]interface{}{
        "url":    "https://api.example.com/v1",
        "method": "POST",
    })

    resp, err := http.Post(...)
    if err != nil {
        logger.LogErrorFromCtx(ctx, "downstream_error", err)
        return nil, err
    }

    // Log after downstream responds
    logger.LogStepFromCtx(ctx, "downstream_response", map[string]interface{}{
        "status_code": resp.StatusCode,
        "duration_ms": elapsed.Milliseconds(),
    })

    return output, nil
}
```

All logs (framework + your processor) end up in the same file in the same JSON format, correlated by UUID.

## API

### Submit a Task

```
POST /submit
Content-Type: application/json

{"your": "task input"}
```

Response:
```json
{"uuid": "550e8400-e29b-41d4-a716-446655440000", "status": "pending"}
```

### Poll for Result

```
GET /result/:uuid
```

Pending:
```json
{"uuid": "...", "status": "pending"}
```

Completed:
```json
{"uuid": "...", "status": "completed", "output": {"your": "result"}}
```

Failed:
```json
{"uuid": "...", "status": "failed", "error": "error message"}
```

### Health Check

```
GET /health
```

```json
{"status": "ok"}
```

## License

MIT
