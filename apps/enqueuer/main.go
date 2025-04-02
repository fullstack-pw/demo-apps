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
	"sync"
	"syscall"
	"time"

	"github.com/fullstack-pw/shared/connections"
	"github.com/fullstack-pw/shared/health"
	"github.com/fullstack-pw/shared/logging"
	"github.com/fullstack-pw/shared/server"
	"github.com/fullstack-pw/shared/tracing"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/nats-io/nats.go"

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
	natsConn    *connections.NATSConnection
	logger      *logging.Logger
	tracer      = tracing.GetTracer("enqueuer")
	browserPool *BrowserPool
)

// ResponseCache stores response messages by trace ID
type ResponseCache struct {
	mu      sync.Mutex
	results map[string]map[string]interface{}
	signals map[string]chan struct{}
}

// Global response cache
var responseCache = ResponseCache{
	results: make(map[string]map[string]interface{}),
	signals: make(map[string]chan struct{}),
}

// Add response to cache
func (rc *ResponseCache) Add(traceID string, response map[string]interface{}) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.results[traceID] = response

	// Signal to any waiting goroutines that the response has arrived
	if signal, ok := rc.signals[traceID]; ok {
		close(signal)
	}
}

// CORS middleware to allow cross-origin requests
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*") // Allow any origin
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Call the next handler
		next.ServeHTTP(w, r)
	})
}

// Get response from cache with timeout
func (rc *ResponseCache) GetWithTimeout(traceID string, timeout time.Duration) (map[string]interface{}, error) {
	// Check if result is already in cache
	rc.mu.Lock()
	if result, ok := rc.results[traceID]; ok {
		// Clean up after retrieval
		delete(rc.results, traceID)
		rc.mu.Unlock()
		return result, nil
	}

	// Create signal channel for this trace ID if none exists
	if _, ok := rc.signals[traceID]; !ok {
		rc.signals[traceID] = make(chan struct{})
	}
	signal := rc.signals[traceID]
	rc.mu.Unlock()

	// Wait for signal or timeout
	select {
	case <-signal:
		// Response arrived, get it from cache
		rc.mu.Lock()
		defer rc.mu.Unlock()

		result, ok := rc.results[traceID]
		if !ok {
			return nil, fmt.Errorf("result was signaled but not found in cache")
		}

		// Clean up
		delete(rc.results, traceID)
		delete(rc.signals, traceID)

		return result, nil

	case <-time.After(timeout):
		// Timeout occurred
		rc.mu.Lock()
		defer rc.mu.Unlock()

		// Clean up
		delete(rc.signals, traceID)

		return nil, fmt.Errorf("timed out waiting for response")
	}
}

// Clean up expired entries (should be called periodically)
func (rc *ResponseCache) CleanExpired(maxAge time.Duration) {
	// TODO Implementation would depend on when entries were added, we could add timestamps
	// For now, we'll skip this as we're cleaning up after retrieval
}

// BrowserPool manages a pool of browser instances
type BrowserPool struct {
	mu           sync.Mutex
	browserList  []*rod.Browser
	maxInstances int
	logger       *logging.Logger
}

// NewBrowserPool creates a new browser pool with configuration
func NewBrowserPool(maxInstances int, logger *logging.Logger) *BrowserPool {
	return &BrowserPool{
		browserList:  make([]*rod.Browser, 0, maxInstances),
		maxInstances: maxInstances,
		logger:       logger,
	}
}

// Status returns the current status of the pool
func (p *BrowserPool) Status() map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()

	return map[string]interface{}{
		"pool_size":     len(p.browserList),
		"max_instances": p.maxInstances,
	}
}

// Initialize initializes the browser pool
func (p *BrowserPool) Initialize(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Warm-up: create one browser instance to speed up the first request
	if p.maxInstances > 0 {
		p.logger.Info(ctx, "Warming up browser pool with one instance")
		browser, err := p.createBrowserInstance(ctx)
		if err != nil {
			p.logger.Error(ctx, "Failed to warm up browser pool", "error", err)
			return err
		}
		p.browserList = append(p.browserList, browser)
		p.logger.Info(ctx, "Browser pool initialized successfully", "instances", 1)
	}

	return nil
}

