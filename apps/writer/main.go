package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fullstack-pw/shared/connections"
	"github.com/fullstack-pw/shared/health"
	"github.com/fullstack-pw/shared/logging"
	"github.com/fullstack-pw/shared/server"
	"github.com/fullstack-pw/shared/tracing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RedisMessage represents the data structure coming from Redis
type RedisMessage struct {
	ID      string            `json:"id,omitempty"`
	Content string            `json:"content"`
	Headers map[string]string `json:"headers,omitempty"`
}

var (
	// Global variables
	redisConn    *connections.RedisConnection
	postgresConn *connections.PostgresConnection
	logger       *logging.Logger
	tracer       = tracing.GetTracer("writer")
)

func writeToPostgres(ctx context.Context, id string, content string, headers map[string]string, asciiTerminal, asciiFile, asciiHTML string) error {
	// Create a child span for the database operation
	ctx, span := tracer.Start(ctx, "pg.write_message", trace.WithAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation", "insert"),
		attribute.String("message.id", id),
	))
	defer span.End()
	tableName := getTableName()
	logger.Debug(ctx, "Starting PostgreSQL write", "id", id, "table", tableName)

	span.SetAttributes(
		attribute.String("message.content", content),
		attribute.Int("headers.count", len(headers)),
		attribute.Bool("has_ascii_terminal", asciiTerminal != ""),
		attribute.Bool("has_ascii_file", asciiFile != ""),
		attribute.Bool("has_ascii_html", asciiHTML != ""),
	)
	traceID := tracing.GetTraceID(ctx)
	span.SetAttributes(attribute.String("trace.id", traceID))

	// Prepare headers as JSONB
	headersJSON := "{}"
	if len(headers) > 0 {
		headersBytes, err := json.Marshal(headers)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "Failed to serialize headers")
			logger.Error(ctx, "Failed to serialize headers", "error", err)
			return fmt.Errorf("failed to serialize headers: %w", err)
		}
		headersJSON = string(headersBytes)
		logger.Debug(ctx, "Serialized headers", "headers", headersJSON)
	}

	// If ID is empty, generate one
	if id == "" {
		id = fmt.Sprintf("gen-%d", time.Now().UnixNano())
		span.SetAttributes(attribute.String("generated.id", id))
		logger.Debug(ctx, "Generated ID", "id", id)
	}

	// Prepare the SQL query
	query := fmt.Sprintf(`
        INSERT INTO %s (id, content, source, headers, trace_id, ascii_terminal, ascii_file, ascii_html)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        ON CONFLICT (id) DO UPDATE
        SET content = EXCLUDED.content,
            headers = EXCLUDED.headers,
            trace_id = EXCLUDED.trace_id,
            ascii_terminal = EXCLUDED.ascii_terminal,
            ascii_file = EXCLUDED.ascii_file,
            ascii_html = EXCLUDED.ascii_html,
            created_at = NOW()
    `, tableName)
	logger.Debug(ctx, "Executing query",
		"query", query,
		"id", id,
		"content", content,
		"source", "redis",
		"headers", headersJSON,
		"trace_id", traceID,
		"has_ascii_terminal", asciiTerminal != "",
		"has_ascii_file", asciiFile != "",
		"has_ascii_html", asciiHTML != "",
	)

	if headers != nil {
		if imageURL, ok := headers["image_url"]; ok {
			logger.Info(ctx, "Storing message with image URL in PostgreSQL",
				"id", id,
				"image_url", imageURL)
			span.SetAttributes(attribute.String("message.image_url", imageURL))
		}
	}
	// Execute the query
	_, err := postgresConn.ExecuteWithTracing(ctx, query, id, content, "redis", headersJSON, traceID, asciiTerminal, asciiFile, asciiHTML)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to insert message")
		logger.Error(ctx, "Database error", "error", err)
		return fmt.Errorf("failed to insert message: %w", err)
	}

	span.SetStatus(codes.Ok, "Message inserted successfully")
	logger.Info(ctx, "Message written to PostgreSQL", "id", id, "trace_id", traceID)
	return nil
}

