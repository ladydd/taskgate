package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds the application configuration loaded from environment variables.
// This is the framework-level config; business-specific settings should be loaded
// separately by the concrete TaskProcessor / RequestValidator implementations.
type Config struct {
	Port        string // HTTP server port, default "9531"
	RedisAddr   string // Redis address, default "redis:6379"
	RedisPass   string // Redis password, default ""
	TaskTimeout int    // Task processing timeout in seconds, default 120
	LogDir      string // Directory for structured log files, default "/var/log/taskgate"
	FallbackDir string // Fallback directory when Redis writes fail, default "/var/log/taskgate/fallback"
	LogRetentionDays int // Days to keep log files before cleanup, default 7

	// RabbitMQ settings.
	RabbitMQURL     string // AMQP connection URL, default "amqp://guest:guest@rabbitmq:5672/"
	RabbitMQQueue   string // Main task queue name, default "tasks"
	WorkerCount     int    // Number of worker goroutines, default 10
	PrefetchCount   int    // RabbitMQ prefetch count per consumer, default same as WorkerCount
	MaxPriority     int    // Max priority level (0 = disabled), default 10
	DeadLetterQueue string // Dead-letter queue name (empty = disabled), default "tasks.dead"

	// Rate limiting.
	RateLimit int // Max tasks per second sent to downstream (0 = unlimited), default 0
}

// Load reads configuration from environment variables, applying defaults where not set.
func Load() *Config {
	workerCount := getEnvInt("WORKER_COUNT", 10)
	return &Config{
		Port:        getEnv("PORT", "9531"),
		RedisAddr:   getEnv("REDIS_ADDR", "redis:6379"),
		RedisPass:   getEnv("REDIS_PASS", ""),
		TaskTimeout: getEnvInt("TASK_TIMEOUT", 120),
		LogDir:      getEnv("LOG_DIR", "/var/log/taskgate"),
		FallbackDir: getEnv("FALLBACK_DIR", "/var/log/taskgate/fallback"),
		LogRetentionDays: getEnvInt("LOG_RETENTION_DAYS", 7),

		RabbitMQURL:     getEnv("RABBITMQ_URL", "amqp://guest:guest@rabbitmq:5672/"),
		RabbitMQQueue:   getEnv("RABBITMQ_QUEUE", "tasks"),
		WorkerCount:     workerCount,
		PrefetchCount:   getEnvInt("PREFETCH_COUNT", workerCount),
		MaxPriority:     getEnvInt("MAX_PRIORITY", 10),
		DeadLetterQueue: getEnv("DEAD_LETTER_QUEUE", "tasks.dead"),
		RateLimit:       getEnvInt("RATE_LIMIT", 0),
	}
}

// Validate checks that the configuration values are sensible.
func (c *Config) Validate() error {
	if c.Port == "" {
		return fmt.Errorf("PORT must not be empty")
	}
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("PORT must be a valid port number (1-65535), got %q", c.Port)
	}
	if c.RedisAddr == "" {
		return fmt.Errorf("REDIS_ADDR must not be empty")
	}
	if c.TaskTimeout <= 0 {
		return fmt.Errorf("TASK_TIMEOUT must be a positive integer, got %d", c.TaskTimeout)
	}
	if c.LogDir == "" {
		return fmt.Errorf("LOG_DIR must not be empty")
	}
	if c.FallbackDir == "" {
		return fmt.Errorf("FALLBACK_DIR must not be empty")
	}
	if c.LogRetentionDays <= 0 {
		return fmt.Errorf("LOG_RETENTION_DAYS must be a positive integer, got %d", c.LogRetentionDays)
	}
	if c.RabbitMQURL == "" {
		return fmt.Errorf("RABBITMQ_URL must not be empty")
	}
	if c.RabbitMQQueue == "" {
		return fmt.Errorf("RABBITMQ_QUEUE must not be empty")
	}
	if c.WorkerCount <= 0 {
		return fmt.Errorf("WORKER_COUNT must be a positive integer, got %d", c.WorkerCount)
	}
	if c.PrefetchCount <= 0 {
		return fmt.Errorf("PREFETCH_COUNT must be a positive integer, got %d", c.PrefetchCount)
	}
	if c.MaxPriority < 0 || c.MaxPriority > 255 {
		return fmt.Errorf("MAX_PRIORITY must be between 0 and 255, got %d", c.MaxPriority)
	}
	if c.RateLimit < 0 {
		return fmt.Errorf("RATE_LIMIT must be non-negative, got %d", c.RateLimit)
	}
	return nil
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val, ok := os.LookupEnv(key); ok {
		if intVal, err := strconv.Atoi(val); err == nil {
			return intVal
		}
	}
	return defaultVal
}
