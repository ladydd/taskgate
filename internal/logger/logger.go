// Package logger provides structured task logging built on top of Go's slog.
//
// It writes JSON-formatted log entries to daily-rotated files and automatically
// cleans up files older than a configurable retention period. The same slog handler
// is used for both framework-level logs (via the global slog.Default) and task-level
// logs (via the TaskLogger interface), so all output goes to one place in one format.
package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TaskLogger defines the interface for structured task logging.
// Implementations are free to write to files, databases, or external services.
type TaskLogger interface {
	// LogStep records a named step in the task lifecycle (e.g. "request", "process", "result").
	LogStep(ctx context.Context, uuid string, step string, data map[string]interface{}) error
	// LogError records an error that occurred during task processing.
	LogError(ctx context.Context, uuid string, errType string, err error) error
}

// Config holds the configuration for the file logger.
type Config struct {
	// LogDir is the directory where daily log files are written.
	LogDir string
	// RetentionDays is how many days of log files to keep. Files older than this are deleted.
	// Defaults to 7 if <= 0.
	RetentionDays int
}

// FileLogger implements TaskLogger by writing structured slog entries to daily-rotated files.
// It also implements io.Writer so it can be used as the output for a slog.JSONHandler,
// unifying framework-level and task-level logs into a single stream.
type FileLogger struct {
	logDir        string
	retentionDays int
	mu            sync.Mutex
	currentDate   string
	currentFile   *os.File
	slogger       *slog.Logger
	done          chan struct{}
}

// NewFileLogger creates a new FileLogger and starts a background cleanup goroutine.
// It returns both the FileLogger (which implements TaskLogger) and a *slog.Logger
// that should be set as the global default via slog.SetDefault().
func NewFileLogger(cfg Config) (*FileLogger, *slog.Logger, error) {
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}

	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		return nil, nil, fmt.Errorf("failed to create log directory %s: %w", cfg.LogDir, err)
	}

	fl := &FileLogger{
		logDir:        cfg.LogDir,
		retentionDays: cfg.RetentionDays,
		done:          make(chan struct{}),
	}

	// Create a slog.JSONHandler that writes through the FileLogger (which implements io.Writer).
	// This means both slog.Info("...") and fl.LogStep(...) end up in the same daily file.
	handler := slog.NewJSONHandler(fl, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	fl.slogger = slog.New(handler)

	go fl.cleanupLoop()

	return fl, fl.slogger, nil
}

// Close stops the background cleanup goroutine and closes the current log file.
func (fl *FileLogger) Close() {
	close(fl.done)

	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.currentFile != nil {
		fl.currentFile.Close()
		fl.currentFile = nil
	}
}

// Write implements io.Writer. slog.JSONHandler calls this for every log line.
// It routes the bytes to the current day's log file, rotating when the date changes.
func (fl *FileLogger) Write(p []byte) (n int, err error) {
	fl.mu.Lock()
	defer fl.mu.Unlock()

	f, err := fl.getFile()
	if err != nil {
		// If we can't write to the file, fall back to stderr so logs aren't silently lost.
		return os.Stderr.Write(p)
	}

	n, err = f.Write(p)
	if err != nil {
		// Write failed (disk full?) — fall back to stderr.
		os.Stderr.Write(p)
	}
	return n, err
}

// LogStep records a named step in the task lifecycle.
func (fl *FileLogger) LogStep(ctx context.Context, uuid string, step string, data map[string]interface{}) error {
	attrs := make([]slog.Attr, 0, len(data)+2)
	attrs = append(attrs, slog.String("uuid", uuid))
	attrs = append(attrs, slog.String("event", step))
	for k, v := range data {
		attrs = append(attrs, slog.Any(k, v))
	}

	fl.slogger.LogAttrs(ctx, slog.LevelInfo, "task.step", attrs...)
	return nil
}

// LogError records an error that occurred during task processing.
func (fl *FileLogger) LogError(ctx context.Context, uuid string, errType string, err error) error {
	fl.slogger.LogAttrs(ctx, slog.LevelError, "task.error",
		slog.String("uuid", uuid),
		slog.String("error_type", errType),
		slog.String("error", err.Error()),
	)
	return nil
}

