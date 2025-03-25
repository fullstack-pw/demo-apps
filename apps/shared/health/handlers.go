package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"fullstack.pw/shared/tracing"
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

// CreateHealthHandler creates a standard health check handler
func CreateHealthHandler(checkers ...Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracing.GetTracer("health").Start(r.Context(), "health-check")
		defer span.End()

		fmt.Printf("TraceID=%s Handling health check: %s %s\n", tracing.GetTraceID(ctx), r.Method, r.URL.Path)

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
				response.Status = StatusDown // If any component is down, overall status is down
			}

			response.Components[checker.Name()] = ComponentStatus{
				Status:  status,
				Name:    checker.Name(),
				Message: message,
			}
		}

		// If no components are provided, just return a basic UP status
		if len(checkers) == 0 {
			fmt.Println("No component checkers provided, returning basic UP status")
		}

		w.Header().Set("Content-Type", "application/json")

		if response.Status == StatusDown {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		if err := json.NewEncoder(w).Encode(response); err != nil {
			fmt.Printf("Error encoding health response: %v\n", err)
		}

		fmt.Printf("TraceID=%s Health check completed with status: %s\n", tracing.GetTraceID(ctx), response.Status)
	}
}

// CreateLivenessHandler creates a simplified liveness probe handler
func CreateLivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}
}

// CreateReadinessHandler creates a readiness probe that checks all components
func CreateReadinessHandler(checkers ...Checker) http.HandlerFunc {
	return CreateHealthHandler(checkers...)
}

// RegisterHealthEndpoints registers all health endpoints on the given mux
func RegisterHealthEndpoints(mux *http.ServeMux, livenessCheckers, readinessCheckers []Checker) {
	// Fix: We need to create a separate handler for the combined checkers
	allCheckers := make([]Checker, 0, len(livenessCheckers)+len(readinessCheckers))
	allCheckers = append(allCheckers, livenessCheckers...)
	allCheckers = append(allCheckers, readinessCheckers...)

	mux.HandleFunc("/health", CreateHealthHandler(allCheckers...))
	mux.HandleFunc("/livez", CreateLivenessHandler())
	mux.HandleFunc("/readyz", CreateReadinessHandler(readinessCheckers...))
}
