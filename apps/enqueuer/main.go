package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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
	natsConn *nats.Conn
	tracer   trace.Tracer
)

// Message represents the data structure we'll enqueue
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
			semconv.ServiceNameKey.String("enqueuer"),
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
		natsURL = "nats://nats.default.svc.cluster.local:4222"
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

func getTraceID(ctx context.Context) string {
	if span := trace.SpanFromContext(ctx); span != nil {
		return span.SpanContext().TraceID().String()
	}
	return "unknown"
}

// publishToNATS publishes a message to NATS with tracing
func publishToNATS(ctx context.Context, queueName string, msg Message) error {
	// Create a child span for the NATS operation
	ctx, span := tracer.Start(ctx, "nats.publish", trace.WithAttributes(
		attribute.String("queue.name", queueName),
		attribute.String("message.id", msg.ID),
	))
	defer span.End()

	// Ensure Headers map exists
	if msg.Headers == nil {
		msg.Headers = make(map[string]string)
	}

	// Inject current trace context into message headers
	carrier := propagation.MapCarrier(msg.Headers)
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	fmt.Printf("TraceID=%s Injected trace context into message headers\n", getTraceID(ctx))

	// Convert message to JSON
	data, err := json.Marshal(msg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to marshal message to JSON")
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Publish message to NATS
	err = natsConn.Publish(queueName, data)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to publish message to NATS")
		return fmt.Errorf("failed to publish message: %w", err)
	}

	span.SetStatus(codes.Ok, "Message published successfully")
	fmt.Printf("TraceID=%s Message published to queue: %s\n", getTraceID(ctx), queueName)
	return nil
}

// handleAdd handles the /add endpoint
func handleAdd(w http.ResponseWriter, r *http.Request) {
	// Extract context and create a span
	ctx, span := tracer.Start(r.Context(), "handle-add-request")
	defer span.End()

	fmt.Printf("TraceID=%s Handling request: %s %s\n", getTraceID(ctx), r.Method, r.URL.Path)

	// Only accept POST requests
	if r.Method != http.MethodPost {
		span.SetStatus(codes.Error, "Method not allowed")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to read request body")
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Parse message
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Invalid JSON payload")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// Extract queue name from query parameter or use default
	queueName := r.URL.Query().Get("queue")
	if queueName == "" {
		queueName = "default"
	}

	// Set span attributes for request details
	span.SetAttributes(
		attribute.String("http.method", r.Method),
		attribute.String("http.path", r.URL.Path),
		attribute.String("queue.name", queueName),
		attribute.String("message.content", msg.Content),
	)

	// Publish to NATS
	err = publishToNATS(ctx, queueName, msg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to publish message")
		http.Error(w, fmt.Sprintf("Failed to publish message: %v", err), http.StatusInternalServerError)
		return
	}

	// Return success response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	response := map[string]string{"status": "queued", "queue": queueName}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		span.RecordError(err)
		log.Printf("Error encoding response: %v", err)
	}

	span.SetStatus(codes.Ok, "Message queued successfully")
	fmt.Printf("TraceID=%s Request handled successfully\n", getTraceID(ctx))
}

// handleCheckMemorizer checks if a message was processed by memorizer
func handleCheckMemorizer(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handle-check-memorizer")
	defer span.End()

	messageId := r.URL.Query().Get("id")
	if messageId == "" {
		http.Error(w, "Missing message ID", http.StatusBadRequest)
		return
	}
	fmt.Printf("TraceID=%s Handling request: %s %s\n", getTraceID(ctx), r.Method, r.URL.Path)
	// Use internal service discovery to check with memorizer
	// This would involve a HTTP call to memorizer service within the cluster
	resp, err := http.Get(fmt.Sprintf("http://memorizer.default.svc.cluster.local:8080/status?id=%s", messageId))
	if err != nil {
		span.RecordError(err)
		http.Error(w, fmt.Sprintf("Error contacting memorizer: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// Forward the response from memorizer
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleCheckWriter checks if a message was written by writer
func handleCheckWriter(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handle-check-writer")
	defer span.End()

	messageId := r.URL.Query().Get("id")
	if messageId == "" {
		http.Error(w, "Missing message ID", http.StatusBadRequest)
		return
	}
	fmt.Printf("TraceID=%s Handling request: %s %s\n", getTraceID(ctx), r.Method, r.URL.Path)
	// Use internal service discovery to check with writer
	resp, err := http.Get(fmt.Sprintf("http://writer.default.svc.cluster.local:8080/query?id=%s", messageId))
	if err != nil {
		span.RecordError(err)
		http.Error(w, fmt.Sprintf("Error contacting writer: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// Forward the response from writer
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleCheckTrace checks trace propagation
func handleCheckTrace(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handle-check-trace")
	defer span.End()

	messageId := r.URL.Query().Get("id")
	if messageId == "" {
		http.Error(w, "Missing message ID", http.StatusBadRequest)
		return
	}
	fmt.Printf("TraceID=%s Handling request: %s %s\n", getTraceID(ctx), r.Method, r.URL.Path)
	// Use internal service discovery to check with writer
	resp, err := http.Get(fmt.Sprintf("http://writer.default.svc.cluster.local:8080/traces?id=%s", messageId))
	if err != nil {
		span.RecordError(err)
		http.Error(w, fmt.Sprintf("Error contacting writer for trace: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// Forward the response
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func main() {
	fmt.Println("Starting enqueuer application...")

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

	// Create a tracer
	tracer = otel.Tracer("enqueuer")
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
	}()

	// Create instrumented handlers
	addHandler := otelhttp.NewHandler(
		http.HandlerFunc(handleAdd),
		"add-handler",
	)

	// Register routes
	http.Handle("/add", addHandler)
	// Add to the HTTP server registration section in main.go
	http.Handle("/check-memorizer", otelhttp.NewHandler(http.HandlerFunc(handleCheckMemorizer), "check-memorizer-handler"))
	http.Handle("/check-writer", otelhttp.NewHandler(http.HandlerFunc(handleCheckWriter), "check-writer-handler"))
	http.Handle("/check-trace", otelhttp.NewHandler(http.HandlerFunc(handleCheckTrace), "check-trace-handler"))
	fmt.Println("Registered /add endpoint")

	// Add a health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("Handling health check: %s %s\n", r.Method, r.URL.Path)
		_, err := w.Write([]byte("OK"))
		if err != nil {
			fmt.Printf("Error writing health check response: %v\n", err)
		}
		fmt.Println("Health check handled successfully")
	})
	fmt.Println("Registered health check handler")

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
		fmt.Println("NATS check handled successfully")
	})
	fmt.Println("Registered NATS check handler")

	// Set up the server with a timeout
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := &http.Server{
		Addr:         ":" + port,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Handle graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigChan
		fmt.Printf("Received signal %s, shutting down...\n", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			fmt.Printf("HTTP server shutdown error: %v\n", err)
		}
	}()

	// Start the HTTP server
	fmt.Printf("HTTP server starting on port %s...\n", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		fmt.Printf("HTTP server error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Server stopped gracefully")
}
