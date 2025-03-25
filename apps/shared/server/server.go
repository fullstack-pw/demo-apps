package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fullstack-pw/shared/health"
	"github.com/fullstack-pw/shared/logging"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Server represents a configurable HTTP server
type Server struct {
	name    string
	addr    string
	mux     *http.ServeMux
	server  *http.Server
	logger  *logging.Logger
	readyFn func() bool
}

// ServerOption is a function that configures a Server
type ServerOption func(*Server)

// WithAddress sets the server address
func WithAddress(addr string) ServerOption {
	return func(s *Server) {
		s.addr = addr
	}
}

// WithLogger sets the server logger
func WithLogger(logger *logging.Logger) ServerOption {
	return func(s *Server) {
		s.logger = logger
	}
}

// WithReadinessCheck sets a function to determine if the server is ready
func WithReadinessCheck(fn func() bool) ServerOption {
	return func(s *Server) {
		s.readyFn = fn
	}
}

// WithTimeout sets server timeouts
func WithTimeout(read, write, idle time.Duration) ServerOption {
	return func(s *Server) {
		s.server.ReadTimeout = read
		s.server.WriteTimeout = write
		s.server.IdleTimeout = idle
	}
}

// NewServer creates a new HTTP server
func NewServer(name string, options ...ServerOption) *Server {
	mux := http.NewServeMux()

	server := &Server{
		name: name,
		addr: ":8080", // Default port
		mux:  mux,
		server: &http.Server{
			Handler:      otelhttp.NewHandler(mux, name),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  30 * time.Second,
		},
		logger: logging.NewLogger(name),
		readyFn: func() bool {
			return true
		},
	}

	for _, option := range options {
		option(server)
	}

	// Update server address
	server.server.Addr = server.addr

	return server
}

// Handle registers a handler for a given pattern
func (s *Server) Handle(pattern string, handler http.Handler) {
	// Wrap handler with OpenTelemetry instrumentation
	wrappedHandler := otelhttp.NewHandler(handler, pattern)
	s.mux.Handle(pattern, wrappedHandler)
}

// HandleFunc registers a handler function for a given pattern
func (s *Server) HandleFunc(pattern string, handlerFunc http.HandlerFunc) {
	// Wrap handler function with OpenTelemetry instrumentation
	wrappedHandler := otelhttp.NewHandler(handlerFunc, pattern)
	s.mux.Handle(pattern, wrappedHandler)
}

// RegisterHealthChecks registers health check endpoints
func (s *Server) RegisterHealthChecks(livenessCheckers, readinessCheckers []health.Checker) {
	health.RegisterHealthEndpoints(s.mux, livenessCheckers, readinessCheckers)
}

// Start starts the server
func (s *Server) Start() error {
	go s.waitForShutdown()

	s.logger.Info(context.Background(), fmt.Sprintf("Starting %s server on %s", s.name, s.addr))

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server failed to start: %w", err)
	}

	return nil
}

// waitForShutdown waits for a shutdown signal
func (s *Server) waitForShutdown() {
	// Create a channel to receive signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Wait for a signal
	sig := <-stop
	s.logger.Info(context.Background(), fmt.Sprintf("Received signal %s, shutting down...", sig))

	// Create a deadline context for shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Attempt graceful shutdown
	if err := s.server.Shutdown(ctx); err != nil {
		s.logger.Error(ctx, "Server shutdown error", "error", err)
	}

	s.logger.Info(context.Background(), "Server stopped")
}

// Stop stops the server
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return s.server.Shutdown(ctx)
}

// Middleware is a function that wraps an http.Handler
type Middleware func(http.Handler) http.Handler

// UseMiddleware applies middleware to the server
func (s *Server) UseMiddleware(middleware ...Middleware) {
	// Create a chain of middleware
	var handler http.Handler = s.mux

	// Apply middleware in reverse order
	for i := len(middleware) - 1; i >= 0; i-- {
		handler = middleware[i](handler)
	}

	// Update the server handler
	s.server.Handler = handler
}

// LoggingMiddleware creates a middleware that logs requests
func LoggingMiddleware(logger *logging.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Create a response wrapper to capture the status code
			wrapper := newResponseWriter(w)

			// Process the request
			next.ServeHTTP(wrapper, r)

			// Log the request
			duration := time.Since(start)
			logger.Info(r.Context(),
				fmt.Sprintf("%s %s %d %s",
					r.Method,
					r.URL.Path,
					wrapper.statusCode,
					duration,
				),
				"method", r.Method,
				"path", r.URL.Path,
				"status", wrapper.statusCode,
				"duration_ms", duration.Milliseconds(),
				"user_agent", r.UserAgent(),
			)
		})
	}
}

// responseWriter is a wrapper for http.ResponseWriter that captures the status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

// newResponseWriter creates a new responseWriter
func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{w, http.StatusOK}
}

// WriteHeader captures the status code
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
