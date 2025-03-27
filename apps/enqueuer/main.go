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
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/fullstack-pw/shared/connections"
	"github.com/fullstack-pw/shared/health"
	"github.com/fullstack-pw/shared/logging"
	"github.com/fullstack-pw/shared/server"
	"github.com/fullstack-pw/shared/tracing"
	"github.com/go-rod/rod"

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

	browser := rod.New().
		Timeout(20 * time.Second).
		MustConnect()
	defer browser.MustClose()

	page := browser.MustPage()

	searchURL := "https://www.google.com/search?tbm=isch&q=" + url.QueryEscape(query)

	err := page.Navigate(searchURL)
	if err != nil {
		logger.Error(ctx, "Failed to navigate to Google Images", "error", err)
	}

	err = page.WaitStable(2 * time.Second)
	if err != nil {
		logger.Error(ctx, "Failed to wait for page to be stable", "error", err)
	}

	consentButtonSelectors := []string{
		"button#L2AGLb",
		"[aria-label='Accept all']",
		"[aria-label='Aceitar tudo']",
		"button.tHlp8d",
		"form:nth-child(2) button",
		"div.VDity button:first-child",
		"button:contains('Accept')",
		"button:contains('I agree')",
	}

	for _, selector := range consentButtonSelectors {
		err := rod.Try(func() {
			el := page.MustElement(selector)
			el.MustClick()
		})
		if err == nil {
			logger.Info(ctx, "Accepted cookies using selector", "selector", selector)
			break
		}
	}

	time.Sleep(1 * time.Second)
	page.MustWaitLoad()

	var imgURL string

	// Look specifically for images with base64 src attributes
	err = rod.Try(func() {
		elements, err := page.Elements("img[src^='data:image/']")
		if err != nil {
			logger.Error(ctx, "Failed to find base64 image elements", "error", err)
			return
		}

		logger.Debug(ctx, "Found base64 images", "count", len(elements))

		if len(elements) > 0 {
			var targetElement *rod.Element

			// DUMP ALL DEBUG
			// for i, el := range elements {
			// 	logger.Debug(ctx, "Using image", "index", i)
			// 	src, _ := el.Attribute("src")
			// 	alt, _ := el.Attribute("alt")
			// 	class, _ := el.Attribute("class")
			// 	id, _ := el.Attribute("id")

			// 	srcValue := "nil"
			// 	if src != nil {
			// 		// Truncate base64 data to prevent log flooding
			// 		if len(*src) > 100 {
			// 			srcValue = (*src)[:20] + "..."
			// 		} else {
			// 			srcValue = *src
			// 		}
			// 	}

			// 	altValue := "nil"
			// 	if alt != nil {
			// 		altValue = *alt
			// 	}

			// 	classValue := "nil"
			// 	if class != nil {
			// 		classValue = *class
			// 	}

			// 	idValue := "nil"
			// 	if id != nil {
			// 		idValue = *id
			// 	}

			// 	// Also try to get the HTML content
			// 	html, _ := el.HTML()

			// 	logger.Debug(ctx, "Image element details",
			// 		"index", i,
			// 		"src", srcValue,
			// 		"alt", altValue,
			// 		"class", classValue,
			// 		"id", idValue,
			// 		"html", html)

			// 	targetElement = el
			// 	break
			// }

			// if targetElement == nil && len(elements) > 0 {
			// 	logger.Debug(ctx, "Using first available image as fallback")
			// 	targetElement = elements[0]
			// }

			// Click on the image to open the full-size view
			logger.Debug(ctx, "Clicking on image to open full-size view")
			targetElement.MustClick()

			// Wait for the modal to load
			time.Sleep(3 * time.Second)

			// Take a screenshot after clicking
			page.MustScreenshot("after_click.png")
			logger.Debug(ctx, "Took screenshot after clicking image", "file", "after_click.png")

			// Now try to find the full-size image URL from the modal
			// There are several possible selectors for the full-size image
			fullSizeSelectors := []struct {
				selector string
				attr     string
			}{
				{".n3VNCb", "src"},               // Common selector for full-size image
				{".KAlRDb", "src"},               // Alternative selector
				{"img.iPVvYb", "src"},            // Another common class
				{"img.r48jcc", "src"},            // Another image class
				{"img.tXlD8e", "src"},            // Yet another image class
				{"img.pxhXjf", "src"},            // One more image class
				{".tvh9oe", "src"},               // Container with image
				{".eJH8qd img", "src"},           // Parent container with image
				{"a[href^='http'] img", "src"},   // Image inside a link
				{".islrc.isv-r img", "data-src"}, // Data-src attribute
			}

			for _, sel := range fullSizeSelectors {
				err := rod.Try(func() {
					el := page.MustElement(sel.selector)
					attr, err := el.Attribute(sel.attr)
					if err == nil && attr != nil && *attr != "" {
						value := *attr
						// Skip base64 images and Google logos
						if !strings.HasPrefix(value, "data:") &&
							!strings.Contains(value, "googlelogo") &&
							!strings.Contains(value, "gstatic.com") {
							imgURL = value
							logger.Debug(ctx, "Found full-size image URL",
								"selector", sel.selector,
								"attribute", sel.attr,
								"url", imgURL)
						}
					}
				})

				if err == nil && imgURL != "" {
					break
				}
			}

			// If we still don't have an image URL, try to find it in any img tag
			if imgURL == "" {
				logger.Debug(ctx, "Trying to find any image with a valid src in the modal")
				elements, err := page.Elements("img[src^='http']")
				if err == nil && len(elements) > 0 {
					for _, el := range elements {
						src, err := el.Attribute("src")
						if err == nil && src != nil && *src != "" {
							// Skip Google UI elements
							if !strings.Contains(*src, "googlelogo") &&
								!strings.Contains(*src, "gstatic.com") &&
								!strings.Contains(*src, "ui/") {
								imgURL = *src
								logger.Debug(ctx, "Found image from generic search", "url", imgURL)
								break
							}
						}
					}
				}
			}

		}
	})
	if err != nil {
		logger.Error(ctx, "Failed to select base64 image element", "error", err)
	}

	if imgURL == "" {
		html, err := page.HTML()
		if err == nil {
			// Look for imgurl parameter in the HTML
			imgURLPattern := `imgurl=(http[^&"]+)`
			re := regexp.MustCompile(imgURLPattern)
			matches := re.FindStringSubmatch(html)
			if len(matches) > 1 {
				encodedURL := matches[1]
				decodedURL, err := url.QueryUnescape(encodedURL)
				if err == nil {
					imgURL = decodedURL
					logger.Debug(ctx, "Found image URL from HTML regex", "url", imgURL)
				}
			}
		}
	}

	logger.Info(ctx, "Returning image URL", "url", imgURL)
	return imgURL, nil
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
