package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fullstack-pw/shared/tracing"
)

// Level represents a logging level
type Level int

const (
	// Debug level for verbose development logs
	Debug Level = iota
	// Info level for general information
	Info
	// Warn level for warnings
	Warn
	// Error level for errors
	Error
	// Fatal level for fatal errors that should terminate the application
	Fatal
)

// String returns the string representation of the log level
func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	case Fatal:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// Logger is a structured logger with trace context integration
type Logger struct {
	serviceName string
	minLevel    Level
	output      io.Writer
	mu          sync.Mutex

	// Format options
	includeTimestamp bool
	includeLevel     bool
	includeTraceID   bool
	includeSpanID    bool
	includeFile      bool
	jsonFormat       bool
}

// NewLogger creates a new logger
func NewLogger(serviceName string, options ...LoggerOption) *Logger {
	logger := &Logger{
		serviceName:      serviceName,
		minLevel:         Info,
		output:           os.Stdout,
		includeTimestamp: true,
		includeLevel:     true,
		includeTraceID:   true,
		includeSpanID:    false,
		includeFile:      false,
		jsonFormat:       true,
	}

	for _, option := range options {
		option(logger)
	}

	return logger
}

// LoggerOption is a function that configures a Logger
type LoggerOption func(*Logger)

// WithMinLevel sets the minimum logging level
func WithMinLevel(level Level) LoggerOption {
	return func(l *Logger) {
		l.minLevel = level
	}
}

// WithOutput sets the output writer
func WithOutput(output io.Writer) LoggerOption {
	return func(l *Logger) {
		l.output = output
	}
}

// WithJSONFormat enables or disables JSON formatted logs
func WithJSONFormat(enabled bool) LoggerOption {
	return func(l *Logger) {
		l.jsonFormat = enabled
	}
}

// WithTimestamp enables or disables timestamps in logs
func WithTimestamp(enabled bool) LoggerOption {
	return func(l *Logger) {
		l.includeTimestamp = enabled
	}
}

// WithTraceID enables or disables trace IDs in logs
func WithTraceID(enabled bool) LoggerOption {
	return func(l *Logger) {
		l.includeTraceID = enabled
	}
}

// WithSpanID enables or disables span IDs in logs
func WithSpanID(enabled bool) LoggerOption {
	return func(l *Logger) {
		l.includeSpanID = enabled
	}
}

// WithFileInfo enables or disables source file information in logs
func WithFileInfo(enabled bool) LoggerOption {
	return func(l *Logger) {
		l.includeFile = enabled
	}
}

// LogEntry represents a structured log entry
type LogEntry struct {
	Timestamp  string                 `json:"timestamp,omitempty"`
	Level      string                 `json:"level,omitempty"`
	Service    string                 `json:"service,omitempty"`
	Message    string                 `json:"message"`
	TraceID    string                 `json:"trace_id,omitempty"`
	SpanID     string                 `json:"span_id,omitempty"`
	File       string                 `json:"file,omitempty"`
	Line       int                    `json:"line,omitempty"`
	Attributes map[string]interface{} `json:"attributes,omitempty"`
}

