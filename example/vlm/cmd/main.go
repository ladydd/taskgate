// Command vlm-server is an example application built on the taskgate
// async task framework. It generates UGC short-video shooting scripts by
// calling an OpenAI-compatible Vision Language Model API.
//
// Usage:
//
//	go run ./example/vlm/cmd
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	vlm "github.com/ladydd/taskgate/example/vlm"
	"github.com/ladydd/taskgate/internal/config"
	"github.com/ladydd/taskgate/internal/handler"
	"github.com/ladydd/taskgate/internal/logger"
	"github.com/ladydd/taskgate/internal/queue"
	"github.com/ladydd/taskgate/internal/service"
	"github.com/ladydd/taskgate/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// VLM-specific config.
	promptFilePath := getEnv("PROMPT_FILE_PATH", "/app/prompts/system_prompt.txt")

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
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPass,
	})
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		slog.Error("failed to connect to Redis", "addr", cfg.RedisAddr, "error", err)
		os.Exit(1)
	}
	slog.Info("connected to Redis", "addr", cfg.RedisAddr)

	// Stores.
	sanitizer := vlm.SanitizeInput
	redisStore := store.NewRedisStore(redisClient, sanitizer)
	fallbackStore, err := store.NewFileFallbackTaskStore(cfg.FallbackDir, sanitizer)
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

	// VLM processor and validator.
	processor := vlm.NewProcessor(cfg.TaskTimeout, promptFilePath)
	validator := &vlm.Validator{}

	svc := service.NewTaskService(redisStore, fallbackStore, validator, processor, fileLogger, taskQueue, cfg.TaskTimeout, cfg.WorkerCount, cfg.RateLimit, service.LogSanitizer(vlm.SanitizeInput))
	svc.Start()
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
		slog.Info("starting VLM example server", "port", cfg.Port)
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

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}
