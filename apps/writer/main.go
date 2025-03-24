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

	"github.com/go-redis/redis/v8"
	_ "github.com/lib/pq" // PostgreSQL driver
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"

	"database/sql"
)

var (
	redisClient *redis.Client
	postgresDB  *sql.DB
	tracer      trace.Tracer
)

// RedisMessage represents the data structure coming from Redis
type RedisMessage struct {
	ID      string            `json:"id,omitempty"`
	Content string            `json:"content"`
	Headers map[string]string `json:"headers,omitempty"`
}

// initTracer initializes the OpenTelemetry tracer
func initTracer() (*sdktrace.TracerProvider, error) {
	fmt.Println("Initializing OpenTelemetry tracer...")

	// Get OpenTelemetry Collector endpoint from env vars or use default
	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otelEndpoint == "" {
		otelEndpoint = "opentelemetry-collector.observability.svc.cluster.local:4317"
	}
	fmt.Printf("Using OpenTelemetry endpoint: %s\n", otelEndpoint)

	// Add check for local development
	skipTracing := os.Getenv("SKIP_TRACING")
	if skipTracing == "true" {
		fmt.Println("Skipping actual tracing setup, using no-op tracer")
		// Return a no-op tracer for local development
		return sdktrace.NewTracerProvider(), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Configure the exporter
	fmt.Println("Creating OTLP trace exporter...")
	exporter, err := otlptracegrpc.New(
		ctx,
		otlptracegrpc.WithEndpoint(otelEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}
	fmt.Println("Trace exporter created successfully")

	// Configure resource attributes
	fmt.Println("Configuring resource attributes...")
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("writer"),
			semconv.ServiceVersionKey.String("0.1.0"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create trace provider
	fmt.Println("Creating trace provider...")
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	fmt.Println("Trace provider initialized successfully")

	return tp, nil
}

// initRedis initializes the Redis client connection
func initRedis() error {
	fmt.Println("Initializing Redis connection...")

	// Get Redis connection details from environment
	redisHost := os.Getenv("REDIS_HOST")
	redisPort := os.Getenv("REDIS_PORT")
	redisPassword := os.Getenv("REDIS_PASSWORD")

	// Set defaults if not provided
	if redisHost == "" {
		redisHost = "redis.fullstack.pw"
	}
	if redisPort == "" {
		redisPort = "6379"
	}

	redisAddr := fmt.Sprintf("%s:%s", redisHost, redisPort)
	fmt.Printf("Connecting to Redis at %s\n", redisAddr)

	// Create Redis client
	redisClient = redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPassword,
		DB:       0, // use default DB
	})

	// Test the connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Multiple connection attempts with exponential backoff
	maxRetries := 5
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		fmt.Printf("Attempt %d: Testing Redis connection\n", attempt)

		_, err = redisClient.Ping(ctx).Result()
		if err == nil {
			fmt.Println("Successfully connected to Redis")
			return nil
		}

		fmt.Printf("Redis connection error: %v\n", err)
		if attempt == maxRetries {
			return fmt.Errorf("failed to connect to Redis after %d attempts: %w", maxRetries, err)
		}
		time.Sleep(time.Duration(attempt*2) * time.Second)
	}

	return fmt.Errorf("exhausted all connection attempts to Redis")
}