// processRedisMessage processes a message from Redis with tracing
func processRedisMessage(ctx context.Context, key string, value string) error {
	// Extract trace context from message if available
	var parentCtx = ctx
	var message RedisMessage
	logger.Info(ctx, "Starting to process Redis message", "key", key)
	if err := json.Unmarshal([]byte(value), &message); err == nil && message.Headers != nil {
		// Extract trace context from headers if present
		parentCtx = tracing.ExtractTraceContext(context.Background(), message.Headers)
		logger.Debug(ctx, "Extracted trace context from Redis message")
	}

	// Start new span as child of the extracted context
	ctx, span := tracer.Start(parentCtx, "redis.process_message", trace.WithAttributes(
		attribute.String("redis.key", key),
	))
	defer span.End()
	logger.Debug(ctx, "Processing Redis message", "key", key, "value", value)

	// Try to parse the message as JSON
	var headers map[string]string
	var content string
	var id string

	err := json.Unmarshal([]byte(value), &message)
	if err != nil {
		// If not valid JSON, use the raw value as content
		logger.Warn(ctx, "Message is not valid JSON, using raw value", "error", err)
		span.SetAttributes(attribute.Bool("message.is_json", false))
		content = value

		// Extract ID from the key - must handle environment prefix
		// Format is env:msg-123, so we need to get the part after the last colon
		parts := strings.Split(key, ":")
		if len(parts) >= 2 {
			id = strings.Join(parts[1:], ":")
		} else {
			id = key
		}
	} else {
		// Valid JSON message
		logger.Debug(ctx, "Successfully parsed JSON message", "message", message)
		span.SetAttributes(attribute.Bool("message.is_json", true))
		content = message.Content
		headers = message.Headers
		id = message.ID
		if id == "" {
			// Extract ID from the key - must handle environment prefix
			parts := strings.Split(key, ":")
			if len(parts) >= 2 {
				id = strings.Join(parts[1:], ":")
			} else {
				id = key
			}
		}
	}

	// Check for ASCII art in Redis based on message ID
	var asciiTerminal, asciiFile, asciiHTML string
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev" // Default to dev if ENV not set
	}

	// Only try to retrieve ASCII art if we have a valid message ID
	if id != "" {
		// Check for terminal ASCII art
		terminalKey := fmt.Sprintf("%s:%s:ascii:terminal", env, id)
		val, err := redisConn.Client().Get(ctx, terminalKey).Result()
		if err == nil {
			logger.Info(ctx, "Found terminal ASCII art", "id", id, "key", terminalKey)
			asciiTerminal = val
		}

		// Check for file ASCII art
		fileKey := fmt.Sprintf("%s:%s:ascii:file", env, id)
		val, err = redisConn.Client().Get(ctx, fileKey).Result()
		if err == nil {
			logger.Info(ctx, "Found file ASCII art", "id", id, "key", fileKey)
			asciiFile = val
		}

		// Check for HTML ASCII art
		htmlKey := fmt.Sprintf("%s:%s:ascii:html", env, id)
		val, err = redisConn.Client().Get(ctx, htmlKey).Result()
		if err == nil {
			logger.Info(ctx, "Found HTML ASCII art", "id", id, "key", htmlKey)
			asciiHTML = val
		}
	}

	if message.Headers != nil {
		if imageURL, ok := message.Headers["image_url"]; ok {
			logger.Info(ctx, "Processing message with image URL",
				"id", message.ID,
				"key", key,
				"image_url", imageURL)
			span.SetAttributes(attribute.String("message.image_url", imageURL))
		}
	}

	// Write to PostgreSQL with ASCII art if available
	logger.Debug(ctx, "Writing to PostgreSQL", "id", id, "content", content)
	err = writeToPostgres(ctx, id, content, headers, asciiTerminal, asciiFile, asciiHTML)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to write to PostgreSQL")
		return fmt.Errorf("failed to write to PostgreSQL: %w", err)
	}

	// Delete the key from Redis after successful processing
	err = redisConn.Client().Del(ctx, key).Err()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to delete key from Redis")
		logger.Warn(ctx, "Failed to delete key from Redis", "key", key, "error", err)
		// Continue despite this error
	} else {
		logger.Debug(ctx, "Key deleted from Redis after processing", "key", key)
	}

	span.SetStatus(codes.Ok, "Message processed successfully")
	return nil
}

