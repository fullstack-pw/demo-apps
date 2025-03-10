package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func initTracer() (*sdktrace.TracerProvider, error) {
	// Print clear debugging message
	fmt.Println("Initializing OpenTelemetry tracer...")

	// Get OpenTelemetry Collector endpoint from env vars or use default
	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otelEndpoint == "" {
		otelEndpoint = "otel-collector.observability.svc.cluster.local:4317"
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

	// Create a gRPC connection to the collector
	fmt.Println("Establishing gRPC connection to collector...")

	// Don't use WithBlock for non-local environments to avoid hanging
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	// Only use WithBlock for debugging or if explicitly requested
	useBlockingDial := os.Getenv("OTEL_BLOCKING_DIAL") == "true"
	if useBlockingDial {
		dialOpts = append(dialOpts, grpc.WithBlock())
	}

	conn, err := grpc.DialContext(ctx, otelEndpoint, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection: %w", err)
	}
	fmt.Println("gRPC connection established successfully")

	// Configure the exporter
	fmt.Println("Creating OTLP trace exporter...")
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}
	fmt.Println("Trace exporter created successfully")

	// Configure resource attributes
	fmt.Println("Configuring resource attributes...")
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("trace-demo"),
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

func main() {
	fmt.Println("Starting trace-demo application...")

	// Try catching initialization errors
	var tp *sdktrace.TracerProvider
	var err error

	// Initialize the tracer with retry logic
	for attempts := 0; attempts < 3; attempts++ {
		tp, err = initTracer()
		if err == nil {
			break
		}
		fmt.Printf("Attempt %d: Failed to initialize tracer: %v\n", attempts+1, err)
		time.Sleep(2 * time.Second)
	}

	// If all attempts failed, exit
	if err != nil {
		fmt.Printf("All attempts to initialize tracer failed: %v\n", err)
		os.Exit(1)
	}

	// Ensure tracer is shut down properly
	defer func() {
		fmt.Println("Shutting down tracer provider...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			fmt.Printf("Error shutting down tracer provider: %v\n", err)
		}
		fmt.Println("Tracer provider shut down successfully")
	}()

	// Create a tracer
	tracer := otel.Tracer("trace-demo")
	fmt.Println("Created tracer instance")

	// Create instrumented handlers
	helloHandler := otelhttp.NewHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling request: %s %s\n", r.Method, r.URL.Path)

			// Create a span (without using the ctx variable to avoid compile error)
			_, span := tracer.Start(r.Context(), "hello-operation")
			defer span.End()

			// Fixed: Check the error when writing response
			_, err := w.Write([]byte("Hello, Observability!"))
			if err != nil {
				fmt.Printf("Error writing response: %v\n", err)
			}

			fmt.Printf("Request handled successfully: %s %s\n", r.Method, r.URL.Path)
		}),
		"hello-handler",
	)
	fmt.Println("Created HTTP handler with OpenTelemetry instrumentation")

	// Register routes
	http.Handle("/", helloHandler)
	fmt.Println("Registered root handler")

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

	// Create a server with a timeout
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