// initPostgres initializes the PostgreSQL client connection
func initPostgres() error {
	fmt.Println("Initializing PostgreSQL connection...")

	// Get DB connection details from environment
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")

	// Set defaults if not provided
	if dbHost == "" {
		dbHost = "postgres.fullstack.pw"
	}
	if dbPort == "" {
		dbPort = "5432"
	}
	if dbName == "" {
		dbName = "postgres"
	}
	if dbUser == "" {
		dbUser = "admin"
	}

	// Construct connection string
	connStr := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		dbHost, dbPort, dbUser, dbPassword, dbName,
	)

	// Multiple connection attempts with exponential backoff
	maxRetries := 5
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		fmt.Printf("Attempt %d: Connecting to PostgreSQL at %s:%s/%s\n",
			attempt, dbHost, dbPort, dbName)

		// Attempt database connection
		postgresDB, err = sql.Open("postgres", connStr)
		if err != nil {
			fmt.Printf("Connection configuration error: %v\n", err)
			if attempt == maxRetries {
				return fmt.Errorf("failed to configure database connection after %d attempts", maxRetries)
			}
			time.Sleep(time.Duration(attempt*2) * time.Second)
			continue
		}

		// Test the connection
		err = postgresDB.Ping()
		if err != nil {
			fmt.Printf("Database ping failed: %v\n", err)
			_ = postgresDB.Close()
			if attempt == maxRetries {
				return fmt.Errorf("failed to ping database after %d attempts", maxRetries)
			}
			time.Sleep(time.Duration(attempt*2) * time.Second)
			continue
		}

		// Successfully connected
		fmt.Printf("Successfully connected to PostgreSQL at %s:%s/%s\n",
			dbHost, dbPort, dbName)

		// Create messages table if it doesn't exist
		_, err = postgresDB.Exec(`
			CREATE TABLE IF NOT EXISTS messages (
				id VARCHAR(255) PRIMARY KEY,
				content TEXT NOT NULL,
				source VARCHAR(255),
				headers JSONB,
				trace_id VARCHAR(255),
				span_id VARCHAR(255),
				created_at TIMESTAMPTZ DEFAULT NOW()
			)
		`)
		if err != nil {
			fmt.Printf("Warning: Failed to ensure messages table: %v\n", err)
			return err
		}

		fmt.Println("Database table 'messages' verified/created successfully")
		return nil
	}

	return fmt.Errorf("exhausted all connection attempts to PostgreSQL")
}

func getTraceID(ctx context.Context) string {
	if span := trace.SpanFromContext(ctx); span != nil {
		return span.SpanContext().TraceID().String()
	}
	return "unknown"
}

// writeToPostgres writes message data to PostgreSQL with tracing
func writeToPostgres(ctx context.Context, id string, content string, headers map[string]string) error {
	// Create a child span for the database operation
	ctx, span := tracer.Start(ctx, "pg.write_message", trace.WithAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation", "insert"),
		attribute.String("message.id", id),
	))
	defer span.End()

	fmt.Printf("TraceID=%s Starting PostgreSQL write for id: %s\n", getTraceID(ctx), id)

	span.SetAttributes(
		attribute.String("message.content", content),
		attribute.Int("headers.count", len(headers)),
	)
	traceID := getTraceID(ctx)
	span.SetAttributes(attribute.String("trace.id", traceID))

	fmt.Printf("TraceID=%s Starting PostgreSQL write for id: %s\n", traceID, id)

	// Prepare headers as JSONB
	headersJSON := "{}"
	if len(headers) > 0 {
		headersBytes, err := json.Marshal(headers)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "Failed to serialize headers")
			fmt.Printf("Failed to serialize headers: %v\n", err)
			return fmt.Errorf("failed to serialize headers: %w", err)
		}
		headersJSON = string(headersBytes)
		fmt.Printf("Serialized headers: %s\n", headersJSON)
	}

	// If ID is empty, generate one
	if id == "" {
		id = fmt.Sprintf("gen-%d", time.Now().UnixNano())
		span.SetAttributes(attribute.String("generated.id", id))
		fmt.Printf("Generated ID: %s\n", id)
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
	fmt.Printf("Executing query: %s with values id=%s, content=%s, source=redis, headers=%s\n",
		query, id, content, headersJSON)

	// Execute the query
	_, err := postgresDB.ExecContext(ctx, query, id, content, "redis", headersJSON, traceID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to insert message")
		fmt.Printf("Database error: %v\n", err)
		return fmt.Errorf("failed to insert message: %w", err)
	}

	span.SetStatus(codes.Ok, "Message inserted successfully")
	fmt.Printf("TraceID=%s Message with ID '%s' successfully written to PostgreSQL\n",
		getTraceID(ctx), id)
	return nil
}

