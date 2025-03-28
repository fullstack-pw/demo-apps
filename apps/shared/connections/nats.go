package connections

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/fullstack-pw/shared/tracing"

	"github.com/nats-io/nats.go"
)

// NATSConnection wraps a NATS connection with additional functionality
type NATSConnection struct {
	conn       *nats.Conn
	url        string
	options    []nats.Option
	maxRetries int
}

// NewNATSConnection creates a new NATS connection manager
func NewNATSConnection(options ...nats.Option) *NATSConnection {
	// Get NATS server URL from environment or use default
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://nats.fullstack.pw:4222"
	}

	maxRetries := 5

	return &NATSConnection{
		url:        natsURL,
		options:    options,
		maxRetries: maxRetries,
	}
}

// Connect establishes a connection to NATS with retry logic
func (n *NATSConnection) Connect(ctx context.Context) error {
	ctx, span := tracing.GetTracer("nats").Start(ctx, "nats.connect")
	defer span.End()

	traceID := tracing.GetTraceID(ctx)
	fmt.Printf("TraceID=%s Connecting to NATS at %s\n", traceID, n.url)

	var err error

	for attempt := 1; attempt <= n.maxRetries; attempt++ {
		fmt.Printf("TraceID=%s Attempt %d: Connecting to NATS at %s\n", traceID, attempt, n.url)

		// Attempt NATS connection
		n.conn, err = nats.Connect(n.url, n.options...)
		if err != nil {
			fmt.Printf("TraceID=%s NATS connection error: %v\n", traceID, err)
			if attempt == n.maxRetries {
				return fmt.Errorf("failed to connect to NATS after %d attempts: %w", n.maxRetries, err)
			}

			select {
			case <-time.After(time.Duration(attempt*2) * time.Second):
				// Continue to next attempt
			case <-ctx.Done():
				return fmt.Errorf("context canceled while connecting to NATS: %w", ctx.Err())
			}

			continue
		}

		fmt.Printf("TraceID=%s Successfully connected to NATS\n", traceID)
		return nil
	}

	return fmt.Errorf("exhausted all connection attempts to NATS")
}

// Close closes the NATS connection
func (n *NATSConnection) Close() {
	if n.conn != nil {
		fmt.Println("Closing NATS connection...")
		n.conn.Close()
		fmt.Println("NATS connection closed")
	}
}

// IsConnected checks if NATS connection is still active
func (n *NATSConnection) IsConnected() bool {
	return n.conn != nil && n.conn.IsConnected()
}

// Conn returns the underlying NATS connection
func (n *NATSConnection) Conn() *nats.Conn {
	return n.conn
}

// Check implements the health.Checker interface
func (n *NATSConnection) Check(ctx context.Context) error {
	if n.conn == nil {
		return fmt.Errorf("NATS connection not initialized")
	}

	if !n.conn.IsConnected() {
		return fmt.Errorf("NATS connection lost")
	}

	return nil
}

// Name implements the health.Checker interface
func (n *NATSConnection) Name() string {
	return "nats"
}

// PublishWithTracing publishes a message with tracing information
func (n *NATSConnection) PublishWithTracing(ctx context.Context, subject string, data []byte) error {
	ctx, span := tracing.GetTracer("nats").Start(ctx, "nats.publish")
	defer span.End()

	if n.conn == nil || !n.conn.IsConnected() {
		return fmt.Errorf("NATS connection not available")
	}

	err := n.conn.Publish(subject, data)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	return nil
}

// SubscribeWithTracing creates a subscription with tracing
func (n *NATSConnection) SubscribeWithTracing(subject string, handler nats.MsgHandler) (*nats.Subscription, error) {
	if n.conn == nil || !n.conn.IsConnected() {
		return nil, fmt.Errorf("NATS connection not available")
	}

	// Wrap the handler to add tracing
	tracingHandler := func(msg *nats.Msg) {
		ctx := context.Background()
		ctx, span := tracing.GetTracer("nats").Start(ctx, "nats.message.receive")
		defer span.End()

		handler(msg)
	}

	return n.conn.Subscribe(subject, tracingHandler)
}

// PublishWithEnvTracing publishes a message with environment prefix and tracing
func (n *NATSConnection) PublishWithEnvTracing(ctx context.Context, subject string, data []byte) error {
	// Get environment
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev" // Default to dev if ENV not set
	}

	// Create prefixed subject
	prefixedSubject := fmt.Sprintf("%s.%s", env, subject)

	// Delegate to regular tracing publish
	return n.PublishWithTracing(ctx, prefixedSubject, data)
}

// SubscribeWithEnvTracing creates a subscription with environment prefix and tracing
func (n *NATSConnection) SubscribeWithEnvTracing(subject string, handler nats.MsgHandler) (*nats.Subscription, error) {
	// Get environment
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev" // Default to dev if ENV not set
	}

	// Create prefixed subject
	prefixedSubject := fmt.Sprintf("%s.%s", env, subject)

	// Delegate to regular tracing subscribe
	return n.SubscribeWithTracing(prefixedSubject, handler)
}