// createBrowserInstance creates a new browser instance
func (p *BrowserPool) createBrowserInstance(ctx context.Context) (*rod.Browser, error) {
	p.logger.Info(ctx, "Creating new browser instance")

	// Find the Chrome binary path
	chromePath := os.Getenv("CHROME_PATH")
	if chromePath == "" {
		chromePath = "/usr/bin/chromium-browser" // Default path for our container
	}

	// Create a new launcher instance each time (not reusing p.launch)
	launch := launcher.New().
		Bin(chromePath).
		Headless(true).
		Set("no-sandbox", "").
		Set("disable-dev-shm-usage", "").
		Set("disable-gpu", "")

	u, err := launch.Launch()
	if err != nil {
		return nil, fmt.Errorf("failed to launch browser: %w", err)
	}

	browser := rod.New().ControlURL(u).Timeout(30 * time.Second)
	err = browser.Connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to browser: %w", err)
	}

	p.logger.Info(ctx, "Browser instance created successfully")
	return browser, nil
}

// Get gets a browser instance from the pool or creates a new one
func (p *BrowserPool) Get(ctx context.Context) (*rod.Browser, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var browser *rod.Browser
	var err error

	// Try to get a browser from the pool
	if len(p.browserList) > 0 {
		browser = p.browserList[len(p.browserList)-1]
		p.browserList = p.browserList[:len(p.browserList)-1]
		p.logger.Debug(ctx, "Reusing browser instance from pool", "remaining", len(p.browserList))

		// Use a short context for health check
		checkCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		// Verify browser is still usable
		err := rod.Try(func() {
			// Just create a page and close it as a health check
			page := browser.Context(checkCtx).MustPage()
			page.MustClose()
		})

		if err != nil {
			p.logger.Warn(ctx, "Pooled browser instance is not usable, creating a new one", "error", fmt.Sprintf("%v", err))
			// Try to close the unusable browser
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = browser.Context(closeCtx).Close()
			closeCancel()
			browser = nil // Force creating a new one
		}
	}

	// Create a new browser if we didn't get a usable one from the pool
	if browser == nil {
		browser, err = p.createBrowserInstance(ctx)
		if err != nil {
			return nil, nil, err
		}
	}

	// Create release function that will properly handle the browser
	release := func() {
		p.Release(ctx, browser)
	}

	return browser, release, nil
}

// Release returns a browser to the pool or closes it
func (p *BrowserPool) Release(ctx context.Context, browser *rod.Browser) {
	if browser == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Use a short context for health check
	checkCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Check if the browser is still connected by trying to create a page
	err := rod.Try(func() {
		page := browser.Context(checkCtx).MustPage()
		page.MustClose()
	})

	if err != nil {
		p.logger.Debug(ctx, "Browser disconnected, closing", "error", fmt.Sprintf("%v", err))
		// Try to close it anyway with a new context
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = browser.Context(closeCtx).Close()
		closeCancel()
		return
	}

	// If the pool is full, close the browser
	if len(p.browserList) >= p.maxInstances {
		p.logger.Debug(ctx, "Pool is full, closing browser instance")
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := browser.Context(closeCtx).Close(); err != nil {
			p.logger.Error(ctx, "Error closing browser", "error", fmt.Sprintf("%v", err))
		}
		closeCancel()
		return
	}

	// Return the browser to the pool
	p.browserList = append(p.browserList, browser)
	p.logger.Debug(ctx, "Browser instance returned to pool", "pool_size", len(p.browserList))
}