// processRedisMessage processes a message from Redis with tracing
func processRedisMessage(ctx context.Context, key string, value string) error {
	// Create a span for processing the Redis message
	var parentCtx = ctx
	var message RedisMessage

	if err := json.Unmarshal([]byte(value), &message); err == nil && message.Headers != nil {
		// Extract trace context from headers if present
		carrier := propagation.MapCarrier(message.Headers)
		extractedCtx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
		if extractedCtx != nil {
			parentCtx = extractedCtx
			fmt.Printf("Extracted trace context from Redis message\n")
		}
	}

	// Start new span as child of the extracted context
	ctx, span := tracer.Start(parentCtx, "redis.process_message", trace.WithAttributes(
		attribute.String("redis.key", key),
	))
	defer span.End()

	fmt.Printf("TraceID=%s Processing Redis message with key: %s, value: %s\n",
		getTraceID(ctx), key, value)

	// Try to parse the message as JSON
	var headers map[string]string
	var content string
	var id string

	err := json.Unmarshal([]byte(value), &message)
	if err != nil {
		// If not valid JSON, use the raw value as content
		fmt.Printf("Message is not valid JSON, using raw value: %v\n", err)
		span.SetAttributes(attribute.Bool("message.is_json", false))
		content = value
		id = strings.TrimPrefix(key, "msg:")
	} else {
		// Valid JSON message
		fmt.Printf("Successfully parsed JSON message: %+v\n", message)
		span.SetAttributes(attribute.Bool("message.is_json", true))
		content = message.Content
		headers = message.Headers
		id = message.ID
		if id == "" {
			id = strings.TrimPrefix(key, "msg:")
		}
	}

	// Write to PostgreSQL
	fmt.Printf("Writing to PostgreSQL with id: %s, content: %s\n", id, content)
	err = writeToPostgres(ctx, id, content, headers)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to write to PostgreSQL")
		return fmt.Errorf("failed to write to PostgreSQL: %w", err)
	}
	// Delete the key from Redis after successful processing
	err = redisClient.Del(ctx, key).Err()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to delete key from Redis")
		fmt.Printf("TraceID=%s Warning: Failed to delete key '%s' from Redis: %v\n",
			getTraceID(ctx), key, err)
		// Continue despite this error
	} else {
		fmt.Printf("TraceID=%s Key '%s' deleted from Redis after processing\n",
			getTraceID(ctx), key)
	}

	span.SetStatus(codes.Ok, "Message processed successfully")
	return nil
}

// pollRedis periodically polls Redis for new messages
// Replace the pollRedis function with this
func subscribeToRedisChanges(ctx context.Context) {

	// Set up a Redis subscription to keyspace notifications
	pubsub := redisClient.PSubscribe(ctx, "__keyevent@0__:set", "__keyevent@0__:hset", "__keyspace@0__:msg:*")
	defer func() {
		if err := pubsub.Close(); err != nil {
			fmt.Printf("Error closing pubsub: %v\n", err)
		}
	}()

	// Enable keyspace notifications on the Redis server (only needs to be done once)
	err := redisClient.ConfigSet(ctx, "notify-keyspace-events", "KA").Err()
	if err != nil {
		fmt.Printf("Warning: Failed to enable Redis keyspace notifications: %v\n", err)
		fmt.Println("Falling back to polling method...")

		// Fall back to polling if keyspace notifications can't be enabled
		// pollRedis(ctx, 2*time.Second)
		return
	}
	fmt.Println("Listening for Redis keyspace notifications...")

	// Process messages from the subscription channel
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			fmt.Printf("Received message from Redis channel: %v (ok: %v)\n", msg, ok)

			// Extract the actual key from the channel pattern
			// The channel will be in format "__keyspace@0__:msg:12345" or "__keyevent@0__:set"
			var key string

			if strings.Contains(msg.Channel, "__keyspace@0__:") {
				// For keyspace events, the key is in the channel name
				key = strings.TrimPrefix(msg.Channel, "__keyspace@0__:")
				fmt.Printf("Extracted key from keyspace event: %s\n", key)
			} else if strings.Contains(msg.Channel, "__keyevent@0__:") {
				// For keyevent notifications, the key is in the payload
				key = msg.Payload
				fmt.Printf("Extracted key from keyevent: %s\n", key)
			} else {
				fmt.Printf("Unknown channel format: %s\n", msg.Channel)
				continue
			}

			// Only process 'set' operations for keyspace events
			if strings.Contains(msg.Channel, "__keyspace@0__:") && msg.Payload != "set" {
				fmt.Printf("Ignoring non-set operation: %s\n", msg.Payload)
				continue
			}

			// Only process keys with our prefix
			if !strings.HasPrefix(key, "msg:") {
				fmt.Printf("Ignoring key without msg: prefix: %s\n", key)
				continue
			}

			// Create a trace context for processing this key
			processCtx, span := tracer.Start(ctx, "redis.keyspace_event",
				trace.WithAttributes(attribute.String("redis.key", key)))

			// Add debug output
			fmt.Printf("Processing key: %s\n", key)

			// Get the value from Redis
			val, err := redisClient.Get(processCtx, key).Result()
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "Failed to get value for key")
				fmt.Printf("Error getting value for key '%s': %v\n", key, err)
				span.End()
				continue
			}

			fmt.Printf("Retrieved value for key %s: %s\n", key, val)

			// Process the message
			err = processRedisMessage(processCtx, key, val)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "Failed to process message")
				fmt.Printf("Error processing message for key '%s': %v\n", key, err)
			} else {
				span.SetStatus(codes.Ok, "Message processed successfully")
				fmt.Printf("Successfully processed message for key '%s'\n", key)
			}
			span.End()
		}
	}
}

