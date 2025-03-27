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

// writeToPostgres writes message data to PostgreSQL with tracing
func writeToPostgres(ctx context.Context, id string, content string, headers map[string]string) error {
	// Create a child span for the database operation
	ctx, span := tracer.Start(ctx, "pg.write_message", trace.WithAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation", "insert"),
		attribute.String("message.id", id),
	))
	defer span.End()

	logger.Debug(ctx, "Starting PostgreSQL write", "id", id)

	span.SetAttributes(
		attribute.String("message.content", content),
		attribute.Int("headers.count", len(headers)),
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
	query := `
        INSERT INTO messages (id, content, source, headers, trace_id)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (id) DO UPDATE
        SET content = EXCLUDED.content,
            headers = EXCLUDED.headers,
            trace_id = EXCLUDED.trace_id,
            created_at = NOW()
    `
	logger.Debug(ctx, "Executing query",
		"query", query,
		"id", id,
		"content", content,
		"source", "redis",
		"headers", headersJSON,
		"trace_id", traceID,
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
	_, err := postgresConn.ExecuteWithTracing(ctx, query, id, content, "redis", headersJSON, traceID)
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
	// Create a span for processing the Redis message
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
		id = strings.TrimPrefix(key, "msg:")
	} else {
		// Valid JSON message
		logger.Debug(ctx, "Successfully parsed JSON message", "message", message)
		span.SetAttributes(attribute.Bool("message.is_json", true))
		content = message.Content
		headers = message.Headers
		id = message.ID
		if id == "" {
			id = strings.TrimPrefix(key, "msg:")
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
	// Write to PostgreSQL
	logger.Debug(ctx, "Writing to PostgreSQL", "id", id, "content", content)
	err = writeToPostgres(ctx, id, content, headers)
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

	// Set up keyspace notifications if possible
	err := redisConn.SetupRedisNotifications(ctx)
	if err != nil {
		logger.Error(ctx, "Failed to enable Redis keyspace notifications, falling back to polling", "error", err)
		// TODO: Implement polling fallback if needed
		return
	}

	// Subscribe to keyspace notifications
	patterns := []string{"__keyevent@0__:set", "__keyevent@0__:hset", "__keyspace@0__:msg-*"}
	pubsub := redisConn.SubscribeToKeyspace(ctx, patterns...)
	defer func() {
		if err := pubsub.Close(); err != nil {
			logger.Error(ctx, "Error closing Redis pubsub", "error", err)
		}
	}()

	logger.Info(ctx, "Listening for Redis keyspace notifications")

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

			// Only process keys with our prefix
			if !strings.HasPrefix(key, "msg-") {
				logger.Debug(ctx, "Ignoring key without msg: prefix", "key", key)
				continue
			}

			// Create a trace context for processing this key
			processCtx, span := tracer.Start(ctx, "redis.keyspace_event",
				trace.WithAttributes(attribute.String("redis.key", key)))

			// Get the value from Redis
			val, err := redisConn.GetWithTracing(processCtx, key)
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

// handleQuery handles the /query endpoint to retrieve messages from the database
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

	// Create query context with tracing
	ctx, span := tracer.Start(ctx, "db.query", trace.WithAttributes(
		attribute.String("message.id", id),
	))
	defer span.End()

	// Prepare query
	query := `SELECT id, content, headers, created_at, trace_id FROM messages WHERE id = $1`

	// Execute query
	row := postgresConn.QueryRowWithTracing(ctx, query, id)

	// Parse result
	var (
		messageID   string
		content     string
		headersJSON string
		createdAt   time.Time
		traceID     string
	)

	if err := row.Scan(&messageID, &content, &headersJSON, &createdAt, &traceID); err != nil {
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

	// Create query context with tracing
	ctx, span := tracer.Start(ctx, "db.delete", trace.WithAttributes(
		attribute.String("message.id", id),
	))
	defer span.End()

	// Delete from database
	result, err := postgresConn.ExecuteWithTracing(ctx, "DELETE FROM messages WHERE id = $1", id)
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

// handleTraces retrieves and returns trace information for a message
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
	ctx, span := tracer.Start(ctx, "db.query_trace", trace.WithAttributes(
		attribute.String("message.id", id),
	))
	defer span.End()

	// Simple query to get trace_id
	query := `SELECT trace_id FROM messages WHERE id = $1`
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

// Initialize creates the necessary database tables
func initializeDatabase(ctx context.Context) error {
	// Create messages table if it doesn't exist
	tableSchema := `
		id VARCHAR(255) PRIMARY KEY,
		content TEXT NOT NULL,
		source VARCHAR(255),
		headers JSONB,
		trace_id VARCHAR(255),
		created_at TIMESTAMPTZ DEFAULT NOW()
	`
	return postgresConn.EnsureTable(ctx, "messages", tableSchema)
}

func main() {
	// Create context for the application
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize the logger
	logger = logging.NewLogger("writer",
		logging.WithMinLevel(logging.Debug),
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