// subscribeToRedisChanges sets up subscription to Redis keyspace notifications
func subscribeToRedisChanges(ctx context.Context) {
	logger.Info(ctx, "Setting up Redis keyspace notifications")

	// Get environment for key prefixing
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev" // Default to dev if ENV not set
	}

	// Set up keyspace notifications if possible
	err := redisConn.SetupRedisNotifications(ctx)
	if err != nil {
		logger.Error(ctx, "Failed to enable Redis keyspace notifications, falling back to polling", "error", err)
		// TODO: Implement polling fallback if needed
		return
	}

	// Subscribe to keyspace notifications with environment-specific patterns
	// We need to watch for keys with the environment prefix
	patterns := []string{
		"__keyevent@0__:set",
		"__keyevent@0__:hset",
		fmt.Sprintf("__keyspace@0__:%s:msg-*", env), // Environment-specific prefix
	}
	pubsub := redisConn.SubscribeToKeyspace(ctx, patterns...)
	defer func() {
		if err := pubsub.Close(); err != nil {
			logger.Error(ctx, "Error closing Redis pubsub", "error", err)
		}
	}()

	logger.Info(ctx, "Listening for Redis keyspace notifications", "environment", env, "patterns", patterns)

	// Process messages from the subscription channel
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			logger.Info(ctx, "Context canceled, stopping Redis subscription")
			return
		case msg, ok := <-ch:
			if !ok {
				logger.Warn(ctx, "Redis subscription channel closed")
				return
			}

			logger.Debug(ctx, "Received message from Redis channel", "channel", msg.Channel, "payload", msg.Payload)

			// Extract the actual key from the channel pattern
			var key string

			if strings.Contains(msg.Channel, "__keyspace@0__:") {
				// For keyspace events, the key is in the channel name
				key = strings.TrimPrefix(msg.Channel, "__keyspace@0__:")
				logger.Debug(ctx, "Extracted key from keyspace event", "key", key)
			} else if strings.Contains(msg.Channel, "__keyevent@0__:") {
				// For keyevent notifications, the key is in the payload
				key = msg.Payload
				logger.Debug(ctx, "Extracted key from keyevent", "key", key)
			} else {
				logger.Warn(ctx, "Unknown channel format", "channel", msg.Channel)
				continue
			}

			// Only process 'set' operations for keyspace events
			if strings.Contains(msg.Channel, "__keyspace@0__:") && msg.Payload != "set" {
				logger.Debug(ctx, "Ignoring non-set operation", "operation", msg.Payload)
				continue
			}

			// Check if key has our environment prefix for keyevent notifications
			if strings.Contains(msg.Channel, "__keyevent@0__:") {
				// For keyevent notifications, make sure the key has our environment prefix
				expectedPrefix := fmt.Sprintf("%s:msg-", env)
				if !strings.HasPrefix(key, expectedPrefix) {
					logger.Debug(ctx, "Ignoring key from different environment",
						"key", key,
						"expected_prefix", expectedPrefix,
						"environment", env)
					continue
				}
			}

			// For keyspace events, we already filtered by pattern with the environment prefix

			// Only process keys with our message prefix pattern
			if !strings.Contains(key, ":msg-") {
				logger.Debug(ctx, "Ignoring key without msg- pattern", "key", key)
				continue
			}

			// Create a trace context for processing this key
			processCtx, span := tracer.Start(ctx, "redis.keyspace_event",
				trace.WithAttributes(attribute.String("redis.key", key)))

			// Get the value from Redis - we don't need to prefix the key here
			// since we already have the full prefixed key from the notification
			val, err := redisConn.Client().Get(processCtx, key).Result()
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "Failed to get value for key")
				logger.Error(processCtx, "Error getting value for key", "key", key, "error", err)
				span.End()
				continue
			}

			logger.Debug(processCtx, "Retrieved value for key", "key", key, "value", val)

			// Process the message
			err = processRedisMessage(processCtx, key, val)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "Failed to process message")
				logger.Error(processCtx, "Error processing message", "key", key, "error", err)
			} else {
				span.SetStatus(codes.Ok, "Message processed successfully")
				logger.Info(processCtx, "Successfully processed message", "key", key)
			}
			span.End()
		}
	}
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger.Debug(ctx, "Handling query request", "method", r.Method, "path", r.URL.Path)

	// Get ID from query parameters
	id := r.URL.Query().Get("id")
	if id == "" {
		logger.Warn(ctx, "Missing ID parameter in query")
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}
	tableName := getTableName()
	// Create query context with tracing
	ctx, span := tracer.Start(ctx, "db.query", trace.WithAttributes(
		attribute.String("message.id", id),
		attribute.String("db.table", tableName),
	))
	defer span.End()

	// Prepare query with environment-specific table
	query := fmt.Sprintf(`SELECT id, content, headers, created_at, trace_id, 
              ascii_terminal IS NOT NULL as has_ascii_terminal,
              ascii_file IS NOT NULL as has_ascii_file,
              ascii_html IS NOT NULL as has_ascii_html 
              FROM %s WHERE id = $1`, tableName)
	// Execute query
	row := postgresConn.QueryRowWithTracing(ctx, query, id)

	// Parse result
	var (
		messageID        string
		content          string
		headersJSON      string
		createdAt        time.Time
		traceID          string
		hasAsciiTerminal bool
		hasAsciiFile     bool
		hasAsciiHtml     bool
	)

	if err := row.Scan(&messageID, &content, &headersJSON, &createdAt, &traceID,
		&hasAsciiTerminal, &hasAsciiFile, &hasAsciiHtml); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Message not found or query error")
		logger.Error(ctx, "Database query error", "error", err, "id", id)
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	// Parse headers if present
	var headers map[string]string
	if headersJSON != "" && headersJSON != "{}" {
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			logger.Warn(ctx, "Failed to parse headers JSON", "error", err, "headers_json", headersJSON)
		}
	}

	// Build response
	response := map[string]interface{}{
		"id":        messageID,
		"content":   content,
		"headers":   headers,
		"timestamp": createdAt.Format(time.RFC3339),
		"trace_id":  traceID,
	}

	// Add ASCII art information
	asciiLinks := make(map[string]string)
	if hasAsciiTerminal {
		asciiLinks["terminal"] = fmt.Sprintf("/ascii/terminal?id=%s", id)
	}
	if hasAsciiFile {
		asciiLinks["file"] = fmt.Sprintf("/ascii/file?id=%s", id)
	}
	if hasAsciiHtml {
		asciiLinks["html"] = fmt.Sprintf("/ascii/html?id=%s", id)
	}

	// Only add the ascii_art field if at least one format is available
	if len(asciiLinks) > 0 {
		response["ascii_art"] = asciiLinks
	}

	// Return JSON response
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Error(ctx, "Error encoding response", "error", err)
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
		return
	}

	logger.Info(ctx, "Query completed successfully", "id", id)
}

