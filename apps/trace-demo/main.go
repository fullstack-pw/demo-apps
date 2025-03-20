package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver

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

// Global database connection
var db *sql.DB

// initTracer initializes the OpenTelemetry tracer
func initTracer() (*sdktrace.TracerProvider, error) {
	// Print clear debugging message
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

	// Configure the exporter using the newer pattern (without directly using gRPC deprecated methods)
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

// initDB initializes the PostgreSQL connection
func initDB() error {
	fmt.Println("Initializing PostgreSQL connection...")

	// Get database connection details from environment variables
	dbHost := os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "postgres.default.svc.cluster.local"
	}

	dbPort := os.Getenv("DB_PORT")
	if dbPort == "" {
		dbPort = "5432"
	}

	dbName := os.Getenv("DB_NAME")
	if dbName == "" {
		dbName = "postgres"
	}

	dbUser := os.Getenv("DB_USER")
	if dbUser == "" {
		dbUser = "admin"
	}

	dbPassword := os.Getenv("DB_PASSWORD")
	if dbPassword == "" {
		fmt.Println("Warning: DB_PASSWORD not set, using empty password")
	}

	// Create connection string
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		dbHost, dbPort, dbUser, dbPassword, dbName)

	fmt.Printf("Connecting to PostgreSQL at %s:%s/%s as %s\n", dbHost, dbPort, dbName, dbUser)

	// Open connection
	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("failed to open database connection: %w", err)
	}

	// Test connection
	err = db.Ping()
	if err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	fmt.Println("Successfully connected to PostgreSQL")

	// Create requests table if it doesn't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS requests (
			id SERIAL PRIMARY KEY,
			path TEXT NOT NULL,
			method TEXT NOT NULL,
			remote_addr TEXT,
			user_agent TEXT,
			timestamp TIMESTAMPTZ DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create requests table: %w", err)
	}

	fmt.Println("Database schema initialized")

	return nil
}

// insertRequest inserts request data into PostgreSQL with tracing
func insertRequest(ctx context.Context, path, method, remoteAddr, userAgent string) error {
	// Create a child span for the database operation
	tracer := otel.Tracer("trace-demo")
	ctx, span := tracer.Start(ctx, "db.insert_request", trace.WithAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation", "insert"),
		attribute.String("db.table", "requests"),
	))
	defer span.End()

	// Record the request details in span attributes
	span.SetAttributes(
		attribute.String("http.path", path),
		attribute.String("http.method", method),
		attribute.String("http.remote_addr", remoteAddr),
		attribute.String("http.user_agent", userAgent),
	)

	// Prepare the query
	query := `
		INSERT INTO requests (path, method, remote_addr, user_agent)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`

	// Execute the query
	var id int
	err := db.QueryRowContext(ctx, query, path, method, remoteAddr, userAgent).Scan(&id)
	if err != nil {
		// Record the error in the span
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to insert request: %w", err)
	}

	// Record success and the inserted ID
	span.SetAttributes(attribute.Int("db.request.id", id))
	span.SetStatus(codes.Ok, "Request inserted successfully")

	return nil
}

func main() {
	fmt.Println("Starting trace-demo application with PostgreSQL support...")

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

	// Initialize the database connection with retry logic
	for attempts := 0; attempts < 5; attempts++ {
		err = initDB()
		if err == nil {
			break
		}
		fmt.Printf("Attempt %d: Failed to initialize database: %v\n", attempts+1, err)
		time.Sleep(3 * time.Second)
	}

	// If all attempts failed, exit
	if err != nil {
		fmt.Printf("All attempts to initialize database failed: %v\n", err)
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

		// Close database connection
		if db != nil {
			fmt.Println("Closing database connection...")
			if err := db.Close(); err != nil {
				fmt.Printf("Error closing database connection: %v\n", err)
			}
			fmt.Println("Database connection closed successfully")
		}
	}()

	// Create a tracer
	tracer := otel.Tracer("trace-demo")
	fmt.Println("Created tracer instance")

	// Create instrumented handlers
	helloHandler := otelhttp.NewHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling request: %s %s\n", r.Method, r.URL.Path)

			// Create a span
			ctx, span := tracer.Start(r.Context(), "hello-operation")
			defer span.End()

			// Record incoming request details
			span.SetAttributes(
				attribute.String("http.path", r.URL.Path),
				attribute.String("http.method", r.Method),
				attribute.String("http.remote_addr", r.RemoteAddr),
				attribute.String("http.user_agent", r.UserAgent()),
			)

			// Insert request data into PostgreSQL
			err := insertRequest(ctx, r.URL.Path, r.Method, r.RemoteAddr, r.UserAgent())
			if err != nil {
				fmt.Printf("Error inserting request: %v\n", err)
				span.RecordError(err)
				span.SetStatus(codes.Error, "Database error")
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			// Write response
			_, err = w.Write([]byte("Hello, Observability with PostgreSQL!"))
			if err != nil {
				fmt.Printf("Error writing response: %v\n", err)
				span.RecordError(err)
			}

			span.SetStatus(codes.Ok, "Request handled successfully")
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

	// Add a DB check endpoint
	http.HandleFunc("/dbcheck", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("Handling DB check: %s %s\n", r.Method, r.URL.Path)

		if db == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err := w.Write([]byte("Database not initialized"))
			if err != nil {
				fmt.Printf("Error writing DB check response: %v\n", err)
			}
			return
		}

		err := db.Ping()
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err := w.Write([]byte(fmt.Sprintf("Database not available: %v", err)))
			if err != nil {
				fmt.Printf("Error writing DB check response: %v\n", err)
			}
			return
		}

		_, err = w.Write([]byte("Database connection OK"))
		if err != nil {
			fmt.Printf("Error writing DB check response: %v\n", err)
		}
		fmt.Println("DB check handled successfully")
	})
	fmt.Println("Registered DB check handler")

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
