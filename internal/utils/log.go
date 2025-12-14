// Package utils provides utility functions for SLB including structured logging.
package utils

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/log"
)

// LoggerOptions configures the logger.
type LoggerOptions struct {
	// Level is the minimum log level (debug, info, warn, error)
	Level string
	// Output is the writer for log output (default: os.Stderr)
	Output io.Writer
	// Prefix is the component name prefix
	Prefix string
	// TimeFormat is the time format string (default: RFC3339)
	TimeFormat string
	// ReportCaller adds file:line to log entries
	ReportCaller bool
	// ReportTimestamp adds timestamps to log entries
	ReportTimestamp bool
}

// DefaultLoggerOptions returns sensible default options.
func DefaultLoggerOptions() LoggerOptions {
	return LoggerOptions{
		Level:           "info",
		Output:          os.Stderr,
		Prefix:          "",
		TimeFormat:      time.RFC3339,
		ReportCaller:    false,
		ReportTimestamp: true,
	}
}

// parseLevel converts a string level to log.Level.
func parseLevel(level string) log.Level {
	switch strings.ToLower(level) {
	case "debug":
		return log.DebugLevel
	case "info":
		return log.InfoLevel
	case "warn", "warning":
		return log.WarnLevel
	case "error":
		return log.ErrorLevel
	case "fatal":
		return log.FatalLevel
	default:
		return log.InfoLevel
	}
}

// InitLogger creates a new logger with the given options.
func InitLogger(opts LoggerOptions) *log.Logger {
	logger := log.NewWithOptions(opts.Output, log.Options{
		Level:           parseLevel(opts.Level),
		Prefix:          opts.Prefix,
		TimeFormat:      opts.TimeFormat,
		ReportCaller:    opts.ReportCaller,
		ReportTimestamp: opts.ReportTimestamp,
	})
	return logger
}

// InitDefaultLogger creates a logger with default options, respecting SLB_LOG_LEVEL env.
func InitDefaultLogger() *log.Logger {
	opts := DefaultLoggerOptions()

	// Check environment override
	if level := os.Getenv("SLB_LOG_LEVEL"); level != "" {
		opts.Level = level
	}

	return InitLogger(opts)
}

// InitFileLogger creates a logger that writes to a file.
func InitFileLogger(path string, opts LoggerOptions) (*log.Logger, error) {
	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, err
	}

	// Open file for append
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, err
	}

	opts.Output = f
	return InitLogger(opts), nil
}

// InitDaemonLogger creates the logger for daemon mode.
// Writes to ~/.slb/daemon.log with structured output.
func InitDaemonLogger() (*log.Logger, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	logPath := filepath.Join(home, ".slb", "daemon.log")
	opts := LoggerOptions{
		Level:           "info",
		Prefix:          "daemon",
		TimeFormat:      time.RFC3339,
		ReportCaller:    true,
		ReportTimestamp: true,
	}

	// Check environment override
	if level := os.Getenv("SLB_LOG_LEVEL"); level != "" {
		opts.Level = level
	}

	return InitFileLogger(logPath, opts)
}

// InitRequestLogger creates a per-request logger.
// Writes to .slb/logs/req-<id>.log in the project directory.
func InitRequestLogger(projectDir, requestID string) (*log.Logger, error) {
	logDir := filepath.Join(projectDir, ".slb", "logs")
	logPath := filepath.Join(logDir, "req-"+requestID+".log")

	opts := LoggerOptions{
		Level:           "debug", // Request logs capture everything
		Prefix:          requestID[:8],
		TimeFormat:      time.RFC3339,
		ReportCaller:    true,
		ReportTimestamp: true,
	}

	return InitFileLogger(logPath, opts)
}

// Global default logger instance
var defaultLogger = InitDefaultLogger()

// SetDefaultLogger replaces the global default logger.
func SetDefaultLogger(logger *log.Logger) {
	defaultLogger = logger
}

// GetDefaultLogger returns the global default logger.
func GetDefaultLogger() *log.Logger {
	return defaultLogger
}

// Convenience wrappers for the default logger

// Debug logs a debug message with key-value pairs.
func Debug(msg interface{}, keyvals ...interface{}) {
	defaultLogger.Debug(msg, keyvals...)
}

// Info logs an info message with key-value pairs.
func Info(msg interface{}, keyvals ...interface{}) {
	defaultLogger.Info(msg, keyvals...)
}

// Warn logs a warning message with key-value pairs.
func Warn(msg interface{}, keyvals ...interface{}) {
	defaultLogger.Warn(msg, keyvals...)
}

// Error logs an error message with key-value pairs.
func Error(msg interface{}, keyvals ...interface{}) {
	defaultLogger.Error(msg, keyvals...)
}

// Fatal logs a fatal message and exits.
func Fatal(msg interface{}, keyvals ...interface{}) {
	defaultLogger.Fatal(msg, keyvals...)
}

// With returns a logger with additional default key-value pairs.
func With(keyvals ...interface{}) *log.Logger {
	return defaultLogger.With(keyvals...)
}

// WithPrefix returns a logger with the given prefix.
func WithPrefix(prefix string) *log.Logger {
	return defaultLogger.WithPrefix(prefix)
}
