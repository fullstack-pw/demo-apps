package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fullstack-pw/shared/connections"
	"github.com/fullstack-pw/shared/health"
	"github.com/fullstack-pw/shared/logging"
	"github.com/fullstack-pw/shared/server"
	"github.com/fullstack-pw/shared/tracing"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Message represents the data structure coming from the queue
type Message struct {
	ID      string            `json:"id,omitempty"`
	Content string            `json:"content"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Global variables
var (
	natsConn           *connections.NATSConnection
	redisConn          *connections.RedisConnection
	logger             *logging.Logger
	tracer             = tracing.GetTracer("memorizer")
	asciiConverterPath string               // Path to the ASCII converter script
	subscriptions      []*nats.Subscription // Store subscriptions for cleanup
)

// Update the function signature to include headers
func storeInRedis(ctx context.Context, id string, content string, headers map[string]string) error {
	// Create a child span for the Redis operation
	ctx, span := tracer.Start(ctx, "redis.store", trace.WithAttributes(
		attribute.String("db.system", "redis"),
		attribute.String("db.operation", "set"),
		attribute.String("message.id", id),
	))
	defer span.End()

	span.SetAttributes(
		attribute.String("message.content", content),
	)

	logger.Debug(ctx, "Storing message in Redis", "id", id)

	// Generate a key for Redis storage
	// Using message ID if provided, otherwise generate a timestamp-based key
	key := id
	if key == "" {
		key = fmt.Sprintf("msg:%d", time.Now().UnixNano())
		span.SetAttributes(attribute.String("generated.key", key))
		logger.Debug(ctx, "Generated key for message without ID", "key", key)
	}

	// Create a message with trace context to store in Redis
	redisMsg := Message{
		ID:      id,
		Content: content,
		Headers: make(map[string]string),
	}

	// Copy original headers if provided
	for k, v := range headers {
		redisMsg.Headers[k] = v
		// Log if we find an image URL
		if k == "image_url" {
			logger.Info(ctx, "Found image URL in headers", "id", id, "image_url", v)
			span.SetAttributes(attribute.String("message.image_url", v))
		}
	}

	// Inject current trace context into Redis message headers
	tracing.InjectTraceContext(ctx, redisMsg.Headers)
	if url, ok := redisMsg.Headers["image_url"]; ok {
		logger.Info(ctx, "Storing message with image URL in Redis",
			"id", id,
			"key", key,
			"image_url", url)
		span.SetAttributes(attribute.String("message.image_url", url))
	}
	// Convert to JSON for storage
	msgJSON, err := json.Marshal(redisMsg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to marshal message with trace context")
		logger.Error(ctx, "Failed to marshal message", "error", err)
		return fmt.Errorf("failed to marshal message with trace context: %w", err)
	}

	// Store the JSON in Redis instead of raw content
	expiration := 24 * time.Hour // Keep messages for 24 hours
	err = redisConn.SetWithTracing(ctx, key, string(msgJSON), expiration)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to store message in Redis")
		logger.Error(ctx, "Failed to store message in Redis", "error", err, "key", key)
		return fmt.Errorf("failed to store message in Redis: %w", err)
	}

	span.SetStatus(codes.Ok, "Message stored successfully")
	logger.Info(ctx, "Message stored in Redis", "key", key, "id", id)
	return nil
}

// downloadImage downloads an image from a URL and saves it to a temporary file
func downloadImage(ctx context.Context, imageURL string) (string, error) {
	ctx, span := tracer.Start(ctx, "image.download", trace.WithAttributes(
		attribute.String("image.url", imageURL),
	))
	defer span.End()

	logger.Info(ctx, "Downloading image", "url", imageURL)

	// Create HTTP request with context
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to create HTTP request")
		logger.Error(ctx, "Failed to create HTTP request", "error", err, "url", imageURL)
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set user agent to avoid being blocked
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")

	// Execute request
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to download image")
		logger.Error(ctx, "Failed to download image", "error", err, "url", imageURL)
		return "", fmt.Errorf("failed to download image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(codes.Error, "Invalid response status")
		logger.Error(ctx, "Invalid response status", "status", resp.StatusCode, "url", imageURL)
		return "", err
	}

	// Check content type
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		err := fmt.Errorf("unexpected content type: %s", contentType)
		span.RecordError(err)
		span.SetStatus(codes.Error, "Invalid content type")
		logger.Error(ctx, "Invalid content type", "content_type", contentType, "url", imageURL)
		return "", err
	}

	// Create temporary file
	tempFile, err := os.CreateTemp("", "image-*.jpg")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to create temporary file")
		logger.Error(ctx, "Failed to create temporary file", "error", err)
		return "", fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer func() { _ = tempFile.Close() }()

	// Save image to temporary file
	_, err = io.Copy(tempFile, resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to save image")
		logger.Error(ctx, "Failed to save image to file", "error", err, "path", tempFile.Name())
		return "", fmt.Errorf("failed to save image: %w", err)
	}

	logger.Info(ctx, "Image downloaded successfully", "url", imageURL, "path", tempFile.Name())
	span.SetStatus(codes.Ok, "Image downloaded successfully")
	return tempFile.Name(), nil
}

func publishResultsToNATS(ctx context.Context, messageId string, traceId string, asciiTerminal, asciiHTML string, headers map[string]string) error {
	ctx, span := tracer.Start(ctx, "nats.publish_results")
	defer span.End()

	// Get environment
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev" // Default to dev if ENV not set
	}

	// Create response queue name with environment prefix
	responseQueue := fmt.Sprintf("%s.result-queue", env)

	// Create response payload
	response := map[string]interface{}{
		"message_id":     messageId,
		"trace_id":       traceId,
		"ascii_terminal": asciiTerminal,
		"ascii_html":     asciiHTML,
		"headers":        headers,
	}

	// Convert to JSON
	responseData, err := json.Marshal(response)
	if err != nil {
		span.RecordError(err)
		logger.Error(ctx, "Failed to marshal response message", "error", err)
		return fmt.Errorf("failed to marshal response: %w", err)
	}

	// Publish to NATS
	err = natsConn.PublishWithTracing(ctx, responseQueue, responseData)
	if err != nil {
		span.RecordError(err)
		logger.Error(ctx, "Failed to publish response message", "error", err)
		return fmt.Errorf("failed to publish response: %w", err)
	}

	logger.Info(ctx, "Published results to NATS",
		"queue", responseQueue,
		"message_id", messageId,
		"trace_id", traceId)
	return nil
}

// handleMessage processes a message from NATS with tracing
func handleMessage(msg *nats.Msg) {
	// Extract trace context from message if available
	parentCtx := context.Background()
	var terminalAscii, htmlAscii string
	// Try to unmarshal the message to extract headers
	var message Message
	if err := json.Unmarshal(msg.Data, &message); err == nil && message.Headers != nil {
		// Extract trace context from headers
		parentCtx = tracing.ExtractTraceContext(context.Background(), message.Headers)
		logger.Debug(parentCtx, "Extracted trace context from message headers")
	} else {
		logger.Debug(parentCtx, "No trace context found in message, starting new trace")
	}

	// Now start the span with the extracted context
	ctx, span := tracer.Start(parentCtx, "nats.message.process")
	defer span.End()

	logger.Debug(ctx, "Received message", "data", string(msg.Data), "subject", msg.Subject)
	if message.Headers != nil {
		if imageURL, ok := message.Headers["image_url"]; ok {
			logger.Info(ctx, "Received message with image URL",
				"id", message.ID,
				"image_url", imageURL,
				"subject", msg.Subject)
			span.SetAttributes(attribute.String("message.image_url", imageURL))
		}
	}
	if url, ok := message.Headers["image_url"]; ok {
		logger.Info(ctx, "Found image URL in message", "id", message.ID, "image_url", url)
		span.SetAttributes(attribute.String("message.image_url", url))

		// Download the image
		imagePath, err := downloadImage(ctx, url)
		if err != nil {
			logger.Error(ctx, "Failed to download image", "error", err, "image_url", url)
			// Continue processing even if image download fails
		} else {
			logger.Info(ctx, "Image downloaded successfully", "id", message.ID, "path", imagePath)
			// Store the image path in the message headers for later steps
			message.Headers["image_path"] = imagePath
			// Convert image to ASCII art if we have a script path
			if asciiConverterPath != "" && message.Headers["image_path"] != "" {
				logger.Info(ctx, "Converting image to ASCII art", "id", message.ID, "image_path", message.Headers["image_path"])

				// Generate base filename for ASCII outputs
				baseFilename := strings.TrimSuffix(message.Headers["image_path"], filepath.Ext(message.Headers["image_path"]))

				// 1. Terminal mode (original functionality)
				terminalAscii, err = executeScript(ctx, asciiConverterPath, message.Headers["image_path"], "--mode", "terminal", "--columns", "80")
				if err != nil {
					logger.Error(ctx, "Failed to convert image to terminal ASCII art",
						"error", err,
						"id", message.ID,
						"image_path", message.Headers["image_path"])
					// Continue with other conversions even if this one fails
				} else {
					// Store the terminal ASCII art in Redis
					terminalKey := message.ID + ":ascii:terminal"
					logger.Info(ctx, "Storing terminal ASCII art in Redis",
						"id", message.ID,
						"key", terminalKey,
						"ascii_length", len(terminalAscii))

					// Print the ASCII art to logs
					fmt.Print(terminalAscii)

					// Store in Redis
					err = redisConn.SetWithTracing(ctx, terminalKey, terminalAscii, 24*time.Hour)
					if err != nil {
						logger.Error(ctx, "Failed to store terminal ASCII art in Redis",
							"error", err,
							"id", message.ID,
							"key", terminalKey)
					} else {
						logger.Info(ctx, "Terminal ASCII art stored successfully",
							"id", message.ID,
							"key", terminalKey)
						message.Headers["ascii_terminal_key"] = terminalKey
					}
				}

				// 2. HTML mode
				htmlFilename := baseFilename + "_ascii.html"
				htmlAscii, err = executeScript(ctx, asciiConverterPath, message.Headers["image_path"], "--mode", "html", "--columns", "80", "--output-file", htmlFilename)
				if err != nil {
					logger.Error(ctx, "Failed to convert image to HTML ASCII art",
						"error", err,
						"id", message.ID,
						"image_path", message.Headers["image_path"])
				} else {
					// Read the generated HTML file
					logger.Info(ctx, "HTLM generated ", htmlAscii)
					htmlContent, err := os.ReadFile(htmlFilename)
					htmlAscii = string(htmlContent)
					if err != nil {
						logger.Error(ctx, "Failed to read HTML ASCII file",
							"error", err,
							"id", message.ID,
							"file_path", htmlFilename)
					} else {
						// Store the HTML content in Redis
						htmlKey := message.ID + ":ascii:html"
						logger.Info(ctx, "Storing HTML ASCII art in Redis",
							"id", message.ID,
							"key", htmlKey,
							"file_path", htmlFilename,
							"content_length", len(htmlContent))

						err = redisConn.SetWithTracing(ctx, htmlKey, string(htmlContent), 24*time.Hour)
						if err != nil {
							logger.Error(ctx, "Failed to store HTML ASCII art in Redis",
								"error", err,
								"id", message.ID,
								"key", htmlKey)
						} else {
							logger.Info(ctx, "HTML ASCII art stored successfully",
								"id", message.ID,
								"key", htmlKey)
							message.Headers["ascii_html_key"] = htmlKey
						}

						// Clean up the HTML file
						_ = os.Remove(htmlFilename)
					}
				}

				// Clean up the temporary image file
				err = os.Remove(message.Headers["image_path"])
				if err != nil {
					logger.Warn(ctx, "Failed to remove temporary image file",
						"error", err,
						"path", message.Headers["image_path"])
				}
			}
		}
	}

	// Set attributes for the message
	span.SetAttributes(
		attribute.String("message.subject", msg.Subject),
		attribute.Int("message.size", len(msg.Data)),
	)

	// Parse message
	if err := json.Unmarshal(msg.Data, &message); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to parse message")
		logger.Error(ctx, "Error parsing message", "error", err, "data", string(msg.Data))
		return
	}

	// Extract trace context from headers if available
	if message.Headers != nil {
		span.SetAttributes(attribute.Int("headers.count", len(message.Headers)))
		for k, v := range message.Headers {
			span.SetAttributes(attribute.String("header."+k, v))
		}
	}

	// Store message in Redis
	if err := storeInRedis(ctx, message.ID, message.Content, message.Headers); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to store message")
		logger.Error(ctx, "Error storing message", "error", err, "id", message.ID)
		return
	}

	// Extract traceId for correlation
	traceId := tracing.GetTraceID(ctx)

	// Directly publish the results we already have in memory
	if err := publishResultsToNATS(ctx, message.ID, traceId, terminalAscii, htmlAscii, message.Headers); err != nil {
		span.RecordError(err)
		logger.Error(ctx, "Failed to publish results to NATS", "error", err)
	}

	span.SetStatus(codes.Ok, "Message processed successfully")
	logger.Info(ctx, "Message processed successfully", "id", message.ID)
}

// handleStatus handles the /status endpoint
func handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger.Debug(ctx, "Handling status request", "method", r.Method, "path", r.URL.Path)

	// Get message ID from query parameters
	id := r.URL.Query().Get("id")
	if id == "" {
		logger.Warn(ctx, "Missing ID parameter in status request")
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}

	// Check if the message exists in Redis
	ctx, span := tracer.Start(ctx, "redis.get")
	defer span.End()

	span.SetAttributes(attribute.String("message.id", id))

	// Apply environment prefix to key (same as SetWithTracing does)
	prefixedKey := redisConn.PrefixKey(id)

	// Check if the message exists in Redis using the prefixed key
	exists, err := redisConn.Client().Exists(ctx, prefixedKey).Result()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Redis query failed")
		logger.Error(ctx, "Error checking if message exists", "error", err, "id", id, "prefixed_key", prefixedKey)
		http.Error(w, "Error checking message status", http.StatusInternalServerError)
		return
	}

	// Build response
	response := map[string]interface{}{
		"id":        id,
		"processed": exists > 0,
	}

	// Return JSON response
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Error(ctx, "Error encoding response", "error", err)
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
		return
	}

	logger.Info(ctx, "Status check completed", "id", id, "exists", exists > 0)
}

// executeScript executes a Python script with the given arguments and returns the output
func executeScript(ctx context.Context, scriptPath string, args ...string) (string, error) {
	ctx, span := tracer.Start(ctx, "script.execute", trace.WithAttributes(
		attribute.String("script.path", scriptPath),
		attribute.StringSlice("script.args", args),
	))
	defer span.End()

	logger.Info(ctx, "Executing script", "path", scriptPath, "args", args)

	// Use the Python from our virtual environment
	pythonPath := "/opt/venv/bin/python3"

	// Check if the Python executable exists
	if _, err := os.Stat(pythonPath); os.IsNotExist(err) {
		// Fall back to system Python if virtual environment Python doesn't exist
		pythonPath = "python3"
		logger.Warn(ctx, "Virtual environment Python not found, falling back to system Python", "path", pythonPath)
	}

	// Prepare the command
	cmd := exec.CommandContext(ctx, pythonPath, append([]string{scriptPath}, args...)...)

	// Create buffers for stdout and stderr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run the command
	err := cmd.Run()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Script execution failed")
		logger.Error(ctx, "Script execution failed",
			"error", err,
			"stderr", stderr.String(),
			"path", scriptPath,
			"args", args,
		)
		return "", fmt.Errorf("script execution failed: %w\nstderr: %s", err, stderr.String())
	}

	output := stdout.String()
	logger.Info(ctx, "Script executed successfully",
		"path", scriptPath,
		"output_length", len(output),
	)

	// Log a preview of the output if it's not too large
	if len(output) > 0 && len(output) <= 500 {
		logger.Debug(ctx, "Script output", "output", output)
	} else if len(output) > 500 {
		logger.Debug(ctx, "Script output preview", "output", output[:500]+"...")
	}

	span.SetStatus(codes.Ok, "Script executed successfully")
	return output, nil
}

// setupAsciiConverter creates the Python script for ASCII conversion
func setupAsciiConverter() (string, error) {
	// Script content
	scriptContent := `#!/usr/bin/env python3
import sys
import os
import json
import ascii_magic
import argparse

def convert_image_to_ascii(image_path, mode='terminal', columns=80, back='black', output_file=None):
    try:
        # Check if file exists
        if not os.path.exists(image_path):
            raise FileNotFoundError(f"Image file not found: {image_path}")
            
        # Convert image to ASCII art
        output = ascii_magic.from_image(image_path)
        
        # Handle different output modes
        if mode == 'terminal':
            return output.to_terminal(columns=columns, back=back)
        elif mode == 'file':
            if output_file is None:
                output_file = f"{os.path.splitext(image_path)[0]}_ascii.txt"
            output.to_file(output_file, columns=columns)
            return f"ASCII art saved to {output_file}"
        elif mode == 'html':
            if output_file is None:
                output_file = f"{os.path.splitext(image_path)[0]}_ascii.html"
            output.to_html_file(output_file, columns=columns)
            return f"HTML ASCII art saved to {output_file}"
        else:
            raise ValueError(f"Invalid mode: {mode}. Must be 'terminal', 'file', or 'html'")
    except Exception as e:
        return json.dumps({"error": str(e)})

def main():
    parser = argparse.ArgumentParser(description='Convert image to ASCII art')
    parser.add_argument('image_path', help='Path to the image file')
    parser.add_argument('--mode', choices=['terminal', 'file', 'html'], default='terminal', 
                        help='Output mode: terminal, file, or html (default: terminal)')
    parser.add_argument('--columns', type=int, default=80, help='Width of the ASCII art in characters')
    parser.add_argument('--back', default='black', help='Background color')
    parser.add_argument('--output-file', help='Path for output file (used in file and html modes)')
    
    args = parser.parse_args()
    
    result = convert_image_to_ascii(args.image_path, args.mode, args.columns, args.back, args.output_file)

if __name__ == "__main__":
    main()
`

	// Create the scripts directory if it doesn't exist
	scriptsDir := "./scripts"
	if err := os.MkdirAll(scriptsDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create scripts directory: %w", err)
	}

	// Path for the script
	scriptPath := filepath.Join(scriptsDir, "ascii_converter.py")

	// Write the script to a file
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
		return "", fmt.Errorf("failed to write ASCII converter script: %w", err)
	}

	// Check if ascii_magic is installed, and install it if not
	// Use the virtual environment Python if available
	pythonPath := "/opt/venv/bin/python3"
	pipPath := "/opt/venv/bin/pip"

	if _, err := os.Stat(pipPath); os.IsNotExist(err) {
		// Fall back to system Python if virtual environment Python doesn't exist
		pythonPath = "python3"
		pipPath = "pip3"
	}

	// Try to import ascii_magic to check if it's installed
	cmd := exec.Command(pythonPath, "-c", "import ascii_magic")
	if err := cmd.Run(); err != nil {
		// Install ascii_magic if not already installed
		installCmd := exec.Command(pipPath, "install", "ascii_magic", "pillow")
		if err := installCmd.Run(); err != nil {
			return "", fmt.Errorf("failed to install ascii_magic: %w", err)
		}
	}

	return scriptPath, nil
}

func main() {
	// Create context for the application
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize the logger
	logger = logging.NewLogger("memorizer",
		logging.WithMinLevel(logging.Info),
		logging.WithJSONFormat(true),
	)

	// Initialize the tracer
	tracerCfg := tracing.DefaultConfig("memorizer")
	tp, err := tracing.InitTracer(tracerCfg)
	if err != nil {
		logger.Fatal(ctx, "Failed to initialize tracer", "error", err)
	}
	defer func() {
		if err := tracing.ShutdownTracer(ctx, tp); err != nil {
			logger.Error(ctx, "Error shutting down tracer", "error", err)
		}
	}()

	// Initialize NATS connection
	natsConn = connections.NewNATSConnection()
	if err := natsConn.Connect(ctx); err != nil {
		logger.Fatal(ctx, "Failed to connect to NATS", "error", err)
	}
	defer func() {
		natsConn.Close()
		logger.Info(ctx, "NATS connection closed")
	}()

	// Initialize Redis connection
	redisConn = connections.NewRedisConnection(connections.DefaultRedisConfig())
	if err := redisConn.Connect(ctx); err != nil {
		logger.Fatal(ctx, "Failed to connect to Redis", "error", err)
	}
	defer func() {
		if err := redisConn.Close(); err != nil {
			logger.Error(ctx, "Error closing Redis connection", "error", err)
		}
	}()

	// Subscribe to NATS queues
	queueNames := os.Getenv("QUEUE_NAMES")
	if queueNames == "" {
		queueNames = "default"
	}

	// Get environment for subject prefixing
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev" // Default to dev if ENV not set
	}

	// Split multiple queue names if present
	queuesList := strings.Split(queueNames, ",")
	for _, queue := range queuesList {
		queue = strings.TrimSpace(queue)
		if queue == "" {
			continue
		}

		// Apply environment prefix to queue name
		prefixedQueue := fmt.Sprintf("%s.%s", env, queue)

		// Subscribe to the prefixed queue
		logger.Info(ctx, "Subscribing to NATS queue", "queue", prefixedQueue, "original_queue", queue)
		sub, err := natsConn.SubscribeWithTracing(prefixedQueue, handleMessage)
		if err != nil {
			logger.Fatal(ctx, "Failed to subscribe to NATS queue", "error", err, "queue", prefixedQueue)
		}

		// Store the subscription for cleanup
		subscriptions = append(subscriptions, sub)
	}

	// Deferred cleanup for all subscriptions
	defer func() {
		for _, sub := range subscriptions {
			if err := sub.Unsubscribe(); err != nil {
				logger.Error(ctx, "Error unsubscribing from NATS", "error", err)
			}
		}
	}()

	// Set up ASCII converter script
	asciiConverterPath, err = setupAsciiConverter()
	if err != nil {
		logger.Error(ctx, "Failed to set up ASCII converter script", "error", err)
		// Continue without ASCII conversion capability
	} else {
		logger.Info(ctx, "ASCII converter script set up successfully", "path", asciiConverterPath)
	}

	// Create a server with logging middleware
	srv := server.NewServer("memorizer",
		server.WithLogger(logger),
	)

	// Use middleware
	srv.UseMiddleware(server.LoggingMiddleware(logger))

	// Register handlers
	srv.HandleFunc("/status", handleStatus)

	// Register health checks
	srv.RegisterHealthChecks(
		[]health.Checker{natsConn, redisConn},
		[]health.Checker{natsConn, redisConn}, // Readiness checks
	)

	// Set up signal handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start server in a goroutine
	go func() {
		logger.Info(ctx, "Starting memorizer service", "port", 8080)
		if err := srv.Start(); err != nil {
			logger.Fatal(ctx, "Server failed", "error", err)
		}
	}()

	// Wait for termination signal
	<-stop

	// Shut down gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
	defer shutdownCancel()

	logger.Info(shutdownCtx, "Shutting down memorizer service")
}
