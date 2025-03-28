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
	"strconv"
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

// Global variables
var (
	natsConn *connections.NATSConnection
	logger   *logging.Logger
	tracer   = tracing.GetTracer("enqueuer")
)

// publishToNATS publishes a message to NATS with tracing
func publishToNATS(ctx context.Context, queueName string, msg Message) error {
	ctx, span := tracer.Start(ctx, "nats.publish")
	defer span.End()

	// Add environment prefix to queue name
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev" // Default to dev if ENV not set
	}

	// Apply environment prefix to queue name
	prefixedQueueName := fmt.Sprintf("%s.%s", env, queueName)

	span.SetAttributes(
		attribute.String("queue.name", prefixedQueueName),
		attribute.String("message.id", msg.ID),
	)

	// Ensure Headers map exists
	if msg.Headers == nil {
		msg.Headers = make(map[string]string)
	}

	// Inject current trace context into message headers
	tracing.InjectTraceContext(ctx, msg.Headers)

	logger.Debug(ctx, "Injected trace context into message headers")

	if url, ok := msg.Headers["image_url"]; ok {
		logger.Info(ctx, "Publishing message with image URL",
			"queue", prefixedQueueName,
			"message_id", msg.ID,
			"image_url", url)
		span.SetAttributes(attribute.String("message.image_url", url))
	}

	// Convert message to JSON
	data, err := json.Marshal(msg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to marshal message to JSON")
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Publish message to NATS with prefixed queue name
	err = natsConn.PublishWithTracing(ctx, prefixedQueueName, data)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to publish message to NATS")
		return fmt.Errorf("failed to publish message: %w", err)
	}

	span.SetStatus(codes.Ok, "Message published successfully")
	logger.Info(ctx, "Message published", "queue", prefixedQueueName, "original_queue", queueName, "message_id", msg.ID)
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

	if msg.ID == "" {
		// Generate ID if not provided
		msg.ID = fmt.Sprintf("msg-%d", time.Now().UnixNano())
		logger.Debug(ctx, "Generated message ID", "id", msg.ID)
	}

	// Add more detailed logging for the image URL
	logger.Debug(ctx, "Adding image URL to message", "id", msg.ID, "image_url", imageURL)
	// Add the image URL to the message
	if msg.Headers == nil {
		msg.Headers = make(map[string]string)
	}
	msg.Headers["image_url"] = imageURL

	// Publish to NATS (will now use prefixed queue name)
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
		"queue":     queueName, // Return the original queue name to the client
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

	// Create browser instance
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

	// Create page
	var page *rod.Page
	err = rod.Try(func() {
		page = browser.MustPage()
	})

	if err != nil {
		logger.Error(ctx, "Failed to create page", "error", fmt.Sprintf("%v", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Page created successfully")

	// Navigate to Google Images search
	searchURL := "https://www.google.com/search?tbm=isch&q=" + url.QueryEscape(query)
	logger.Info(ctx, "Navigating to search URL", "url", searchURL)

	err = rod.Try(func() {
		page.MustNavigate(searchURL)
	})

	if err != nil {
		logger.Error(ctx, "Failed to navigate to URL", "url", searchURL, "error", fmt.Sprintf("%v", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Successfully navigated to URL", "url", searchURL)

	// Wait for page to stabilize
	err = rod.Try(func() {
		err = page.WaitStable(2 * time.Second)
	})

	if err != nil {
		logger.Error(ctx, "Failed to wait for page stability", "error", fmt.Sprintf("%v", err))
	}

	// Try to handle cookie consent dialogs
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
			logger.Info(ctx, "Clicked consent button", "selector", selector)
		})
		if err == nil {
			break
		}
	}

	// Wait a bit for the page to load images
	time.Sleep(1 * time.Second)
	err = rod.Try(func() {
		page.MustWaitLoad()
	})

	if err != nil {
		logger.Error(ctx, "Failed to wait for page load", "error", fmt.Sprintf("%v", err))
	}

	var imgURL string

	err = rod.Try(func() {
		elements, err := page.Elements("img[src^='data:image/']")
		if err != nil {
			logger.Error(ctx, "Failed to find base64 image elements", "error", err)
			return
		}
		logger.Debug(ctx, "Found base64 images", "count", len(elements))
		if len(elements) > 0 {
			for i, el := range elements {
				// DEBUGGING SESSION
				src, _ := el.Attribute("src")
				alt, _ := el.Attribute("alt")
				height, _ := el.Attribute("height")
				width, _ := el.Attribute("width")
				class, _ := el.Attribute("class")
				id, _ := el.Attribute("id")
				srcValue := "nil"
				if src != nil {
					if len(*src) > 100 {
						srcValue = (*src)[:20] + "..."
					} else {
						srcValue = *src
					}
				}
				altValue := "nil"
				if alt != nil {
					altValue = *alt
				}
				heightValue := "nil"
				if height != nil {
					heightValue = *height
				}
				widthValue := "nil"
				if width != nil {
					widthValue = *width
				}
				classValue := "nil"
				if class != nil {
					classValue = *class
				}
				idValue := "nil"
				if id != nil {
					idValue = *id
				}
				html, _ := el.HTML()
				intheight, err := strconv.Atoi(heightValue)
				if err != nil {
					logger.Error(ctx, "Error trying to get image heigh")
				}
				intwidth, err := strconv.Atoi(widthValue)
				if err != nil {
					logger.Error(ctx, "Error trying to get image width")
				}
				if intheight < 100 || intwidth < 100 {
					logger.Debug(ctx, "Small element, skipping and selecting next one...")
					continue
				}
				logger.Debug(ctx, "Image element details",
					"index", i,
					"src", srcValue,
					"alt", altValue,
					"height", heightValue,
					"width", widthValue,
					"class", classValue,
					"id", idValue,
					"html", html)
				// DEBUGGING

				logger.Debug(ctx, "Clicking on image to open full-size view")
				err = rod.Try(func() {
					el.MustClick()
				})
				if err != nil {
					break
				}
				logger.Debug(ctx, "Successfully clicked first element")
				page.MustWaitLoad()

				html, err = page.HTML()
				if err == nil {
					imgURLPattern := `imgurl=(http[^&"]+)`
					re := regexp.MustCompile(imgURLPattern)
					matches := re.FindStringSubmatch(html)
					if len(matches) > 1 {
						encodedURL := matches[1]
						decodedURL, err := url.QueryUnescape(encodedURL)
						if err == nil {
							imgURL = decodedURL
							logger.Debug(ctx, "Found image URL from HTML regex", "url", imgURL)
							break
						}
					}
				}
				maxAttempts := 3
				var success bool

				for attempt := 1; attempt <= maxAttempts; attempt++ {
					err = rod.Try(func() {
						page.MustScreenshot("after_click.png")
						logger.Debug(ctx, "Took screenshot after clicking image")
						success = true
					})

					if success {
						break
					}

					if err != nil {
						logger.Error(ctx, fmt.Sprintf("Error taking screenshot after clicking image (attempt %d/%d): ", attempt, maxAttempts), "error", err)
						if attempt == maxAttempts {
							logger.Error(ctx, "Failed to take screenshot after 10 attempts")
							break
						}
						time.Sleep(1000 * time.Millisecond)
					}
				}
				if imgURL != "" {
					break
				}
			}
		}
	})
	if err != nil {
		logger.Error(ctx, "Failed to select base64 image element", "error", err)
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
		logging.WithMinLevel(logging.Info),
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
		[]health.Checker{natsConn},
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