// log writes a log message at the specified level
func (l *Logger) log(ctx context.Context, level Level, msg string, attrs map[string]interface{}) {
	if level < l.minLevel {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	entry := LogEntry{
		Service:    l.serviceName,
		Message:    msg,
		Attributes: attrs,
	}

	if l.includeTimestamp {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	if l.includeLevel {
		entry.Level = level.String()
	}

	if l.includeTraceID && ctx != nil {
		entry.TraceID = tracing.GetTraceID(ctx)
	}

	if l.includeSpanID && ctx != nil {
		entry.SpanID = tracing.GetSpanID(ctx)
	}

	if l.includeFile {
		_, file, line, ok := runtime.Caller(2)
		if ok {
			// Trim the file path for readability
			parts := strings.Split(file, "/")
			if len(parts) > 2 {
				file = strings.Join(parts[len(parts)-2:], "/")
			}
			entry.File = file
			entry.Line = line
		}
	}

	if l.jsonFormat {
		// Write as JSON
		jsonData, err := json.Marshal(entry)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error marshalling log entry to JSON: %v\n", err)
			return
		}
		fmt.Fprintln(l.output, string(jsonData))
	} else {
		// Write as plain text
		var sb strings.Builder

		if l.includeTimestamp {
			sb.WriteString(fmt.Sprintf("[%s] ", entry.Timestamp))
		}

		if l.includeLevel {
			sb.WriteString(fmt.Sprintf("%s ", entry.Level))
		}

		if l.includeTraceID && entry.TraceID != "" && entry.TraceID != "unknown" {
			sb.WriteString(fmt.Sprintf("TraceID=%s ", entry.TraceID))
		}

		if l.includeSpanID && entry.SpanID != "" && entry.SpanID != "unknown" {
			sb.WriteString(fmt.Sprintf("SpanID=%s ", entry.SpanID))
		}

		if l.includeFile && entry.File != "" {
			sb.WriteString(fmt.Sprintf("%s:%d ", entry.File, entry.Line))
		}

		sb.WriteString(entry.Message)

		if len(entry.Attributes) > 0 {
			sb.WriteString(" ")
			first := true
			for k, v := range entry.Attributes {
				if !first {
					sb.WriteString(", ")
				}
				sb.WriteString(fmt.Sprintf("%s=%v", k, v))
				first = false
			}
		}

		fmt.Fprintln(l.output, sb.String())
	}

	// Auto-exit on Fatal level
	if level == Fatal {
		os.Exit(1)
	}
}

// Debug logs a debug message
func (l *Logger) Debug(ctx context.Context, msg string, attrs ...interface{}) {
	l.log(ctx, Debug, msg, attributesToMap(attrs...))
}

// Info logs an info message
func (l *Logger) Info(ctx context.Context, msg string, attrs ...interface{}) {
	l.log(ctx, Info, msg, attributesToMap(attrs...))
}

// Warn logs a warning message
func (l *Logger) Warn(ctx context.Context, msg string, attrs ...interface{}) {
	l.log(ctx, Warn, msg, attributesToMap(attrs...))
}

// Error logs an error message
func (l *Logger) Error(ctx context.Context, msg string, attrs ...interface{}) {
	l.log(ctx, Error, msg, attributesToMap(attrs...))
}

// Fatal logs a fatal message and exits
func (l *Logger) Fatal(ctx context.Context, msg string, attrs ...interface{}) {
	l.log(ctx, Fatal, msg, attributesToMap(attrs...))
	// log method handles the os.Exit(1)
}

// WithContext returns a new logger with context values added to every log entry
func (l *Logger) WithContext(ctx context.Context) *ContextLogger {
	return &ContextLogger{
		logger: l,
		ctx:    ctx,
	}
}

// ContextLogger is a logger with a bound context
type ContextLogger struct {
	logger *Logger
	ctx    context.Context
}

// Debug logs a debug message with the bound context
func (cl *ContextLogger) Debug(msg string, attrs ...interface{}) {
	cl.logger.Debug(cl.ctx, msg, attrs...)
}

// Info logs an info message with the bound context
func (cl *ContextLogger) Info(msg string, attrs ...interface{}) {
	cl.logger.Info(cl.ctx, msg, attrs...)
}

// Warn logs a warning message with the bound context
func (cl *ContextLogger) Warn(msg string, attrs ...interface{}) {
	cl.logger.Warn(cl.ctx, msg, attrs...)
}

// Error logs an error message with the bound context
func (cl *ContextLogger) Error(msg string, attrs ...interface{}) {
	cl.logger.Error(cl.ctx, msg, attrs...)
}

// Fatal logs a fatal message with the bound context and exits
func (cl *ContextLogger) Fatal(msg string, attrs ...interface{}) {
	cl.logger.Fatal(cl.ctx, msg, attrs...)
}

// attributesToMap converts a series of key-value pairs to a map
// Format should be key1, value1, key2, value2, etc.
func attributesToMap(attrs ...interface{}) map[string]interface{} {
	if len(attrs) == 0 {
		return nil
	}

	// Handle case where a single map is passed
	if len(attrs) == 1 {
		if m, ok := attrs[0].(map[string]interface{}); ok {
			return m
		}
	}

	result := make(map[string]interface{})

	// Process key-value pairs
	for i := 0; i < len(attrs); i += 2 {
		if i+1 >= len(attrs) {
			break // Ignore the last item if we have an odd number
		}

		key, ok := attrs[i].(string)
		if !ok {
			continue // Skip if key is not a string
		}

		result[key] = attrs[i+1]
	}

	return result
}
