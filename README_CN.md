# TaskGate

**[English Documentation](README.md)**

一个生产级的 Go 异步任务框架。提交任务，拿到 UUID，轮询结果。框架负责队列、并发控制、速率限制、持久化和容错——你只需要实现业务逻辑。

```
POST /submit  →  { "uuid": "...", "status": "pending" }
GET  /result/:uuid  →  { "uuid": "...", "status": "completed", "output": {...} }
```

## 为什么做这个

大多数异步任务系统的基础设施都是一样的：接收请求、入队、后台处理、存储结果、让调用方轮询。TaskGate 把这套基础设施抽成了一个可复用的框架，让你只关注处理逻辑本身。

你只需要实现两个接口：

```go
type TaskProcessor interface {
    Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

type RequestValidator interface {
    Validate(input json.RawMessage) []string
}
```

其他的事情框架全包了。

## 特性

- **RabbitMQ 队列** — 持久化消息，服务重启和 broker 重启都不丢任务
- **优先级队列** — 高优先级任务插队处理
- **死信队列** — 失败/畸形消息自动路由到 DLQ，可排查、可重试
- **并发控制** — 可配置的 worker pool，限制同时在飞的请求数
- **速率限制** — 令牌桶限流，精确控制每秒打向下游的请求数
- **Redis 状态存储** — 任务状态和结果持久化，7 天 TTL
- **文件兜底存储** — Redis 写入失败时自动持久化到本地文件
- **卡死任务检测** — pending 超过 2 倍超时自动标记为 failed
- **输入脱敏钩子** — 持久化前自动剥离敏感字段（API Key、Token 等）
- **结构化日志** — 基于 slog 的统一 JSON 日志，按天轮转，保留天数可配置
- **SSRF 防护** — DNS 解析校验，拒绝内网/回环地址
- **优雅关停** — 排空 HTTP 连接，等待进行中的任务完成

## 快速开始

### 环境要求

- Docker & Docker Compose

### 启动

```bash
docker compose up -d --build
```

启动后：
- API 地址：`http://localhost:9531`
- RabbitMQ 管理面板：`http://localhost:15672`（guest/guest）

### 停止

```bash
docker compose down
```

### 运行测试

```bash
make test
```

## 架构

```
                         ┌──────────────┐
    POST /submit ──────► │   Handler    │ ──► Redis（status=pending）
                         │  (Gin HTTP)  │ ──► RabbitMQ（入队）
                         └──────────────┘ ──► 响应 {uuid, pending}

                         ┌──────────────┐
    RabbitMQ ──────────► │  Worker Pool │ ──► Rate Limiter（速率限制）
                         │  （N 个 worker）│ ──► TaskProcessor.Process()
                         └──────────────┘ ──► Redis（completed/failed）
                                          ──► 文件兜底（Redis 写失败时）

    GET /result/:uuid ─► Redis ─► 文件兜底 ─► 响应 {uuid, status, output}
```

### 职责分离

| 组件 | 职责 |
|------|------|
| **Handler** | HTTP 解析、校验、响应 |
| **RabbitMQ** | 任务调度、优先级、死信路由 |
| **Redis** | 任务状态和结果存储 |
| **Worker Pool** | 并发控制、任务执行 |
| **Rate Limiter** | 下游 QPS 保护 |
| **Fallback Store** | Redis 不可用时的容灾 |

## 配置项

所有配置通过环境变量设置，均有默认值。

### 基础配置

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `PORT` | `9531` | HTTP 服务端口 |
| `REDIS_ADDR` | `redis:6379` | Redis 地址 |
| `REDIS_PASS` | （空） | Redis 密码 |
| `TASK_TIMEOUT` | `120` | 任务处理超时（秒） |
| `LOG_DIR` | `/var/log/taskgate` | 结构化日志目录 |
| `FALLBACK_DIR` | `/var/log/taskgate/fallback` | 兜底存储目录 |
| `LOG_RETENTION_DAYS` | `7` | 日志文件保留天数，超期自动清理 |

### RabbitMQ 配置

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `RABBITMQ_URL` | `amqp://guest:guest@rabbitmq:5672/` | AMQP 连接地址 |
| `RABBITMQ_QUEUE` | `tasks` | 主任务队列名 |
| `MAX_PRIORITY` | `10` | 最大优先级（0 = 禁用） |
| `DEAD_LETTER_QUEUE` | `tasks.dead` | 死信队列名（空 = 禁用） |

