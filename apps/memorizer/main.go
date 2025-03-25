package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fullstack-pw/shared/connections"
	"github.com/fullstack-pw/shared/health"
	"github.com/fullstack-pw/shared/logging"
	"github.com/fullstack-pw/shared/server"
	"github.com/fullstack-pw/shared/tracing"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Message represents the data structure coming from the queue
type Message struct {
	ID      string            `json:"id,omitempty"`
	Content string            `json:"content"`
	Headers map[string]string `json:"headers,omitempty"`
}

var (
	// Global variables
	natsConn  *connections.NATSConnection
	redisConn *connections.RedisConnection
	logger    *logging.Logger
	tracer    = tracing.GetTracer("memorizer")
)

// storeInRedis stores a message in Redis with tracing
func storeInRedis(ctx context.Context, id string, content string) error {
	// Create a child span for the Redis operation
	ctx, span := tracer.Start(ctx, "redis.store", trace.WithAttributes(
		attribute.String("db.system", "redis"),
		attribute.String("db.operation", "set"),
		attribute.String("message.id", id),
	))
	defer span.End()

	span.SetAttributes(
		attribute.String("message.content", content),
	)

	logger.Debug(ctx, "Storing message in Redis", "id", id)

	// Generate a key for Redis storage
	// Using message ID if provided, otherwise generate a timestamp-based key
	key := id
	if key == "" {
		key = fmt.Sprintf("msg:%d", time.Now().UnixNano())
		span.SetAttributes(attribute.String("generated.key", key))
		logger.Debug(ctx, "Generated key for message without ID", "key", key)
	}

	// Create a message with trace context to store in Redis
	redisMsg := Message{
		ID:      id,
		Content: content,
		Headers: make(map[string]string),
	}

	// Inject current trace context into Redis message headers
	tracing.InjectTraceContext(ctx, redisMsg.Headers)

	// Convert to JSON for storage
	msgJSON, err := json.Marshal(redisMsg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to marshal message with trace context")
		logger.Error(ctx, "Failed to marshal message", "error", err)
		return fmt.Errorf("failed to marshal message with trace context: %w", err)
	}

	// Store the JSON in Redis instead of raw content
	expiration := 24 * time.Hour // Keep messages for 24 hours
	err = redisConn.SetWithTracing(ctx, key, string(msgJSON), expiration)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to store message in Redis")
		logger.Error(ctx, "Failed to store message in Redis", "error", err, "key", key)
		return fmt.Errorf("failed to store message in Redis: %w", err)
	}

	span.SetStatus(codes.Ok, "Message stored successfully")
	logger.Info(ctx, "Message stored in Redis", "key", key, "id", id)
	return nil
}

// handleMessage processes a message from NATS with tracing
func handleMessage(msg *nats.Msg) {
	// Extract trace context from message if available
	parentCtx := context.Background()

	// Try to unmarshal the message to extract headers
	var message Message
	if err := json.Unmarshal(msg.Data, &message); err == nil && message.Headers != nil {
		// Extract trace context from headers
		parentCtx = tracing.ExtractTraceContext(context.Background(), message.Headers)
		logger.Debug(parentCtx, "Extracted trace context from message headers")
	} else {
		logger.Debug(parentCtx, "No trace context found in message, starting new trace")
	}

	// Now start the span with the extracted context
	ctx, span := tracer.Start(parentCtx, "nats.message.process")
	defer span.End()

	logger.Debug(ctx, "Received message", "data", string(msg.Data), "subject", msg.Subject)

	// Set attributes for the message
	span.SetAttributes(
		attribute.String("message.subject", msg.Subject),
		attribute.Int("message.size", len(msg.Data)),
	)

	// Parse message
	if err := json.Unmarshal(msg.Data, &message); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to parse message")
		logger.Error(ctx, "Error parsing message", "error", err, "data", string(msg.Data))
		return
	}

	// Extract trace context from headers if available
	if message.Headers != nil {
		span.SetAttributes(attribute.Int("headers.count", len(message.Headers)))
		for k, v := range message.Headers {
			span.SetAttributes(attribute.String("header."+k, v))
		}
	}

	// Store message in Redis
	if err := storeInRedis(ctx, message.ID, message.Content); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to store message")
		logger.Error(ctx, "Error storing message", "error", err, "id", message.ID)
		return
	}

	span.SetStatus(codes.Ok, "Message processed successfully")
	logger.Info(ctx, "Message processed successfully", "id", message.ID)
}

