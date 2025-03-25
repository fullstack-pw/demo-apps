package pkg

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
)

// TracingConfig holds configuration for the tracer setup
type TracingConfig struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	OtelEndpoint   string
	SkipTracing    bool
}

// DefaultTracingConfig returns a config with sensible defaults
func DefaultTracingConfig() TracingConfig {
	return TracingConfig{
		ServiceName:    "service",
		ServiceVersion: "0.1.0",
		Environment:    os.Getenv("ENV"),
		OtelEndpoint:   os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		SkipTracing:    os.Getenv("SKIP_TRACING") == "true",
	}
}

// InitTracer initializes and returns an OpenTelemetry TracerProvider
func InitTracer(config TracingConfig) (*sdktrace.TracerProvider, error) {
	fmt.Println("Initializing OpenTelemetry tracer...")

	// Use default endpoint if not provided
	if config.OtelEndpoint == "" {
		config.OtelEndpoint = "opentelemetry-collector.observability.svc.cluster.local:4317"
	}
	fmt.Printf("Using OpenTelemetry endpoint: %s\n", config.OtelEndpoint)

	// Return no-op tracer for local development if requested
	if config.SkipTracing {
		fmt.Println("Skipping actual tracing setup, using no-op tracer")
		return sdktrace.NewTracerProvider(), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Configure the exporter
	fmt.Println("Creating OTLP trace exporter...")
	exporter, err := otlptracegrpc.New(
		ctx,
		otlptracegrpc.WithEndpoint(config.OtelEndpoint),
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
			semconv.ServiceNameKey.String(config.ServiceName),
			semconv.ServiceVersionKey.String(config.ServiceVersion),
			attribute.String("deployment.environment", config.Environment),
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

// GetTraceID extracts the trace ID from the context
func GetTraceID(ctx context.Context) string {
	if span := trace.SpanFromContext(ctx); span != nil {
		return span.SpanContext().TraceID().String()
	}
	return "unknown"
}
