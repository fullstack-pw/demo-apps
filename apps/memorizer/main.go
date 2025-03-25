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

	"github.com/go-redis/redis/v8"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	natsConn    *nats.Conn
	redisClient *redis.Client
	tracer      trace.Tracer
)

// Message represents the data structure coming from the queue
type Message struct {
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
			semconv.ServiceNameKey.String("memorizer"),
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

// initNATS initializes the connection to NATS server
func initNATS() error {
	fmt.Println("Initializing NATS connection...")

	// Get NATS server URL from environment or use default
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://nats.fullstack.pw:4222"
	}
	fmt.Printf("Using NATS server: %s\n", natsURL)

	// Multiple connection attempts with exponential backoff
	maxRetries := 5
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		fmt.Printf("Attempt %d: Connecting to NATS at %s\n", attempt, natsURL)

		// Attempt NATS connection
		natsConn, err = nats.Connect(natsURL)
		if err != nil {
			fmt.Printf("NATS connection error: %v\n", err)
			if attempt == maxRetries {
				return fmt.Errorf("failed to connect to NATS after %d attempts: %w", maxRetries, err)
			}
			time.Sleep(time.Duration(attempt*2) * time.Second)
			continue
		}

		fmt.Println("Successfully connected to NATS")
		return nil
	}

	return fmt.Errorf("exhausted all connection attempts to NATS")
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

func getTraceID(ctx context.Context) string {
	if span := trace.SpanFromContext(ctx); span != nil {
		return span.SpanContext().TraceID().String()
	}
	return "unknown"
}

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

	// Generate a key for Redis storage
	// Using message ID if provided, otherwise generate a timestamp-based key
	key := id
	if key == "" {
		key = fmt.Sprintf("msg:%d", time.Now().UnixNano())
		span.SetAttributes(attribute.String("generated.key", key))
	}

	// Create a message with trace context to store in Redis
	redisMsg := Message{
		ID:      id,
		Content: content,
		Headers: make(map[string]string),
	}
	// Inject current trace context into Redis message headers
	carrier := propagation.MapCarrier(redisMsg.Headers)
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	// Convert to JSON for storage
	msgJSON, err := json.Marshal(redisMsg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to marshal message with trace context")
		return fmt.Errorf("failed to marshal message with trace context: %w", err)
	}

	// Store the JSON in Redis instead of raw content
	expiration := 24 * time.Hour // Keep messages for 24 hours
	err = redisClient.Set(ctx, key, string(msgJSON), expiration).Err()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to store message in Redis")
		return fmt.Errorf("failed to store message in Redis: %w", err)
	}

	span.SetStatus(codes.Ok, "Message stored successfully")
	fmt.Printf("TraceID=%s Message stored in Redis with key: %s\n", getTraceID(ctx), key)
	return nil
}

// handleMessage processes a message from NATS with tracing
func handleMessage(msg *nats.Msg) {
	// Create a context and start a span for the message handling
	// Create a context with the trace from the message headers
	var parentCtx context.Context

	// Try to unmarshal the message to extract headers
	var message Message
	if err := json.Unmarshal(msg.Data, &message); err == nil && message.Headers != nil {
		// Extract trace context from headers
		carrier := propagation.MapCarrier(message.Headers)
		parentCtx = otel.GetTextMapPropagator().Extract(context.Background(), carrier)
		fmt.Printf("Extracted trace context from message headers\n")
	} else {
		// If headers unavailable, use background context
		parentCtx = context.Background()
		fmt.Printf("No trace context found in message, starting new trace\n")
	}

	// Now start the span with the extracted context
	ctx, span := tracer.Start(parentCtx, "nats.message.process")
	defer span.End()

	fmt.Printf("TraceID=%s Received message: %s\n", getTraceID(ctx), string(msg.Data))

	// Set attributes for the message
	span.SetAttributes(
		attribute.String("message.subject", msg.Subject),
		attribute.Int("message.size", len(msg.Data)),
	)

	// Parse message
	if err := json.Unmarshal(msg.Data, &message); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to parse message")
		fmt.Printf("TraceID=%s Error parsing message: %v\n", getTraceID(ctx), err)
		return
	}

	// Extract trace context from headers if available
	// This could be used for trace propagation if headers contain trace context
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
		fmt.Printf("TraceID=%s Error storing message: %v\n", getTraceID(ctx), err)
		return
	}

	span.SetStatus(codes.Ok, "Message processed successfully")
	fmt.Printf("TraceID=%s Message processed successfully\n", getTraceID(ctx))
}