// Close closes all browser instances in the pool
func (p *BrowserPool) Close(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, browser := range p.browserList {
		p.logger.Debug(ctx, "Closing browser instance")
		if err := browser.Close(); err != nil {
			p.logger.Error(ctx, "Error closing browser", "error", err)
		}
	}

	p.browserList = nil
	p.logger.Info(ctx, "All browser instances closed")
}

// BrowserPoolChecker checks the health of the browser pool
type BrowserPoolChecker struct {
	pool *BrowserPool
}

// Check implements the health.Checker interface
func (c *BrowserPoolChecker) Check(ctx context.Context) error {
	if c.pool == nil {
		return fmt.Errorf("browser pool not initialized")
	}

	status := c.pool.Status()
	if status["pool_size"].(int) == 0 {
		// No browsers in pool, but not necessarily an error
		return nil
	}

	return nil
}

// Name implements the health.Checker interface
func (c *BrowserPoolChecker) Name() string {
	return "browser-pool"
}

// handleResultMessage processes messages from the result queue
func handleResultMessage(msg *nats.Msg) {
	// Extract trace context from message if available
	parentCtx := context.Background()

	// Parse the message
	var response map[string]interface{}
	if err := json.Unmarshal(msg.Data, &response); err != nil {
		logger.Error(parentCtx, "Failed to parse result message", "error", err)
		return
	}
	// Get trace ID from the response
	traceID, ok := response["trace_id"].(string)
	if !ok || traceID == "" {
		logger.Error(parentCtx, "Missing trace ID in result message")
		return
	}

	// Create context with trace ID for logging
	ctx := context.Background()
	logger.Info(ctx, "Received result message", "trace_id", traceID, "subject", msg.Subject)

	// Add to response cache
	responseCache.Add(traceID, response)
}

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

	// Search Images for the content
	imageURL, err := searchGoogleImages(ctx, msg.Content)
	if err != nil {
		logger.Error(ctx, "Failed to search Google Images", "error", err, "query", msg.Content)
		// Try Bing as a fallback
		imageURL, err = searchBingImages(ctx, msg.Content)
		if err != nil {
			logger.Error(ctx, "Failed to search Bing Images", "error", err, "query", msg.Content)
			// Return an error instead of using a placeholder
			http.Error(w, "Failed to find an image for the given content", http.StatusInternalServerError)
			return
		}
	}

	if imageURL == "" {
		logger.Error(ctx, "Both search engines returned empty image URL", "query", msg.Content)
		http.Error(w, "Failed to find an image for the given content", http.StatusInternalServerError)
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

	// Get current trace ID for correlation
	traceID := tracing.GetTraceID(ctx)

	var asciiTerminal, asciiHTML string
	timeout := 30 * time.Second // Adjust timeout as needed

	// Wait for ASCII results
	result, err := responseCache.GetWithTimeout(traceID, timeout)
	if err != nil {
		logger.Warn(ctx, "Timed out waiting for ASCII results", "error", err)
	} else {
		logger.Info(ctx, "Received ASCII results", "trace_id", traceID)
	}

	response := map[string]string{
		"status":           "completed",
		"queue":            queueName, // Return the original queue name to the client
		"image_url":        imageURL,
		"image_ascii_text": result["ascii_terminal"].(string),
		"image_ascii_html": result["ascii_html"].(string),
	}

	// Return full response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Error(ctx, "Error encoding response", "error", err)
		logger.Debug(ctx, "ASCII results", "response", response)
		return
	}

	logger.Info(ctx, "Request handled successfully",
		"queue", queueName,
		"message_id", msg.ID,
		"image_url", imageURL,
		"has_ascii_terminal", asciiTerminal != "",
		"has_ascii_html", asciiHTML != "")
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

