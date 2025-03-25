package tracing

import (
	"context"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// GetTraceID extracts the trace ID from a context
func GetTraceID(ctx context.Context) string {
	if span := trace.SpanFromContext(ctx); span != nil {
		return span.SpanContext().TraceID().String()
	}
	return "unknown"
}

// GetSpanID extracts the span ID from a context
func GetSpanID(ctx context.Context) string {
	if span := trace.SpanFromContext(ctx); span != nil {
		return span.SpanContext().SpanID().String()
	}
	return "unknown"
}

// ContextWithRequestID adds a request ID to the context if one doesn't exist
func ContextWithRequestID(ctx context.Context) (context.Context, string) {
	if existingID := ctx.Value("request_id"); existingID != nil {
		if id, ok := existingID.(string); ok {
			return ctx, id
		}
	}

	id := uuid.New().String()
	return context.WithValue(ctx, "request_id", id), id
}

// GetRequestID gets the request ID from the context
func GetRequestID(ctx context.Context) string {
	if id := ctx.Value("request_id"); id != nil {
		if strID, ok := id.(string); ok {
			return strID
		}
	}
	return "unknown"
}

// WithContextValues adds useful values to the context for tracing
func WithContextValues(ctx context.Context, keyValues map[string]interface{}) context.Context {
	for k, v := range keyValues {
		ctx = context.WithValue(ctx, k, v)
	}
	return ctx
}