// handleCleanup handles the /cleanup endpoint to delete test messages
func handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	logger.Debug(ctx, "Handling cleanup request", "method", r.Method, "path", r.URL.Path)

	// Get ID from query parameters
	id := r.URL.Query().Get("id")
	if id == "" {
		logger.Warn(ctx, "Missing ID parameter in cleanup request")
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}
	tableName := getTableName()
	// Create query context with tracing
	ctx, span := tracer.Start(ctx, "db.delete", trace.WithAttributes(
		attribute.String("message.id", id),
		attribute.String("db.table", tableName),
	))
	defer span.End()

	// Delete from database with environment-specific table
	result, err := postgresConn.ExecuteWithTracing(ctx, fmt.Sprintf("DELETE FROM %s WHERE id = $1", tableName), id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to delete message")
		logger.Error(ctx, "Database deletion error", "error", err, "id", id)
		http.Error(w, "Error deleting message", http.StatusInternalServerError)
		return
	}

	// Check if any rows were affected
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		logger.Warn(ctx, "No rows affected by cleanup", "id", id)
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	logger.Info(ctx, "Message deleted successfully", "id", id)
}

func handleTraces(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger.Debug(ctx, "Handling traces request", "method", r.Method, "path", r.URL.Path)

	// Get ID from query parameters
	id := r.URL.Query().Get("id")
	if id == "" {
		logger.Warn(ctx, "Missing ID parameter in traces request")
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}

	// Query trace information from the database
	tableName := getTableName()
	ctx, span := tracer.Start(ctx, "db.query_trace", trace.WithAttributes(
		attribute.String("message.id", id),
		attribute.String("db.table", tableName),
	))
	defer span.End()

	// Simple query to get trace_id with environment-specific table
	query := fmt.Sprintf(`SELECT trace_id FROM %s WHERE id = $1`, tableName)
	row := postgresConn.QueryRowWithTracing(ctx, query, id)

	var traceID string
	if err := row.Scan(&traceID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Trace information not found")
		logger.Error(ctx, "Error retrieving trace information", "error", err, "id", id)
		http.Error(w, "Trace information not found", http.StatusNotFound)
		return
	}

	// Return trace information
	response := map[string]string{
		"id":       id,
		"trace_id": traceID,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Error(ctx, "Error encoding trace response", "error", err)
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
		return
	}

	logger.Info(ctx, "Trace information retrieved successfully", "id", id, "trace_id", traceID)
}