// handleAsciiTerminal returns the terminal ASCII art
func handleAsciiTerminal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get trace ID from query parameter
	traceID := r.URL.Query().Get("trace_id")
	if traceID == "" {
		http.Error(w, "Missing trace_id parameter", http.StatusBadRequest)
		return
	}

	// Try to get from response cache
	result, err := responseCache.GetWithTimeout(traceID, 100*time.Millisecond) // Short timeout as it should be there already
	if err != nil {
		logger.Error(ctx, "Failed to get ASCII terminal", "error", err, "trace_id", traceID)
		http.Error(w, "ASCII terminal not found", http.StatusNotFound)
		return
	}

	// Extract terminal ASCII
	terminal, ok := result["ascii_terminal"].(string)
	if !ok || terminal == "" {
		logger.Error(ctx, "ASCII terminal not available", "trace_id", traceID)
		http.Error(w, "ASCII terminal not available", http.StatusNotFound)
		return
	}

	// Return the ASCII art
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(terminal))
}

// handleAsciiHTML returns the HTML ASCII art
func handleAsciiHTML(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get trace ID from query parameter
	traceID := r.URL.Query().Get("trace_id")
	if traceID == "" {
		http.Error(w, "Missing trace_id parameter", http.StatusBadRequest)
		return
	}

	// Try to get from response cache
	result, err := responseCache.GetWithTimeout(traceID, 100*time.Millisecond) // Short timeout as it should be there already
	if err != nil {
		logger.Error(ctx, "Failed to get ASCII HTML", "error", err, "trace_id", traceID)
		http.Error(w, "ASCII HTML not found", http.StatusNotFound)
		return
	}

	// Extract HTML ASCII
	html, ok := result["ascii_html"].(string)
	if !ok || html == "" {
		logger.Error(ctx, "ASCII HTML not available", "trace_id", traceID)
		http.Error(w, "ASCII HTML not available", http.StatusNotFound)
		return
	}

	// Return the ASCII HTML
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func searchBingImages(ctx context.Context, query string) (string, error) {
	// Create a span for tracing
	ctx, span := tracer.Start(ctx, "bing.image_search", trace.WithAttributes(
		attribute.String("search.query", query),
	))
	defer span.End()

	// Retry configuration
	maxRetries := 2
	var imgURL string
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			logger.Info(ctx, "Retrying Bing image search",
				"attempt", attempt,
				"max_retries", maxRetries,
				"query", query)
			// Wait a bit before retrying
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
				// Continue after waiting
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		// Create context with timeout for this attempt
		searchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)

		// Get browser from pool
		browser, release, err := browserPool.Get(searchCtx)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("failed to get browser from pool: %w", err)
			logger.Error(ctx, "Failed to get browser from pool", "error", lastErr.Error(), "attempt", attempt)
			continue // Try again
		}

		imgURL, err = executeBingImageSearch(searchCtx, browser, query)

		// Always release the browser back to the pool
		release()
		cancel() // Cancel the timeout context

		if err == nil && imgURL != "" {
			return imgURL, nil // Success
		}

		lastErr = err
		logger.Error(ctx, "Bing image search failed",
			"error", fmt.Sprintf("%v", err),
			"attempt", attempt,
			"query", query)
	}

	return "", lastErr // Return the last error after all retries fail
}