func subscribeToQueues() error {
	// Get queue names from environment or use default
	queueNames := os.Getenv("QUEUE_NAMES")
	if queueNames == "" {
		queueNames = "default"
	}

	// Subscribe to the specified queue
	fmt.Printf("Subscribing to NATS queue: %s\n", queueNames)
	_, err := natsConn.Subscribe(queueNames, handleMessage)
	if err != nil {
		return fmt.Errorf("failed to subscribe to queue %s: %w", queueNames, err)
	}

	fmt.Printf("Successfully subscribed to queue: %s\n", queueNames)
	return nil
}

func main() {
	fmt.Println("Starting memorizer application...")

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

	// Initialize NATS connection with retry logic
	for attempts := 0; attempts < 3; attempts++ {
		err = initNATS()
		if err == nil {
			break
		}
		fmt.Printf("Attempt %d: Failed to initialize NATS: %v\n", attempts+1, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		fmt.Printf("All attempts to initialize NATS failed: %v\n", err)
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

	// Create a tracer
	tracer = otel.Tracer("memorizer")
	fmt.Println("Created tracer instance")

	// Ensure tracer is shut down properly
	defer func() {
		fmt.Println("Shutting down tracer provider...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			fmt.Printf("Error shutting down tracer provider: %v\n", err)
		}
		fmt.Println("Tracer provider shut down successfully")

		// Close NATS connection
		if natsConn != nil {
			fmt.Println("Closing NATS connection...")
			natsConn.Close()
			fmt.Println("NATS connection closed successfully")
		}

		// Close Redis connection
		if redisClient != nil {
			fmt.Println("Closing Redis connection...")
			if err := redisClient.Close(); err != nil {
				fmt.Printf("Error closing Redis connection: %v\n", err)
			}
			fmt.Println("Redis connection closed successfully")
		}
	}()

	// Subscribe to NATS queues
	if err := subscribeToQueues(); err != nil {
		fmt.Printf("Failed to subscribe to queues: %v\n", err)
		os.Exit(1)
	}

	// Set up HTTP server for health checks
	// Since we need to expose endpoints for Kubernetes probes
	setupHealthEndpoints()

	// Wait for interrupt signal to gracefully terminate the service
	fmt.Println("Memorizer service is running. Press CTRL+C to exit.")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("Shutting down gracefully...")
}

// setupHealthEndpoints sets up a small HTTP server for health checks
func setupHealthEndpoints() {
	go func() {
		// Create a simple HTTP server for health checks
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling health check: %s %s\n", r.Method, r.URL.Path)
			_, err := w.Write([]byte("OK"))
			if err != nil {
				fmt.Printf("Error writing health check response: %v\n", err)
			}
		})

		// Add a NATS check endpoint
		http.HandleFunc("/natscheck", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling NATS check: %s %s\n", r.Method, r.URL.Path)

			if natsConn == nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, err := w.Write([]byte("NATS not connected"))
				if err != nil {
					fmt.Printf("Error writing NATS check response: %v\n", err)
				}
				return
			}

			if !natsConn.IsConnected() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, err := w.Write([]byte("NATS connection lost"))
				if err != nil {
					fmt.Printf("Error writing NATS check response: %v\n", err)
				}
				return
			}

			_, err := w.Write([]byte("NATS connection OK"))
			if err != nil {
				fmt.Printf("Error writing NATS check response: %v\n", err)
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