### 流量控制

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `WORKER_COUNT` | `10` | Worker 数量（并发上限） |
| `PREFETCH_COUNT` | `10` | RabbitMQ 每个 consumer 的预取数（背压控制） |
| `RATE_LIMIT` | `0` | 每秒最大请求数（0 = 不限制） |

### 流量控制原理

`WORKER_COUNT` 和 `RATE_LIMIT` 控制两个独立的维度：

- **WORKER_COUNT** 限制同时在飞的请求数（并发）
- **RATE_LIMIT** 限制每秒发出的请求数（吞吐量）

两个约束同时生效，互相兜底。

配置示例：

| 下游场景 | WORKER_COUNT | RATE_LIMIT | 效果 |
|---------|-------------|-----------|------|
| OpenAI API（50 QPS，10 并发） | 10 | 50 | 精确匹配 API 限制 |
| 内部高速服务（无限制） | 50 | 0 | 全速运行 |
| 慢速图片处理（5 QPS，3 并发） | 3 | 5 | 保护下游 |

## 项目结构

```
├── cmd/server/                  # 框架入口（echo processor 演示）
├── example/vlm/                 # 完整示例：VLM 视频脚本生成
│   ├── cmd/main.go              #   入口
│   ├── processor.go             #   TaskProcessor 实现
│   ├── validator.go             #   RequestValidator 实现
│   └── sanitizer.go             #   InputSanitizer（脱敏 API Key）
├── internal/
│   ├── config/                  # 环境变量配置
│   ├── handler/                 # HTTP 层（Gin 路由）
│   ├── service/                 # 业务编排（worker pool、rate limiter）
│   ├── queue/                   # RabbitMQ 队列（优先级、死信）
│   ├── store/                   # Redis 存储 + 文件兜底存储
│   ├── logger/                  # 按天滚动的 JSON 结构化日志
│   ├── model/                   # 任务模型（通用 json.RawMessage 输入输出）
│   ├── netsec/                  # SSRF 防护（DNS + IP 校验）
│   └── apperror/                # 统一错误类型
├── Dockerfile                   # 多阶段构建
├── docker-compose.yml           # Redis + RabbitMQ + 应用
└── Makefile                     # build, test, lint, docker-up/down
```

## 如何基于框架构建你的服务

1. 实现 `TaskProcessor` — 你的业务逻辑
2. 实现 `RequestValidator` — 你的输入校验
3. 可选：提供 `InputSanitizer` — 持久化前剥离敏感字段
4. 在 `main.go` 里组装注入

最小示例（已在 `cmd/server/main.go` 中）：

```go
type myProcessor struct{}

func (myProcessor) Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
    // 你的业务逻辑
    // 解析 input，调用下游 API，返回 JSON 格式的 output
    return input, nil
}

type myValidator struct{}

func (myValidator) Validate(input json.RawMessage) []string {
    // 返回错误描述列表，空切片表示校验通过
    return nil
}
```

完整的真实示例见 `example/vlm/` — 一个基于 VLM 的视频脚本生成服务，包含 SSRF 防护、API Key 脱敏和系统提示词热加载。

### 在 Processor 里记录日志

框架会自动把 logger 注入到传给 `Process()` 的 context 中。你可以用 helper 函数记录中间步骤——这些日志会和框架日志写到同一个文件里，自动带上任务 UUID：

```go
func (p *MyProcessor) Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
    // 调用下游前记录
    logger.LogStepFromCtx(ctx, "calling_downstream", map[string]interface{}{
        "url":    "https://api.example.com/v1",
        "method": "POST",
    })

    resp, err := http.Post(...)
    if err != nil {
        logger.LogErrorFromCtx(ctx, "downstream_error", err)
        return nil, err
    }

    // 下游返回后记录
    logger.LogStepFromCtx(ctx, "downstream_response", map[string]interface{}{
        "status_code": resp.StatusCode,
        "duration_ms": elapsed.Milliseconds(),
    })

    return output, nil
}
```

所有日志（框架 + 你的 Processor）输出到同一个文件，同一种 JSON 格式，通过 UUID 串联。

## API

### 提交任务

```
POST /submit
Content-Type: application/json

{"your": "task input"}
```

响应：
```json
{"uuid": "550e8400-e29b-41d4-a716-446655440000", "status": "pending"}
```

### 轮询结果

```
GET /result/:uuid
```

处理中：
```json
{"uuid": "...", "status": "pending"}
```

完成：
```json
{"uuid": "...", "status": "completed", "output": {"your": "result"}}
```

失败：
```json
{"uuid": "...", "status": "failed", "error": "错误信息"}
```

### 健康检查

```
GET /health
```

```json
{"status": "ok"}
```

## License

MIT
