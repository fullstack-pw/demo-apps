package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fullstack-pw/shared/connections"
	"github.com/fullstack-pw/shared/health"
	"github.com/fullstack-pw/shared/logging"
	"github.com/fullstack-pw/shared/server"
	"github.com/fullstack-pw/shared/tracing"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Message represents the data structure we'll enqueue
type Message struct {
	ID      string            `json:"id,omitempty"`
	Content string            `json:"content"`
	Headers map[string]string `json:"headers,omitempty"`
}

var (
	// Global variables
	natsConn *connections.NATSConnection
	logger   *logging.Logger
	tracer   = tracing.GetTracer("enqueuer")
)

// publishToNATS publishes a message to NATS with tracing
func publishToNATS(ctx context.Context, queueName string, msg Message) error {
	ctx, span := tracer.Start(ctx, "nats.publish")
	defer span.End()

	span.SetAttributes(
		attribute.String("queue.name", queueName),
		attribute.String("message.id", msg.ID),
	)

	// Ensure Headers map exists
	if msg.Headers == nil {
		msg.Headers = make(map[string]string)
	}

	// Inject current trace context into message headers
	tracing.InjectTraceContext(ctx, msg.Headers)

	logger.Debug(ctx, "Injected trace context into message headers")

	// Convert message to JSON
	data, err := json.Marshal(msg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to marshal message to JSON")
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Publish message to NATS
	err = natsConn.PublishWithTracing(ctx, queueName, data)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to publish message to NATS")
		return fmt.Errorf("failed to publish message: %w", err)
	}

	span.SetStatus(codes.Ok, "Message published successfully")
	logger.Info(ctx, "Message published", "queue", queueName, "message_id", msg.ID)
	return nil
}

// handleAdd handles the /add endpoint
// Modified handleAdd function
func handleAdd(w http.ResponseWriter, r *http.Request) {
	// Create a context for this request
	ctx := r.Context()
	logger.Debug(ctx, "Handling request", "method", r.Method, "path", r.URL.Path)

	// Only accept POST requests
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Error(ctx, "Failed to read request body", "error", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Parse message
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		logger.Error(ctx, "Invalid JSON payload", "error", err)
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// Extract queue name from query parameter or use default
	queueName := r.URL.Query().Get("queue")
	if queueName == "" {
		queueName = "default"
	}

	// Search Google Images for the content
	imageURL, err := searchGoogleImages(ctx, msg.Content)
	if err != nil {
		logger.Error(ctx, "Failed to search Google Images", "error", err)
		http.Error(w, fmt.Sprintf("Failed to search Google Images: %v", err), http.StatusInternalServerError)
		return
	}

	// Add the image URL to the message
	if msg.Headers == nil {
		msg.Headers = make(map[string]string)
	}
	msg.Headers["image_url"] = imageURL

	// Publish to NATS
	err = publishToNATS(ctx, queueName, msg)
	if err != nil {
		logger.Error(ctx, "Failed to publish message", "error", err, "queue", queueName)
		http.Error(w, fmt.Sprintf("Failed to publish message: %v", err), http.StatusInternalServerError)
		return
	}

	// Return success response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	response := map[string]string{
		"status":    "queued",
		"queue":     queueName,
		"image_url": imageURL,
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Error(ctx, "Error encoding response", "error", err)
	}

	logger.Info(ctx, "Request handled successfully", "queue", queueName, "message_id", msg.ID, "image_url", imageURL)
}

// handleCheckMemorizer checks if a message was processed by memorizer
func handleCheckMemorizer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	messageId := r.URL.Query().Get("id")
	if messageId == "" {
		http.Error(w, "Missing message ID", http.StatusBadRequest)
		return
	}

	logger.Debug(ctx, "Checking memorizer status", "message_id", messageId)

	// Use internal service discovery to check with memorizer
	resp, err := http.Get(fmt.Sprintf("http://memorizer.default.svc.cluster.local:8080/status?id=%s", messageId))
	if err != nil {
		logger.Error(ctx, "Error contacting memorizer", "error", err)
		http.Error(w, fmt.Sprintf("Error contacting memorizer: %v", err), http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			// Handle the error, for example:
			log.Printf("Error closing response body: %v", err)
		}
	}()

	// Forward the response from memorizer
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logger.Error(ctx, "Error copying response", "error", err)
	}
}

// handleCheckWriter checks if a message was written by writer
func handleCheckWriter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	messageId := r.URL.Query().Get("id")
	if messageId == "" {
		http.Error(w, "Missing message ID", http.StatusBadRequest)
		return
	}

	logger.Debug(ctx, "Checking writer status", "message_id", messageId)

	// Use internal service discovery to check with writer
	resp, err := http.Get(fmt.Sprintf("http://writer.default.svc.cluster.local:8080/query?id=%s", messageId))
	if err != nil {
		logger.Error(ctx, "Error contacting writer", "error", err)
		http.Error(w, fmt.Sprintf("Error contacting writer: %v", err), http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			// Handle the error, for example:
			log.Printf("Error closing response body: %v", err)
		}
	}()

	// Forward the response from writer
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logger.Error(ctx, "Error copying response", "error", err)
	}
}