// executeBingImageSearch performs the actual Bing search with the browser
func executeBingImageSearch(ctx context.Context, browser *rod.Browser, query string) (string, error) {
	// Set the context on the browser
	browser = browser.Context(ctx)

	// Create page
	var page *rod.Page
	err := rod.Try(func() {
		page = browser.MustPage()
	})

	if err != nil {
		return "", fmt.Errorf("failed to create page: %w", err)
	}

	// Make sure page is closed
	defer func() {
		// Use a new context for closing to avoid deadline exceeded errors
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := rod.Try(func() {
			page.Context(closeCtx).MustClose()
		})

		if err != nil {
			logger.Warn(ctx, "Failed to close page", "error", fmt.Sprintf("%v", err))
		}
	}()

	// Navigate to Bing Images search
	searchURL := "https://www.bing.com/images/search?q=" + url.QueryEscape(query)
	logger.Info(ctx, "Navigating to Bing search URL", "url", searchURL)

	err = rod.Try(func() {
		page.MustNavigate(searchURL)
	})

	if err != nil {
		return "", fmt.Errorf("failed to navigate to Bing URL: %w", err)
	}
	// Wait for page to stabilize
	err = rod.Try(func() {
		err = page.WaitStable(2 * time.Second)
	})

	if err != nil {
		logger.Error(ctx, "Failed to wait for Bing page stability", "error", fmt.Sprintf("%v", err))
	}

	// Try to handle cookie consent dialogs specific to Bing
	consentButtonSelectors := []string{
		"#bnp_btn_accept",
		".ccBtnAccept",
		"#idSIButton9",
		".bnp_btn_accept",
		"[aria-label='Accept']",
		"[title='Accept']",
	}

	for _, selector := range consentButtonSelectors {
		err := rod.Try(func() {
			el := page.MustElement(selector)
			el.MustClick()
			logger.Info(ctx, "Clicked Bing consent button", "selector", selector)
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
		logger.Error(ctx, "Failed to wait for Bing page load", "error", fmt.Sprintf("%v", err))
	}

	var imgURL string

	err = rod.Try(func() {
		html, err := page.HTML()
		if err == nil {
			// Look for image URLs in the page source
			patterns := []string{
				`murl&quot;:&quot;(http[^&"]+)&quot;`,
				`murl":"(http[^"]+)"`,
				`src="(https://th.bing.com/[^"]+)"`,
			}

			for _, pattern := range patterns {
				re := regexp.MustCompile(pattern)
				matches := re.FindStringSubmatch(html)
				if len(matches) > 1 {
					encodedURL := matches[1]
					decodedURL, err := url.QueryUnescape(encodedURL)
					if err == nil {
						imgURL = decodedURL
						logger.Debug(ctx, "Found Bing image URL from HTML regex", "url", imgURL, "pattern", pattern)
						break
					}
				}
			}
		}
	})

	if err != nil {
		logger.Error(ctx, "Failed to extract Bing image URL", "error", err)
	}

	logger.Info(ctx, "Returning Bing image URL", "url", imgURL)
	// Return the image URL (or empty string) and any error
	return imgURL, err
}

func searchGoogleImages(ctx context.Context, query string) (string, error) {
	// Create a span for tracing
	ctx, span := tracer.Start(ctx, "google.image_search", trace.WithAttributes(
		attribute.String("search.query", query),
	))
	defer span.End()

	// Retry configuration
	maxRetries := 2
	var imgURL string
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			logger.Info(ctx, "Retrying Google image search",
				"attempt", attempt,
				"max_retries", maxRetries,
				"query", query)
			// Wait a bit before retrying
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
				// Continue after waiting
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		// Create context with timeout for this attempt
		searchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)

		// Get browser from pool
		browser, release, err := browserPool.Get(searchCtx)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("failed to get browser from pool: %w", err)
			logger.Error(ctx, "Failed to get browser from pool", "error", lastErr.Error(), "attempt", attempt)
			continue // Try again
		}

		imgURL, err = executeGoogleImageSearch(searchCtx, browser, query)

		// Always release the browser back to the pool
		release()
		cancel() // Cancel the timeout context

		if err == nil && imgURL != "" {
			return imgURL, nil // Success
		}

		lastErr = err
		logger.Error(ctx, "Google image search failed",
			"error", fmt.Sprintf("%v", err),
			"attempt", attempt,
			"query", query)
	}

	return "", lastErr // Return the last error after all retries fail
}