// Initialize creates the necessary database tables with environment prefix
func initializeDatabase(ctx context.Context) error {
	tableName := getTableName()
	// Create messages table if it doesn't exist
	tableSchema := `
        id VARCHAR(255) PRIMARY KEY,
        content TEXT NOT NULL,
        source VARCHAR(255),
        headers JSONB,
        trace_id VARCHAR(255),
        ascii_terminal TEXT,
        ascii_file TEXT,
        ascii_html TEXT,
        created_at TIMESTAMPTZ DEFAULT NOW()
    `
	return postgresConn.EnsureTable(ctx, tableName, tableSchema)
}

func handleAsciiTerminal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger.Debug(ctx, "Handling ASCII terminal request", "method", r.Method, "path", r.URL.Path)

	// Get ID from query parameters
	id := r.URL.Query().Get("id")
	if id == "" {
		logger.Warn(ctx, "Missing ID parameter in ASCII terminal request")
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}

	// Create query context with tracing
	tableName := getTableName()
	ctx, span := tracer.Start(ctx, "db.ascii_terminal_query", trace.WithAttributes(
		attribute.String("message.id", id),
		attribute.String("db.table", tableName),
	))
	defer span.End()

	// Query the ASCII terminal content with environment-specific table
	query := fmt.Sprintf(`SELECT ascii_terminal FROM %s WHERE id = $1 AND ascii_terminal IS NOT NULL`, tableName)
	var asciiTerminal string

	err := postgresConn.QueryRowWithTracing(ctx, query, id).Scan(&asciiTerminal)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "ASCII terminal art not found")
		logger.Error(ctx, "Error retrieving ASCII terminal art", "error", err, "id", id)
		http.Error(w, "ASCII terminal art not found", http.StatusNotFound)
		return
	}

	// Set content type to text/plain
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(asciiTerminal))

	logger.Info(ctx, "ASCII terminal art retrieved successfully", "id", id)
}

// handleAsciiFile handles requests for file (plain text) ASCII art format
func handleAsciiFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger.Debug(ctx, "Handling ASCII file request", "method", r.Method, "path", r.URL.Path)

	// Get ID from query parameters
	id := r.URL.Query().Get("id")
	if id == "" {
		logger.Warn(ctx, "Missing ID parameter in ASCII file request")
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}

	// Create query context with tracing
	tableName := getTableName()
	ctx, span := tracer.Start(ctx, "db.ascii_file_query", trace.WithAttributes(
		attribute.String("message.id", id),
		attribute.String("db.table", tableName),
	))
	defer span.End()

	// Query the ASCII file content
	query := fmt.Sprintf(`SELECT ascii_file FROM %s WHERE id = $1 AND ascii_file IS NOT NULL`, tableName)
	var asciiFile string

	err := postgresConn.QueryRowWithTracing(ctx, query, id).Scan(&asciiFile)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "ASCII file art not found")
		logger.Error(ctx, "Error retrieving ASCII file art", "error", err, "id", id)
		http.Error(w, "ASCII file art not found", http.StatusNotFound)
		return
	}

	// Set content type to text/plain
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Set Content-Disposition header to suggest downloading as a file
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"ascii-%s.txt\"", id))
	w.Write([]byte(asciiFile))

	logger.Info(ctx, "ASCII file art retrieved successfully", "id", id)
}