// getFile returns the file handle for today's log file.
// Caller must hold fl.mu.
func (fl *FileLogger) getFile() (*os.File, error) {
	today := time.Now().UTC().Format("2006-01-02")

	if fl.currentFile != nil && fl.currentDate == today {
		return fl.currentFile, nil
	}

	if fl.currentFile != nil {
		fl.currentFile.Close()
		fl.currentFile = nil
	}

	filename := fmt.Sprintf("tasks-%s.log", today)
	path := filepath.Join(fl.logDir, filename)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w", path, err)
	}

	fl.currentDate = today
	fl.currentFile = f
	return f, nil
}

// cleanupLoop runs daily to remove log files older than the retention period.
func (fl *FileLogger) cleanupLoop() {
	fl.cleanupOldFiles()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			fl.cleanupOldFiles()
		case <-fl.done:
			return
		}
	}
}

// cleanupOldFiles removes log files older than retentionDays from the log directory.
func (fl *FileLogger) cleanupOldFiles() {
	entries, err := os.ReadDir(fl.logDir)
	if err != nil {
		return
	}

	cutoff := time.Now().AddDate(0, 0, -fl.retentionDays)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, "tasks-") || !strings.HasSuffix(name, ".log") {
			continue
		}

		dateStr := strings.TrimPrefix(name, "tasks-")
		dateStr = strings.TrimSuffix(dateStr, ".log")

		fileDate, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}

		if fileDate.Before(cutoff) {
			os.Remove(filepath.Join(fl.logDir, name))
		}
	}
}

// Ensure FileLogger implements io.Writer at compile time.
var _ io.Writer = (*FileLogger)(nil)

// --- Context helpers for use inside TaskProcessor implementations ---

// ctxKey is an unexported type for logger context keys.
type ctxKey struct{}

// WithLogger returns a new context carrying the given TaskLogger.
// The framework calls this automatically before invoking TaskProcessor.Process(),
// so processor implementations can retrieve the logger via FromContext().
func WithLogger(ctx context.Context, l TaskLogger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext retrieves the TaskLogger from the context.
// Returns nil if no logger is present.
func FromContext(ctx context.Context) TaskLogger {
	l, _ := ctx.Value(ctxKey{}).(TaskLogger)
	return l
}

// LogStepFromCtx is a convenience function for use inside TaskProcessor.Process().
// It retrieves the logger from the context and logs a step. If no logger is in the
// context, the call is silently ignored.
//
// Example usage inside a TaskProcessor:
//
//	func (p *MyProcessor) Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
//	    logger.LogStepFromCtx(ctx, "calling_downstream", map[string]interface{}{
//	        "url":    "https://api.example.com/v1",
//	        "method": "POST",
//	    })
//	    // ... call downstream ...
//	    logger.LogStepFromCtx(ctx, "downstream_response", map[string]interface{}{
//	        "status_code": resp.StatusCode,
//	        "duration_ms": elapsed.Milliseconds(),
//	    })
//	    return output, nil
//	}
func LogStepFromCtx(ctx context.Context, step string, data map[string]interface{}) {
	l := FromContext(ctx)
	if l == nil {
		return
	}
	// Extract uuid from context if available.
	uuid, _ := ctx.Value(requestIDKey{}).(string)
	l.LogStep(ctx, uuid, step, data)
}

// LogErrorFromCtx is a convenience function for use inside TaskProcessor.Process().
// It retrieves the logger from the context and logs an error.
func LogErrorFromCtx(ctx context.Context, errType string, err error) {
	l := FromContext(ctx)
	if l == nil {
		return
	}
	uuid, _ := ctx.Value(requestIDKey{}).(string)
	l.LogError(ctx, uuid, errType, err)
}

// requestIDKey is used to extract the task UUID from context.
// This must match the key used by the service package when injecting the UUID.
type requestIDKey struct{}

// RequestIDCtxKey returns the context key used to store the task UUID.
// The service package uses this to inject the UUID before calling TaskProcessor.Process().
func RequestIDCtxKey() interface{} {
	return requestIDKey{}
}