func searchGoogleImages(ctx context.Context, query string) (string, error) {
	// Create a span for tracing
	ctx, span := tracer.Start(ctx, "google.image_search", trace.WithAttributes(
		attribute.String("search.query", query),
	))
	defer span.End()

	// Find the Chrome binary path
	chromePath := os.Getenv("CHROME_PATH")
	if chromePath == "" {
		chromePath = "/usr/bin/google-chrome" // Default path for our container
	}

	logger.Info(ctx, "Using Chrome binary from path", "path", chromePath)

	// Check if the chrome binary exists and log details
	if fileInfo, err := os.Stat(chromePath); os.IsNotExist(err) {
		logger.Error(ctx, "Chrome binary not found at specified path", "path", chromePath)
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	} else {
		logger.Info(ctx, "Chrome binary details",
			"size", fileInfo.Size(),
			"mode", fileInfo.Mode().String(),
			"modified", fileInfo.ModTime())
	}

	// Log directory where Chrome is located to check permissions
	chromeDir := filepath.Dir(chromePath)
	if dirInfo, err := os.Stat(chromeDir); err != nil {
		logger.Error(ctx, "Failed to get Chrome directory info", "dir", chromeDir, "error", err)
	} else {
		logger.Info(ctx, "Chrome directory permissions",
			"dir", chromeDir,
			"mode", dirInfo.Mode().String())
	}

	// Use a very basic approach - create a launcher with just essential flags
	logger.Info(ctx, "Creating Chrome launcher")
	u, err := launcher.New().
		Bin(chromePath).
		Headless(true).
		Set("no-sandbox", "").
		Set("disable-dev-shm-usage", "").
		Set("disable-gpu", "").
		Launch()

	if err != nil {
		logger.Error(ctx, "Failed to launch Chrome", "error", fmt.Sprintf("%v", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Chrome launched successfully", "control_url", u)

	// Try with the simplest browser creation
	logger.Info(ctx, "Connecting to browser")
	browser := rod.New().
		ControlURL(u).
		Timeout(30 * time.Second)

	err = browser.Connect()
	if err != nil {
		logger.Error(ctx, "Failed to connect to Chrome", "error", fmt.Sprintf("%v", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Successfully connected to browser")
	defer browser.Close()

	// Try the very simplest page creation approach
	logger.Info(ctx, "Creating page")
	var page *rod.Page
	err = rod.Try(func() {
		page = browser.MustPage()
	})

	if err != nil {
		logger.Error(ctx, "Failed to create page with MustPage()",
			"error", fmt.Sprintf("%v", err),
			"error_type", fmt.Sprintf("%T", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	if page == nil {
		logger.Error(ctx, "Page is nil after creation")
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Page created successfully")

	// Very simple navigation with enhanced error reporting
	searchURL := "https://www.google.com/search?tbm=isch&q=" + url.QueryEscape(query)
	logger.Info(ctx, "Navigating to search URL", "url", searchURL)

	err = rod.Try(func() {
		page.MustNavigate(searchURL)
	})

	if err != nil {
		logger.Error(ctx, "Failed to navigate to URL",
			"url", searchURL,
			"error", fmt.Sprintf("%v", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Successfully navigated to URL", "url", searchURL)

	// Instead of doing complex processing, just return a placeholder for now
	// We can incrementally add the original functionality once we get this working
	placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
	logger.Info(ctx, "Returning placeholder URL for now", "url", placeholderURL)
	return placeholderURL, nil
}

func main() {
	// Create context for the application
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize the logger
	logger = logging.NewLogger("enqueuer",
		logging.WithMinLevel(logging.Debug),
		logging.WithJSONFormat(true),
	)

	// Initialize the tracer
	tracerCfg := tracing.DefaultConfig("enqueuer")
	tp, err := tracing.InitTracer(tracerCfg)
	if err != nil {
		logger.Fatal(ctx, "Failed to initialize tracer", "error", err)
	}
	defer func() {
		if err := tracing.ShutdownTracer(ctx, tp); err != nil {
			// Handle the error, for example:
			log.Printf("Error shutting down tracer: %v", err)
		}
	}()

	// Initialize NATS connection
	natsConn = connections.NewNATSConnection()
	if err := natsConn.Connect(ctx); err != nil {
		logger.Fatal(ctx, "Failed to connect to NATS", "error", err)
	}
	defer natsConn.Close()

	// Create a server with logging middleware
	srv := server.NewServer("enqueuer",
		server.WithLogger(logger),
	)

	// Use middleware
	srv.UseMiddleware(server.LoggingMiddleware(logger))

	// Register handlers
	srv.HandleFunc("/add", handleAdd)
	srv.HandleFunc("/check-memorizer", handleCheckMemorizer)
	srv.HandleFunc("/check-writer", handleCheckWriter)
	srv.HandleFunc("/natscheck", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger.Debug(ctx, "Handling NATS check", "method", r.Method, "path", r.URL.Path)

		if !natsConn.IsConnected() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err := w.Write([]byte("NATS connection lost"))
			if err != nil {
				logger.Error(ctx, "Error writing response", "error", err)
			}
			return
		}

		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("NATS connection OK"))
		if err != nil {
			logger.Error(ctx, "Error writing response", "error", err)
		}
	})
	// Register health checks
	srv.RegisterHealthChecks(
		[]health.Checker{natsConn}, // Liveness checks
		[]health.Checker{natsConn}, // Readiness checks
	)

	// Set up signal handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start server in a goroutine
	go func() {
		logger.Info(ctx, "Starting enqueuer service", "port", 8080)
		if err := srv.Start(); err != nil {
			logger.Fatal(ctx, "Server failed", "error", err)
		}
	}()

	// Wait for termination signal
	<-stop

	// Shut down gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
	defer shutdownCancel()

	logger.Info(shutdownCtx, "Shutting down enqueuer service")
}