// handleAsciiHtml handles requests for HTML ASCII art format
func handleAsciiHtml(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger.Debug(ctx, "Handling ASCII HTML request", "method", r.Method, "path", r.URL.Path)

	// Get ID from query parameters
	id := r.URL.Query().Get("id")
	if id == "" {
		logger.Warn(ctx, "Missing ID parameter in ASCII HTML request")
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}

	// Create query context with tracing
	tableName := getTableName()
	ctx, span := tracer.Start(ctx, "db.ascii_html_query", trace.WithAttributes(
		attribute.String("message.id", id),
		attribute.String("db.table", tableName),
	))
	defer span.End()

	// Query the ASCII HTML content
	query := fmt.Sprintf(`SELECT ascii_html FROM %s WHERE id = $1 AND ascii_html IS NOT NULL`, tableName)
	var asciiHtml string

	err := postgresConn.QueryRowWithTracing(ctx, query, id).Scan(&asciiHtml)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "ASCII HTML art not found")
		logger.Error(ctx, "Error retrieving ASCII HTML art", "error", err, "id", id)
		http.Error(w, "ASCII HTML art not found", http.StatusNotFound)
		return
	}

	// Set content type to HTML
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(asciiHtml))

	logger.Info(ctx, "ASCII HTML art retrieved successfully", "id", id)
}

// getTableName returns the environment-specific table name
func getTableName() string {
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev" // Default to dev if ENV not set
	}
	return fmt.Sprintf("%s_messages", env)
}

func main() {
	// Create context for the application
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize the logger
	logger = logging.NewLogger("writer",
		logging.WithMinLevel(logging.Info),
		logging.WithJSONFormat(true),
	)

	// Initialize the tracer
	tracerCfg := tracing.DefaultConfig("writer")
	tp, err := tracing.InitTracer(tracerCfg)
	if err != nil {
		logger.Fatal(ctx, "Failed to initialize tracer", "error", err)
	}
	defer func() {
		if err := tracing.ShutdownTracer(ctx, tp); err != nil {
			logger.Error(ctx, "Error shutting down tracer", "error", err)
		}
	}()

	// Initialize Redis connection
	redisConn = connections.NewRedisConnection(connections.DefaultRedisConfig())
	if err := redisConn.Connect(ctx); err != nil {
		logger.Fatal(ctx, "Failed to connect to Redis", "error", err)
	}
	defer func() {
		if err := redisConn.Close(); err != nil {
			logger.Error(ctx, "Error closing Redis connection", "error", err)
		}
	}()

	// Initialize PostgreSQL connection
	postgresConn = connections.NewPostgresConnection(connections.DefaultPostgresConfig())
	if err := postgresConn.Connect(ctx); err != nil {
		logger.Fatal(ctx, "Failed to connect to PostgreSQL", "error", err)
	}
	defer func() {
		if err := postgresConn.Close(); err != nil {
			logger.Error(ctx, "Error closing PostgreSQL connection", "error", err)
		}
	}()

	// Initialize database schema
	if err := initializeDatabase(ctx); err != nil {
		logger.Fatal(ctx, "Failed to initialize database schema", "error", err)
	}

	// Create a server with logging middleware
	srv := server.NewServer("writer",
		server.WithLogger(logger),
	)

	// Use middleware
	srv.UseMiddleware(server.LoggingMiddleware(logger))

	// Register handlers
	srv.HandleFunc("/query", handleQuery)
	srv.HandleFunc("/cleanup", handleCleanup)
	srv.HandleFunc("/traces", handleTraces)
	// Add new ASCII art endpoints
	srv.HandleFunc("/ascii/terminal", handleAsciiTerminal)
	srv.HandleFunc("/ascii/file", handleAsciiFile)
	srv.HandleFunc("/ascii/html", handleAsciiHtml)
	// Register health checks
	srv.RegisterHealthChecks(
		[]health.Checker{redisConn, postgresConn}, // Liveness checks
		[]health.Checker{redisConn, postgresConn}, // Readiness checks
	)

	// Start Redis subscription in a goroutine
	go subscribeToRedisChanges(ctx)

	// Set up signal handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start server in a goroutine
	go func() {
		logger.Info(ctx, "Starting writer service", "port", 8080)
		if err := srv.Start(); err != nil {
			logger.Fatal(ctx, "Server failed", "error", err)
		}
	}()

	// Wait for termination signal
	<-stop

	// Shut down gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
	defer shutdownCancel()

	logger.Info(shutdownCtx, "Shutting down writer service")
}
