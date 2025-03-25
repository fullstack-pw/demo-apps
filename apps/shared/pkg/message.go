package pkg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Message represents the data structure for our microservices
type Message struct {
	ID      string            `json:"id,omitempty"`
	Content string            `json:"content"`
	Headers map[string]string `json:"headers,omitempty"`
}

// PublishToNATS publishes a message to NATS with tracing
func PublishToNATS(ctx context.Context, natsConn *nats.Conn, tracer trace.Tracer, queueName string, msg Message) error {
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

	fmt.Printf("TraceID=%s Injected trace context into message headers\n", GetTraceID(ctx))

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
	fmt.Printf("TraceID=%s Message published to queue: %s\n", GetTraceID(ctx), queueName)
	return nil
}

// ExtractTracingContext tries to extract tracing context from a message
func ExtractTracingContext(message Message) context.Context {
	if message.Headers != nil {
		// Extract trace context from headers if present
		carrier := propagation.MapCarrier(message.Headers)
		return otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	}
	return context.Background()
}