func executeGoogleImageSearch(ctx context.Context, browser *rod.Browser, query string) (string, error) {
	// Set the context on the browser
	browser = browser.Context(ctx)

	// Create page
	var page *rod.Page
	err := rod.Try(func() {
		page = browser.MustPage()
	})

	if err != nil {
		return "", fmt.Errorf("failed to create page: %w", err)
	}

	// Make sure page is closed
	defer func() {
		// Use a new context for closing to avoid deadline exceeded errors
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := rod.Try(func() {
			page.Context(closeCtx).MustClose()
		})

		if err != nil {
			logger.Warn(ctx, "Failed to close page", "error", fmt.Sprintf("%v", err))
		}
	}()

	// Navigate to Google Images search
	searchURL := "https://www.google.com/search?tbm=isch&q=" + url.QueryEscape(query)
	logger.Info(ctx, "Navigating to search URL", "url", searchURL)

	err = rod.Try(func() {
		page.MustNavigate(searchURL)
	})

	if err != nil {
		return "", fmt.Errorf("failed to navigate to URL: %w", err)
	}
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
					logger.Error(ctx, "Error trying to get image height")
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

	// Return the image URL (or empty string) and any error
	return imgURL, err
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

	browserPool = NewBrowserPool(5, logger) // Allow up to 5 concurrent browser instances
	if err := browserPool.Initialize(ctx); err != nil {
		logger.Error(ctx, "Failed to initialize browser pool", "error", err)
		// Continue anyway, as search will create instances on-demand if needed
	}

	// Make sure we clean up the browser pool when shutting down
	defer browserPool.Close(ctx)

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

	// Subscribe to result queue
	env := os.Getenv("ENV")
	if env == "" {
		env = "dev" // Default to dev if ENV not set
	}
	resultQueue := fmt.Sprintf("%s.result-queue", env)

	logger.Info(ctx, "Subscribing to result queue", "queue", resultQueue)
	sub, err := natsConn.SubscribeWithTracing(resultQueue, handleResultMessage)
	if err != nil {
		logger.Fatal(ctx, "Failed to subscribe to result queue", "error", err)
	}
	defer sub.Unsubscribe()

	// Create a server with logging middleware
	srv := server.NewServer("enqueuer",
		server.WithLogger(logger),
	)

	// Use middleware
	srv.UseMiddleware(corsMiddleware, server.LoggingMiddleware(logger))

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
	browserPoolChecker := &BrowserPoolChecker{pool: browserPool}
	srv.RegisterHealthChecks(
		[]health.Checker{natsConn, browserPoolChecker},
		[]health.Checker{natsConn, browserPoolChecker}, // Readiness checks
	)
	// Register ASCII handlers
	srv.HandleFunc("/ascii/terminal", handleAsciiTerminal)
	srv.HandleFunc("/ascii/html", handleAsciiHTML)
	// Add browser pool status endpoint
	srv.HandleFunc("/browsercheck", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger.Debug(ctx, "Handling browser pool status request")

		if browserPool == nil {
			logger.Warn(ctx, "Browser pool not initialized")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("Browser pool not initialized"))
			return
		}

		status := browserPool.Status()
		logger.Debug(ctx, "Browser pool status", "status", status)

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(status); err != nil {
			logger.Error(ctx, "Failed to encode browser pool status", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	})
	// Set up signal handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Use a context for graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Start server in a goroutine
	go func() {
		logger.Info(ctx, "Starting enqueuer service", "port", 8080)
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			logger.Fatal(ctx, "Server failed", "error", err)
		}
	}()

	// Wait for termination signal
	sig := <-stop
	logger.Info(ctx, "Received termination signal", "signal", sig)

	// Initiate graceful shutdown
	logger.Info(ctx, "Shutting down services...")

	// First close the browser pool to free resources
	if browserPool != nil {
		logger.Info(ctx, "Shutting down browser pool...")
		browserPool.Close(ctx)
	}

	// Then stop the HTTP server
	if err := srv.Stop(); err != nil {
		logger.Error(ctx, "Error stopping server", "error", err)
	}

	// Wait for context timeout or cancellation
	<-shutdownCtx.Done()
	if shutdownCtx.Err() == context.DeadlineExceeded {
		logger.Warn(ctx, "Shutdown timed out, forcing exit")
	} else {
		logger.Info(ctx, "Graceful shutdown complete")
	}
}