// handleStatus handles the /status endpoint
func handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger.Debug(ctx, "Handling status request", "method", r.Method, "path", r.URL.Path)

	// Get message ID from query parameters
	id := r.URL.Query().Get("id")
	if id == "" {
		logger.Warn(ctx, "Missing ID parameter in status request")
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}

	// Check if the message exists in Redis
	ctx, span := tracer.Start(ctx, "redis.get")
	defer span.End()

	span.SetAttributes(attribute.String("message.id", id))

	// Check if the message exists in Redis
	exists, err := redisConn.Client().Exists(ctx, id).Result()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Redis query failed")
		logger.Error(ctx, "Error checking if message exists", "error", err, "id", id)
		http.Error(w, "Error checking message status", http.StatusInternalServerError)
		return
	}

	// Build response
	response := map[string]interface{}{
		"id":        id,
		"processed": exists > 0,
	}

	// Return JSON response
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Error(ctx, "Error encoding response", "error", err)
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
		return
	}

	logger.Info(ctx, "Status check completed", "id", id, "exists", exists > 0)
}

func main() {
	// Create context for the application
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize the logger
	logger = logging.NewLogger("memorizer",
		logging.WithMinLevel(logging.Debug),
		logging.WithJSONFormat(true),
	)

	// Initialize the tracer
	tracerCfg := tracing.DefaultConfig("memorizer")
	tp, err := tracing.InitTracer(tracerCfg)
	if err != nil {
		logger.Fatal(ctx, "Failed to initialize tracer", "error", err)
	}
	defer func() {
		if err := tracing.ShutdownTracer(ctx, tp); err != nil {
			logger.Error(ctx, "Error shutting down tracer", "error", err)
		}
	}()

	// Initialize NATS connection
	natsConn = connections.NewNATSConnection()
	if err := natsConn.Connect(ctx); err != nil {
		logger.Fatal(ctx, "Failed to connect to NATS", "error", err)
	}
	defer func() {
		natsConn.Close()
		logger.Info(ctx, "NATS connection closed")
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

	// Subscribe to NATS queues
	queueNames := os.Getenv("QUEUE_NAMES")
	if queueNames == "" {
		queueNames = "default"
	}

	// Subscribe to the specified queue
	logger.Info(ctx, "Subscribing to NATS queue", "queue", queueNames)
	sub, err := natsConn.SubscribeWithTracing(queueNames, handleMessage)
	if err != nil {
		logger.Fatal(ctx, "Failed to subscribe to NATS queue", "error", err, "queue", queueNames)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			logger.Error(ctx, "Error unsubscribing from NATS", "error", err)
		}
	}()

	// Create a server with logging middleware
	srv := server.NewServer("memorizer",
		server.WithLogger(logger),
	)

	// Use middleware
	srv.UseMiddleware(server.LoggingMiddleware(logger))

	// Register handlers
	srv.HandleFunc("/status", handleStatus)

	// Register health checks
	srv.RegisterHealthChecks(
		[]health.Checker{natsConn, redisConn}, // Liveness checks
		[]health.Checker{natsConn, redisConn}, // Readiness checks
	)

	// Set up signal handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start server in a goroutine
	go func() {
		logger.Info(ctx, "Starting memorizer service", "port", 8080)
		if err := srv.Start(); err != nil {
			logger.Fatal(ctx, "Server failed", "error", err)
		}
	}()

	// Wait for termination signal
	<-stop

	// Shut down gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
	defer shutdownCancel()

	logger.Info(shutdownCtx, "Shutting down memorizer service")
}
