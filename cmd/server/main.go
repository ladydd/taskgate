// Command server is the default entry point for the taskgate async task framework.
// It ships with a minimal echo processor that returns the input as-is — useful for
// smoke-testing the infrastructure (Redis, RabbitMQ, HTTP) before plugging in real
// business logic.
//
// For a real-world example, see example/vlm/.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ladydd/taskgate/internal/config"
	"github.com/ladydd/taskgate/internal/handler"
	"github.com/ladydd/taskgate/internal/logger"
	"github.com/ladydd/taskgate/internal/queue"
	"github.com/ladydd/taskgate/internal/service"
	"github.com/ladydd/taskgate/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// echoProcessor is a minimal TaskProcessor that returns the input unchanged.
type echoProcessor struct{}

func (echoProcessor) Process(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	return input, nil
}

// echoValidator accepts all inputs.
type echoValidator struct{}

func (echoValidator) Validate(_ context.Context, _ json.RawMessage) []string { return nil }

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// Logger — unified slog output to daily-rotated JSON files.
	fileLogger, slogger, err := logger.NewFileLogger(logger.Config{
		LogDir:        cfg.LogDir,
		RetentionDays: cfg.LogRetentionDays,
	})
	if err != nil {
		slog.Error("failed to initialize logger", "error", err)
		os.Exit(1)
	}
	defer fileLogger.Close()
	slog.SetDefault(slogger)

	// Redis.
	redisClient := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPass,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		slog.Error("failed to connect to Redis", "addr", cfg.RedisAddr, "error", err)
		os.Exit(1)
	}
	slog.Info("connected to Redis", "addr", cfg.RedisAddr)

	// Stores.
	redisStore := store.NewRedisStore(redisClient, nil)
	fallbackStore, err := store.NewFileFallbackTaskStore(cfg.FallbackDir, nil)
	if err != nil {
		slog.Error("failed to initialize fallback task store", "dir", cfg.FallbackDir, "error", err)
		os.Exit(1)
	}

	// Queue.
	taskQueue, err := queue.NewRabbitMQQueue(queue.Config{
		URL:             cfg.RabbitMQURL,
		QueueName:       cfg.RabbitMQQueue,
		PrefetchCount:   cfg.PrefetchCount,
		MaxPriority:     cfg.MaxPriority,
		DeadLetterQueue: cfg.DeadLetterQueue,
	})
	if err != nil {
		slog.Error("failed to initialize RabbitMQ queue", "error", err)
		os.Exit(1)
	}
	defer taskQueue.Close()

	// Service.
	svc := service.NewTaskService(redisStore, fallbackStore, echoValidator{}, echoProcessor{}, fileLogger, taskQueue, cfg.TaskTimeout, cfg.WorkerCount, cfg.RateLimit, nil, nil)
	svc.Start()

	// HTTP.
	h := handler.NewHandler(svc)
	router := gin.Default()
	router.GET("/health", h.HealthCheck)
	router.POST("/submit", h.Submit)
	router.GET("/result/:uuid", h.GetResult)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: router,
	}

	go func() {
		slog.Info("starting echo server", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	slog.Info("received shutdown signal", "signal", sig.String())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}

	svc.Stop()
}