func main() {
	fmt.Println("Starting writer application...")

	// Initialize the tracer with retry logic
	var tp *sdktrace.TracerProvider
	var err error
	for attempts := 0; attempts < 3; attempts++ {
		tp, err = initTracer()
		if err == nil {
			break
		}
		fmt.Printf("Attempt %d: Failed to initialize tracer: %v\n", attempts+1, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		fmt.Printf("All attempts to initialize tracer failed: %v\n", err)
		os.Exit(1)
	}

	// Initialize Redis connection with retry logic
	for attempts := 0; attempts < 3; attempts++ {
		err = initRedis()
		if err == nil {
			break
		}
		fmt.Printf("Attempt %d: Failed to initialize Redis: %v\n", attempts+1, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		fmt.Printf("All attempts to initialize Redis failed: %v\n", err)
		os.Exit(1)
	}

	// Initialize PostgreSQL connection with retry logic
	for attempts := 0; attempts < 3; attempts++ {
		err = initPostgres()
		if err == nil {
			break
		}
		fmt.Printf("Attempt %d: Failed to initialize PostgreSQL: %v\n", attempts+1, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		fmt.Printf("All attempts to initialize PostgreSQL failed: %v\n", err)
		os.Exit(1)
	}

	// Create a tracer
	tracer = otel.Tracer("writer")
	fmt.Println("Created tracer instance")

	// Set up context with cancellation for the application
	ctx, cancel := context.WithCancel(context.Background())

	// Ensure all resources are cleaned up properly
	defer func() {
		// Cancel the context
		cancel()

		// Shut down tracer provider
		fmt.Println("Shutting down tracer provider...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			fmt.Printf("Error shutting down tracer provider: %v\n", err)
		}
		fmt.Println("Tracer provider shut down successfully")

		// Close Redis connection
		if redisClient != nil {
			fmt.Println("Closing Redis connection...")
			if err := redisClient.Close(); err != nil {
				fmt.Printf("Error closing Redis connection: %v\n", err)
			}
			fmt.Println("Redis connection closed successfully")
		}

		// Close PostgreSQL connection
		if postgresDB != nil {
			fmt.Println("Closing PostgreSQL connection...")
			if err := postgresDB.Close(); err != nil {
				fmt.Printf("Error closing PostgreSQL connection: %v\n", err)
			}
			fmt.Println("PostgreSQL connection closed successfully")
		}
	}()

	// Start Redis polling in a goroutine
	go subscribeToRedisChanges(ctx)

	// Set up HTTP server for health checks
	setupHealthEndpoints()

	// Wait for interrupt signal to gracefully terminate the service
	fmt.Println("Writer service is running. Press CTRL+C to exit.")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("Shutting down gracefully...")
	// Context cancellation will stop the polling
}

// Query a message from PostgreSQL by ID
func queryMessageByID(ctx context.Context, id string) (map[string]interface{}, error) {
	ctx, span := tracer.Start(ctx, "pg.query_message_by_id")
	defer span.End()

	span.SetAttributes(attribute.String("message.id", id))
	fmt.Printf("TraceID=%s Querying message with ID: %s\n", getTraceID(ctx), id)

	query := `
        SELECT id, content, source, headers, created_at, 
               trace_id, span_id 
        FROM messages
        WHERE id = $1
    `

	var (
		messageID   string
		content     string
		source      string
		headersJSON string
		createdAt   time.Time
		traceID     sql.NullString
		spanID      sql.NullString
	)

	err := postgresDB.QueryRowContext(ctx, query, id).Scan(
		&messageID, &content, &source, &headersJSON, &createdAt,
		&traceID, &spanID,
	)

	if err == sql.ErrNoRows {
		span.SetStatus(codes.Error, "Message not found")
		return nil, fmt.Errorf("message with ID %s not found", id)
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Database query error")
		return nil, fmt.Errorf("database error: %w", err)
	}

	// Parse headers JSON if present
	var headers map[string]string
	if headersJSON != "" && headersJSON != "{}" {
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			span.RecordError(err)
			fmt.Printf("Warning: failed to parse headers JSON: %v\n", err)
		}
	}

	result := map[string]interface{}{
		"id":         messageID,
		"content":    content,
		"source":     source,
		"created_at": createdAt.Format(time.RFC3339),
	}

	if headers != nil {
		result["headers"] = headers
	}

	if traceID.Valid {
		result["trace_id"] = traceID.String
	}

	if spanID.Valid {
		result["span_id"] = spanID.String
	}

	span.SetStatus(codes.Ok, "Message retrieved successfully")
	return result, nil
}

// Delete a message from PostgreSQL by ID
func deleteMessageByID(ctx context.Context, id string) error {
	ctx, span := tracer.Start(ctx, "pg.delete_message_by_id")
	defer span.End()

	span.SetAttributes(attribute.String("message.id", id))
	fmt.Printf("TraceID=%s Deleting message with ID: %s\n", getTraceID(ctx), id)

	query := "DELETE FROM messages WHERE id = $1"
	result, err := postgresDB.ExecContext(ctx, query, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to delete message")
		return fmt.Errorf("failed to delete message: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		span.RecordError(err)
		fmt.Printf("Warning: couldn't get rows affected: %v\n", err)
	} else if rowsAffected == 0 {
		span.SetStatus(codes.Error, "Message not found")
		return fmt.Errorf("message with ID %s not found", id)
	}

	span.SetStatus(codes.Ok, "Message deleted successfully")
	return nil
}

// Get trace context for a message
func getTraceInfoByID(ctx context.Context, id string) (map[string]interface{}, error) {
	ctx, span := tracer.Start(ctx, "pg.get_trace_info_by_id")
	defer span.End()

	span.SetAttributes(attribute.String("message.id", id))
	fmt.Printf("TraceID=%s Getting trace info for message ID: %s\n", getTraceID(ctx), id)

	query := `
        SELECT trace_id, span_id, headers 
        FROM messages 
        WHERE id = $1
    `

	var traceID sql.NullString
	var spanID sql.NullString
	var headersJSON string

	err := postgresDB.QueryRowContext(ctx, query, id).Scan(&traceID, &spanID, &headersJSON)
	if err == sql.ErrNoRows {
		span.SetStatus(codes.Error, "Message not found")
		return nil, fmt.Errorf("message with ID %s not found", id)
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Database query error")
		return nil, fmt.Errorf("database error: %w", err)
	}

	// Check if we have trace information
	result := map[string]interface{}{
		"id": id,
	}

	if traceID.Valid {
		result["trace_id"] = traceID.String
	}

	if spanID.Valid {
		result["span_id"] = spanID.String
	}

	// Parse headers JSON if present to check for trace context
	var propagated bool
	if headersJSON != "" && headersJSON != "{}" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			span.RecordError(err)
			fmt.Printf("Warning: failed to parse headers JSON: %v\n", err)
		} else {
			// Check for traceparent header
			if _, ok := headers["traceparent"]; ok {
				propagated = true
			}
		}
	}

	result["propagated"] = propagated

	span.SetStatus(codes.Ok, "Trace info retrieved successfully")
	return result, nil
}

// setupHealthEndpoints sets up a small HTTP server for health checks
func setupHealthEndpoints() {
	go func() {
		// Add query endpoint to fetch messages from PostgreSQL
		http.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling query request: %s %s\n", r.Method, r.URL.Path)

			// Only allow GET requests
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, err := fmt.Fprintf(w, "Method not allowed")
				if err != nil {
					fmt.Printf("Error writing response: %v\n", err)
				}
				return
			}

			// Get message ID from query parameter
			messageID := r.URL.Query().Get("id")
			if messageID == "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, "Missing id parameter")
				return
			}

			// Create a trace span for this operation
			ctx, span := tracer.Start(r.Context(), "query-endpoint")
			defer span.End()
			span.SetAttributes(attribute.String("message.id", messageID))

			// Query the message from PostgreSQL
			message, err := queryMessageByID(ctx, messageID)
			if err != nil {
				status := http.StatusInternalServerError
				if err.Error() == fmt.Sprintf("message with ID %s not found", messageID) {
					status = http.StatusNotFound
				}

				span.RecordError(err)
				span.SetStatus(codes.Error, "Failed to query message")
				w.WriteHeader(status)
				fmt.Fprintf(w, "Error: %v", err)
				return
			}

			// Return the message as JSON
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(message); err != nil {
				span.RecordError(err)
				fmt.Printf("Error encoding response: %v\n", err)
			}

			span.SetStatus(codes.Ok, "Message retrieved successfully")
			fmt.Printf("Successfully returned message with ID: %s\n", messageID)
		})

		// Add traces endpoint to verify trace context propagation
		http.HandleFunc("/traces", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling traces request: %s %s\n", r.Method, r.URL.Path)

			// Only allow GET requests
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, err := fmt.Fprintf(w, "Method not allowed")
				if err != nil {
					fmt.Printf("Error writing response: %v\n", err)
				}
				return
			}

			// Get message ID from query parameter
			messageID := r.URL.Query().Get("id")
			if messageID == "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, "Missing id parameter")
				return
			}

			// Create a trace span for this operation
			ctx, span := tracer.Start(r.Context(), "traces-endpoint")
			defer span.End()
			span.SetAttributes(attribute.String("message.id", messageID))

			// Get trace info for the message
			traceInfo, err := getTraceInfoByID(ctx, messageID)
			if err != nil {
				status := http.StatusInternalServerError
				if err.Error() == fmt.Sprintf("message with ID %s not found", messageID) {
					status = http.StatusNotFound
				}

				span.RecordError(err)
				span.SetStatus(codes.Error, "Failed to get trace info")
				w.WriteHeader(status)
				fmt.Fprintf(w, "Error: %v", err)
				return
			}

			// Return the trace info as JSON
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(traceInfo); err != nil {
				span.RecordError(err)
				fmt.Printf("Error encoding response: %v\n", err)
			}

			span.SetStatus(codes.Ok, "Trace info retrieved successfully")
			fmt.Printf("Successfully returned trace info for message with ID: %s\n", messageID)
		})

		// Add cleanup endpoint to delete test messages
		http.HandleFunc("/cleanup", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling cleanup request: %s %s\n", r.Method, r.URL.Path)

			// Allow DELETE or POST methods
			if r.Method != http.MethodDelete && r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, err := fmt.Fprintf(w, "Method not allowed")
				if err != nil {
					fmt.Printf("Error writing response: %v\n", err)
				}
				return
			}

			// Get message ID from query parameter
			messageID := r.URL.Query().Get("id")
			if messageID == "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, "Missing id parameter")
				return
			}

			// Create a trace span for this operation
			ctx, span := tracer.Start(r.Context(), "cleanup-endpoint")
			defer span.End()
			span.SetAttributes(attribute.String("message.id", messageID))

			// Delete the message from PostgreSQL
			err := deleteMessageByID(ctx, messageID)
			if err != nil {
				status := http.StatusInternalServerError
				if err.Error() == fmt.Sprintf("message with ID %s not found", messageID) {
					status = http.StatusNotFound
				}

				span.RecordError(err)
				span.SetStatus(codes.Error, "Failed to delete message")
				w.WriteHeader(status)
				fmt.Fprintf(w, "Error: %v", err)
				return
			}

			// Also clean up from Redis if possible
			redisKey := "msg:" + messageID
			redisErr := redisClient.Del(ctx, redisKey).Err()
			if redisErr != nil {
				fmt.Printf("Warning: Failed to clean up Redis key %s: %v\n", redisKey, redisErr)
			}

			// Return success response
			w.Header().Set("Content-Type", "application/json")
			response := map[string]interface{}{
				"status":  "success",
				"message": "Message deleted successfully",
				"id":      messageID,
			}
			if err := json.NewEncoder(w).Encode(response); err != nil {
				span.RecordError(err)
				fmt.Printf("Error encoding response: %v\n", err)
			}

			span.SetStatus(codes.Ok, "Message deleted successfully")
			fmt.Printf("Successfully deleted message with ID: %s\n", messageID)
		})

		// Add reset-test-state endpoint for test setup/teardown
		http.HandleFunc("/reset-test-state", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling test state reset: %s %s\n", r.Method, r.URL.Path)

			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, err := fmt.Fprintf(w, "Method not allowed")
				if err != nil {
					fmt.Printf("Error writing response: %v\n", err)
				}
				return
			}

			// Only allow this endpoint in dev/test environments
			env := os.Getenv("ENV")
			if env != "dev" && env != "stg" && env != "test" {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, "This endpoint is only available in dev/test environments")
				return
			}

			ctx := r.Context()

			// Delete test messages from PostgreSQL
			query := `
				DELETE FROM messages 
				WHERE id LIKE '%test%' OR id LIKE '%cypress%'
			`

			result, err := postgresDB.ExecContext(ctx, query)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "Failed to delete test messages: %v", err)
				return
			}

			rowsAffected, err := result.RowsAffected()
			if err != nil {
				fmt.Printf("Warning: couldn't get rows affected: %v\n", err)
				rowsAffected = -1
			}

			// Clean up Redis keys related to test messages
			var redisKeysDeleted int64 = 0
			testKeys, err := redisClient.Keys(ctx, "msg:*test*").Result()
			if err == nil && len(testKeys) > 0 {
				redisKeysDeleted, _ = redisClient.Del(ctx, testKeys...).Result()
			}

			// Return success response
			w.Header().Set("Content-Type", "application/json")
			response := map[string]interface{}{
				"status":             "success",
				"message":            "Test state reset successful",
				"deleted_rows":       rowsAffected,
				"deleted_redis_keys": redisKeysDeleted,
			}
			if err := json.NewEncoder(w).Encode(response); err != nil {
				fmt.Printf("Error encoding response: %v\n", err)
			}

			fmt.Printf("Test state reset: deleted %d database rows and %d Redis keys\n",
				rowsAffected, redisKeysDeleted)
		})
		// Create a simple HTTP server for health checks
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling health check: %s %s\n", r.Method, r.URL.Path)
			_, err := w.Write([]byte("OK"))
			if err != nil {
				fmt.Printf("Error writing health check response: %v\n", err)
			}
		})

		// Add a Redis check endpoint
		http.HandleFunc("/redischeck", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling Redis check: %s %s\n", r.Method, r.URL.Path)

			if redisClient == nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, err := w.Write([]byte("Redis not connected"))
				if err != nil {
					fmt.Printf("Error writing Redis check response: %v\n", err)
				}
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, err := redisClient.Ping(ctx).Result()
			if err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, writeErr := fmt.Fprintf(w, "Redis connection error: %v", err)
				if writeErr != nil {
					fmt.Printf("Error writing Redis check response: %v\n", writeErr)
				}
				return
			}

			_, err = w.Write([]byte("Redis connection OK"))
			if err != nil {
				fmt.Printf("Error writing Redis check response: %v\n", err)
			}
		})

		// Add a DB check endpoint
		http.HandleFunc("/dbcheck", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling DB check: %s %s\n", r.Method, r.URL.Path)

			if postgresDB == nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, err := w.Write([]byte("PostgreSQL not connected"))
				if err != nil {
					fmt.Printf("Error writing DB check response: %v\n", err)
				}
				return
			}

			err := postgresDB.Ping()
			if err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, writeErr := w.Write([]byte(fmt.Sprintf("PostgreSQL connection error: %v", err)))
				if writeErr != nil {
					fmt.Printf("Error writing DB check response: %v\n", writeErr)
				}
				return
			}

			_, err = w.Write([]byte("PostgreSQL connection OK"))
			if err != nil {
				fmt.Printf("Error writing DB check response: %v\n", err)
			}
		})

		// Start the HTTP server
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}

		fmt.Printf("Starting health check server on port %s...\n", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			fmt.Printf("HTTP server error: %v\n", err)
		}
	}()
}
