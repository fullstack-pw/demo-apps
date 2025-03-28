package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/fullstack-pw/shared/tracing"
	"go.opentelemetry.io/otel/trace"
)

// Checker is an interface for components that can be health-checked
type Checker interface {
	Check(ctx context.Context) error
	Name() string
}

// NATSChecker is a health checker for NATS connections
type NATSChecker interface {
	Checker
	IsConnected() bool
}

// RedisChecker is a health checker for Redis connections
type RedisChecker interface {
	Checker
	Ping(ctx context.Context) error
}

// DatabaseChecker is a health checker for database connections
type DatabaseChecker interface {
	Checker
	Ping() error
}

// Status represents the status of a component
type Status string

const (
	StatusUp   Status = "UP"
	StatusDown Status = "DOWN"
)

// ComponentStatus represents the health status of a component
type ComponentStatus struct {
	Status  Status `json:"status"`
	Name    string `json:"name"`
	Message string `json:"message,omitempty"`
}

// HealthResponse represents the overall health response
type HealthResponse struct {
	Status     Status                     `json:"status"`
	Components map[string]ComponentStatus `json:"components,omitempty"`
	Timestamp  string                     `json:"timestamp"`
}

// HealthCheckConfig represents configuration for health checks
type HealthCheckConfig struct {
	// VerboseLogging controls whether health checks produce detailed logs
	VerboseLogging bool
}

// DefaultHealthCheckConfig returns a configuration with default settings
func DefaultHealthCheckConfig() HealthCheckConfig {
	return HealthCheckConfig{
		VerboseLogging: false, // Default to non-verbose
	}
}

// CreateHealthHandler creates a standard health check handler with configuration
func CreateHealthHandler(config HealthCheckConfig, checkers ...Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ctx context.Context
		var span trace.Span

		if config.VerboseLogging {
			ctx, span = tracing.GetTracer("health").Start(r.Context(), "health-check")
			defer span.End()
			fmt.Printf("TraceID=%s Handling health check: %s %s\n", tracing.GetTraceID(ctx), r.Method, r.URL.Path)
		} else {
			ctx = r.Context()
		}

		response := HealthResponse{
			Status:     StatusUp,
			Components: make(map[string]ComponentStatus),
			Timestamp:  time.Now().UTC().Format(time.RFC3339),
		}

		// Check each component
		for _, checker := range checkers {
			ctxWithTimeout, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := checker.Check(ctxWithTimeout)
			cancel()

			status := StatusUp
			message := ""

			if err != nil {
				status = StatusDown
				message = err.Error()
				response.Status = StatusDown
			}

			response.Components[checker.Name()] = ComponentStatus{
				Status:  status,
				Name:    checker.Name(),
				Message: message,
			}
		}

		// If no components are provided, just return a basic UP status
		if len(checkers) == 0 && config.VerboseLogging {
			fmt.Println("No component checkers provided, returning basic UP status")
		}

		w.Header().Set("Content-Type", "application/json")

		if response.Status == StatusDown {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		if err := json.NewEncoder(w).Encode(response); err != nil && config.VerboseLogging {
			fmt.Printf("Error encoding health response: %v\n", err)
		}

		if config.VerboseLogging {
			fmt.Printf("TraceID=%s Health check completed with status: %s\n", tracing.GetTraceID(ctx), response.Status)
		}
	}
}

// CreateLivenessHandler creates a simplified liveness probe handler with configuration
func CreateLivenessHandler(config HealthCheckConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if config.VerboseLogging {
			ctx, span := tracing.GetTracer("health").Start(r.Context(), "liveness-check")
			defer span.End()
			fmt.Printf("TraceID=%s Handling liveness check: %s %s\n", tracing.GetTraceID(ctx), r.Method, r.URL.Path)
		}

		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("OK"))

		if err != nil && config.VerboseLogging {
			fmt.Printf("Error writing liveness response: %v\n", err)
		}

		if config.VerboseLogging {
			fmt.Printf("TraceID=%s Liveness check completed\n", tracing.GetTraceID(r.Context()))
		}
	}
}

// CreateReadinessHandler creates a readiness probe that checks all components with configuration
func CreateReadinessHandler(config HealthCheckConfig, checkers ...Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ctx context.Context
		var span trace.Span

		if config.VerboseLogging {
			ctx, span = tracing.GetTracer("health").Start(r.Context(), "readiness-check")
			defer span.End()
			fmt.Printf("TraceID=%s Handling readiness check: %s %s\n", tracing.GetTraceID(ctx), r.Method, r.URL.Path)
		} else {
			ctx = r.Context()
		}

		response := HealthResponse{
			Status:     StatusUp,
			Components: make(map[string]ComponentStatus),
			Timestamp:  time.Now().UTC().Format(time.RFC3339),
		}

		// Check each component
		for _, checker := range checkers {
			ctxWithTimeout, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := checker.Check(ctxWithTimeout)
			cancel()

			status := StatusUp
			message := ""

			if err != nil {
				status = StatusDown
				message = err.Error()
				response.Status = StatusDown
			}

			response.Components[checker.Name()] = ComponentStatus{
				Status:  status,
				Name:    checker.Name(),
				Message: message,
			}
		}

		w.Header().Set("Content-Type", "application/json")

		if response.Status == StatusDown {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		if err := json.NewEncoder(w).Encode(response); err != nil && config.VerboseLogging {
			fmt.Printf("Error encoding readiness response: %v\n", err)
		}

		if config.VerboseLogging {
			fmt.Printf("TraceID=%s Readiness check completed with status: %s\n", tracing.GetTraceID(ctx), response.Status)
		}
	}
}

func RegisterHealthEndpoints(mux *http.ServeMux, config HealthCheckConfig, livenessCheckers, readinessCheckers []Checker) {
	allCheckers := make([]Checker, 0, len(livenessCheckers)+len(readinessCheckers))
	allCheckers = append(allCheckers, livenessCheckers...)
	allCheckers = append(allCheckers, readinessCheckers...)

	mux.HandleFunc("/health", CreateHealthHandler(config, allCheckers...))
	mux.HandleFunc("/livez", CreateLivenessHandler(config))
	mux.HandleFunc("/readyz", CreateReadinessHandler(config, readinessCheckers...))
}
