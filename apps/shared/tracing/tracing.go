package tracing

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// Config holds configuration for the tracer initialization
type Config struct {
	ServiceName       string
	ServiceVersion    string
	Environment       string
	OtelEndpoint      string
	PropagatorType    string
	AdditionalAttrs   []attribute.KeyValue
	ShutdownTimeout   time.Duration
	DisableForTesting bool
	DebugMode         bool
}

// HealthCheckFilterSampler filters out health check endpoints when not in debug mode
type HealthCheckFilterSampler struct {
	baseSampler sdktrace.Sampler
	debugMode   bool
}

// ShouldSample implements the Sampler interface
func (s *HealthCheckFilterSampler) ShouldSample(params sdktrace.SamplingParameters) sdktrace.SamplingResult {
	// First get the parent's sampling decision
	result := s.baseSampler.ShouldSample(params)

	// If not in debug mode, check if this is a health check span
	if !s.debugMode {
		// Check if span name contains health check related names
		spanName := params.Name
		if strings.Contains(spanName, "health") ||
			strings.Contains(spanName, "/health") ||
			strings.Contains(spanName, "/livez") ||
			strings.Contains(spanName, "/readyz") {
			// Return "don't sample" decision
			return sdktrace.SamplingResult{
				Decision:   sdktrace.Drop,
				Tracestate: result.Tracestate,
			}
		}
	}

	// Otherwise, return the original decision
	return result
}

// Description returns a description of the sampler
func (s *HealthCheckFilterSampler) Description() string {
	return fmt.Sprintf("HealthCheckFilterSampler(debugMode=%v, base=%s)",
		s.debugMode, s.baseSampler.Description())
}

// NewHealthCheckFilterSampler creates a new sampler that filters health checks
func NewHealthCheckFilterSampler(baseSampler sdktrace.Sampler, debugMode bool) sdktrace.Sampler {
	return &HealthCheckFilterSampler{
		baseSampler: baseSampler,
		debugMode:   debugMode,
	}
}

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig(serviceName string) Config {
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev"
	}

	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otelEndpoint == "" {
		otelEndpoint = "opentelemetry-collector.observability.svc.cluster.local:4317"
	}

	return Config{
		ServiceName:     serviceName,
		ServiceVersion:  "0.1.0",
		Environment:     env,
		OtelEndpoint:    otelEndpoint,
		PropagatorType:  "tracecontext",
		ShutdownTimeout: 5 * time.Second,
	}
}

// InitTracer initializes the OpenTelemetry tracer with the given configuration
func InitTracer(cfg Config) (*sdktrace.TracerProvider, error) {
	fmt.Printf("Initializing OpenTelemetry tracer for %s...\n", cfg.ServiceName)

	// Handle testing/local development
	if cfg.DisableForTesting || os.Getenv("SKIP_TRACING") == "true" {
		fmt.Println("Using no-op tracer for local development/testing")
		return sdktrace.NewTracerProvider(), nil
	}

	fmt.Printf("Using OpenTelemetry endpoint: %s\n", cfg.OtelEndpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Configure the exporter
	fmt.Println("Creating OTLP trace exporter...")
	exporter, err := otlptracegrpc.New(
		ctx,
		otlptracegrpc.WithEndpoint(cfg.OtelEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}
	fmt.Println("Trace exporter created successfully")

	// Standard resource attributes
	attrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(cfg.ServiceName),
		semconv.ServiceVersionKey.String(cfg.ServiceVersion),
		attribute.String("environment", cfg.Environment),
	}

	// Add any additional attributes
	attrs = append(attrs, cfg.AdditionalAttrs...)

	// Configure resource attributes
	fmt.Println("Configuring resource attributes...")
	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create trace provider
	fmt.Println("Creating trace provider...")
	baseSampler := sdktrace.ParentBased(sdktrace.AlwaysSample())
	// Wrap it with our custom health check filter
	customSampler := NewHealthCheckFilterSampler(baseSampler, cfg.DebugMode)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(customSampler),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// Set up the appropriate propagator
	switch cfg.PropagatorType {
	case "b3":
		// Implement B3 propagation if needed
	case "jaeger":
		// Implement Jaeger propagation if needed
	default: // "tracecontext"
		otel.SetTextMapPropagator(propagation.TraceContext{})
	}

	fmt.Println("Trace provider initialized successfully")
	return tp, nil
}

// GetTracer returns a named tracer from the global provider
func GetTracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// ExtractTraceContext extracts trace context from headers
func ExtractTraceContext(ctx context.Context, headers map[string]string) context.Context {
	if headers == nil {
		return ctx
	}

	carrier := propagation.MapCarrier(headers)
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// InjectTraceContext injects the current trace context into headers
func InjectTraceContext(ctx context.Context, headers map[string]string) {
	if headers == nil {
		return
	}

	carrier := propagation.MapCarrier(headers)
	otel.GetTextMapPropagator().Inject(ctx, carrier)
}

// ShutdownTracer shuts down the tracer provider
func ShutdownTracer(ctx context.Context, tp *sdktrace.TracerProvider) error {
	if tp == nil {
		return nil
	}

	return tp.Shutdown(ctx)
}
