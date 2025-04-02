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
	"github.com/go-rod/rod/lib/proto"
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
	browserPool *PagePool
	poolSize    = 3 // Configurable pool size
)

// PagePool manages a pool of Rod browser pages
type PagePool struct {
	pages chan *rod.Page
	mu    sync.Mutex
	size  int
}

// NewPagePool creates a new page pool with the specified size
func NewPagePool(size int) *PagePool {
	return &PagePool{
		pages: make(chan *rod.Page, size),
		size:  size,
	}
}

// Get gets a page from the pool or creates a new one using the factory function
func (p *PagePool) Get(ctx context.Context, create func(ctx context.Context) *rod.Page) *rod.Page {
	select {
	case page := <-p.pages:
		logger.Debug(ctx, "Reusing page from pool", "remaining", len(p.pages))
		return page
	default:
		logger.Debug(ctx, "Creating new page for pool", "size", p.size)
		return create(ctx)
	}
}

// Put returns a page to the pool or closes it if the pool is full
func (p *PagePool) Put(ctx context.Context, page *rod.Page) {
	if page == nil {
		return
	}

	select {
	case p.pages <- page:
		logger.Debug(ctx, "Page returned to pool", "pool_size", len(p.pages))
	default:
		logger.Debug(ctx, "Pool full, closing page")
		err := page.Close()
		if err != nil {
			logger.Error(ctx, "Error closing page", "error", err)
		}
	}
}

// Cleanup closes all pages in the pool
func (p *PagePool) Cleanup(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	logger.Info(ctx, "Cleaning up page pool")

	// Close all pages in the pool
	close(p.pages)
	for page := range p.pages {
		if page != nil {
			err := page.Close()
			if err != nil {
				logger.Error(ctx, "Error closing page during cleanup", "error", err)
			}
		}
	}
}

// BrowserConfig holds configuration for browser instances
type BrowserConfig struct {
	Timeout     time.Duration
	Headless    bool
	ChromePath  string
	UserAgent   string
	MaxPoolSize int
}

// createBrowserPage creates a new browser page for the pool
func createBrowserPage(ctx context.Context, config BrowserConfig) *rod.Page {
	logger.Debug(ctx, "Creating new browser instance")

	u, err := launcher.New().
		Bin(config.ChromePath).
		Headless(config.Headless).
		Set("no-sandbox", "").
		Set("disable-dev-shm-usage", "").
		Set("disable-gpu", "").
		Set("window-size", "1366,768").
		Set("user-agent", config.UserAgent).
		Set("accept-language", "en-US,en;q=0.9").
		Launch()

	if err != nil {
		logger.Error(ctx, "Failed to launch browser", "error", err)
		return nil
	}

	// Create browser instance with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()

	browser := rod.New().
		ControlURL(u).
		Context(timeoutCtx)

	err = browser.Connect()
	if err != nil {
		logger.Error(ctx, "Failed to connect to browser", "error", err)
		return nil
	}

	// Create incognito page
	page, err := browser.Incognito()
	if err != nil {
		logger.Error(ctx, "Failed to create incognito context", "error", err)
		return nil
	}

	p, err := page.Page(proto.TargetCreateTarget{})
	if err != nil {
		logger.Error(ctx, "Failed to create page", "error", err)
		return nil
	}

	logger.Debug(ctx, "Browser instance created successfully")
	return p
}

// GetDefaultBrowserConfig returns a default browser configuration
func GetDefaultBrowserConfig() BrowserConfig {
	// Find the Chrome binary path
	chromePath := os.Getenv("CHROME_PATH")
	if chromePath == "" {
		chromePath = "/usr/bin/chromium-browser" // Default path for our container
	}

	return BrowserConfig{
		Timeout:     30 * time.Second,
		Headless:    true,
		ChromePath:  chromePath,
		UserAgent:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36",
		MaxPoolSize: 3,
	}
}

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

	// Search Bing Images for the content
	imageURL, err := searchGoogleImages(ctx, msg.Content)
	if err != nil {
		logger.Error(ctx, "Failed to search Google Images", "error", err)
		http.Error(w, fmt.Sprintf("Failed to search Google Images: %v", err), http.StatusInternalServerError)
		imageURL, err = searchBingImages(ctx, msg.Content)
		if err != nil {
			logger.Error(ctx, "Failed to search Bing Images", "error", err)
			http.Error(w, fmt.Sprintf("Failed to search Bing Images: %v", err), http.StatusInternalServerError)
			return
		}
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

	// Find the Chrome binary path
	chromePath := os.Getenv("CHROME_PATH")
	if chromePath == "" {
		chromePath = "/usr/bin/google-chrome" // Default path for our container
	}

	logger.Info(ctx, "Using Chrome binary from path for Bing search", "path", chromePath)
	userAgent := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36"

	// Create Chrome launcher
	logger.Info(ctx, "Creating Chrome launcher for Bing search")
	u, err := launcher.New().
		Bin(chromePath).
		Headless(true).
		Set("no-sandbox", "").
		Set("disable-dev-shm-usage", "").
		Set("disable-gpu", "").
		Set("window-size", "1366,768"). // Typical laptop resolution
		Set("user-agent", userAgent).
		Set("accept-language", "en-US,en;q=0.9").
		Launch()

	if err != nil {
		logger.Error(ctx, "Failed to launch Chrome for Bing search", "error", fmt.Sprintf("%v", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Chrome launched successfully for Bing search", "control_url", u)

	// Create browser instance
	browser := rod.New().
		ControlURL(u).
		Timeout(30 * time.Second)

	err = browser.Connect()
	if err != nil {
		logger.Error(ctx, "Failed to connect to Chrome for Bing search", "error", fmt.Sprintf("%v", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Successfully connected to browser for Bing search")
	defer browser.Close()

	// Create page
	var page *rod.Page
	err = rod.Try(func() {
		page = browser.MustPage()
	})

	if err != nil {
		logger.Error(ctx, "Failed to create page for Bing search", "error", fmt.Sprintf("%v", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Page created successfully for Bing search")

	// Navigate to Bing Images search
	searchURL := "https://www.bing.com/images/search?q=" + url.QueryEscape(query)
	logger.Info(ctx, "Navigating to Bing search URL", "url", searchURL)

	err = rod.Try(func() {
		page.MustNavigate(searchURL)
	})

	if err != nil {
		logger.Error(ctx, "Failed to navigate to Bing URL", "url", searchURL, "error", fmt.Sprintf("%v", err))
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Successfully navigated to Bing URL", "url", searchURL)

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
	return imgURL, nil
}

// handleConsentDialogs attempts to click on cookie consent buttons
func handleConsentDialogs(ctx context.Context, page *rod.Page) {
	// Different consent button selectors
	consentButtonSelectors := []string{
		"button#L2AGLb",
		"[aria-label='Accept all']",
		"[aria-label='Aceitar tudo']",
		"button.tHlp8d",
		"form:nth-child(2) button",
		"div.VDity button:first-child",
	}

	for _, selector := range consentButtonSelectors {
		err := rod.Try(func() {
			el, err := page.Element(selector)
			if err != nil {
				return
			}
			// Use MustClick instead of Click to simplify the call
			el.MustClick()
			logger.Info(ctx, "Clicked consent button", "selector", selector)
			time.Sleep(500 * time.Millisecond)
		})
		if err == nil {
			break
		}
	}
}

// extractImageUrl tries to extract image URLs from the page using different methods
func extractImageUrl(ctx context.Context, page *rod.Page) string {
	var imgURL string

	// 1. First try to find image elements directly
	elements, err := page.Elements("img[src^='https://']")
	if err == nil && len(elements) > 0 {
		// Filter to find reasonably sized images (skip icons)
		for _, el := range elements {
			src, err := el.Attribute("src")
			if err != nil || src == nil {
				continue
			}

			// Try to get image dimensions
			width, height := 0, 0
			widthAttr, _ := el.Attribute("width")
			heightAttr, _ := el.Attribute("height")

			if widthAttr != nil {
				width, _ = strconv.Atoi(*widthAttr)
			}
			if heightAttr != nil {
				height, _ = strconv.Atoi(*heightAttr)
			}

			// Skip small images that are likely icons
			if width > 0 && height > 0 && (width < 100 || height < 100) {
				continue
			}

			imgURL = *src
			logger.Debug(ctx, "Found image element", "url", imgURL)
			return imgURL
		}
	}

	// 2. Try to extract from HTML source
	html, err := page.HTML()
	if err == nil {
		patterns := []string{
			`murl&quot;:&quot;(http[^&"]+)&quot;`,
			`murl":"(http[^"]+)"`,
			`src="(https://th.bing.com/[^"]+)"`,
			`src="(https://encrypted-tbn0.gstatic.com/[^"]+)"`,
		}

		for _, pattern := range patterns {
			re := regexp.MustCompile(pattern)
			matches := re.FindStringSubmatch(html)
			if len(matches) > 1 {
				encodedURL := matches[1]
				decodedURL, err := url.QueryUnescape(encodedURL)
				if err == nil {
					imgURL = decodedURL
					logger.Debug(ctx, "Found image URL from HTML regex", "url", imgURL, "pattern", pattern)
					return imgURL
				}
			}
		}
	}

	// If no image found, return empty
	return ""
}

func searchGoogleImages(ctx context.Context, query string) (string, error) {
	// Create a span for tracing
	ctx, span := tracer.Start(ctx, "google.image_search", trace.WithAttributes(
		attribute.String("search.query", query),
	))
	defer span.End()

	// Get browser configuration
	config := GetDefaultBrowserConfig()

	// Create a page factory function for the pool
	createPage := func(ctx context.Context) *rod.Page {
		return createBrowserPage(ctx, config)
	}

	// Get a page from the pool
	page := browserPool.Get(ctx, createPage)
	if page == nil {
		logger.Error(ctx, "Failed to get browser from pool")
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	// Schedule returning the page to the pool
	defer func() {
		// Clear page state before returning to pool
		err := rod.Try(func() {
			page.MustNavigate("about:blank")
		})
		if err != nil {
			// If clearing failed, don't return to pool
			logger.Warn(ctx, "Failed to reset page, closing instead of returning to pool", "error", err)
			page.Close()
		} else {
			browserPool.Put(ctx, page)
		}
	}()

	logger.Info(ctx, "Successfully got browser from pool")

	// Set page timeout
	pageCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	page = page.Context(pageCtx)

	// Navigate to Google Images search with timeout
	searchURL := "https://www.google.com/search?tbm=isch&q=" + url.QueryEscape(query)
	logger.Info(ctx, "Navigating to search URL", "url", searchURL)

	err := rod.Try(func() {
		page.MustNavigate(searchURL)
	})

	if err != nil {
		logger.Error(ctx, "Failed to navigate to URL", "url", searchURL, "error", err)
		placeholderURL := fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
		return placeholderURL, nil
	}

	logger.Info(ctx, "Successfully navigated to URL", "url", searchURL)

	// Try to handle cookie consent dialogs
	handleConsentDialogs(ctx, page)

	// Wait a moment for the page to render
	time.Sleep(500 * time.Millisecond)

	var imgURL string

	// Try to extract image URLs with a timeout
	err = rod.Try(func() {
		imgURL = extractImageUrl(ctx, page)
	})

	if err != nil || imgURL == "" {
		logger.Error(ctx, "Failed to extract image URL", "error", err)
		imgURL = fmt.Sprintf("https://via.placeholder.com/500x300/3498db/ffffff?text=%s", url.QueryEscape(query))
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

	// Initialize browser pool
	browserConfig := GetDefaultBrowserConfig()
	poolSize = browserConfig.MaxPoolSize
	browserPool = NewPagePool(poolSize)

	logger.Info(ctx, "Initialized browser pool", "size", poolSize)

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
	srv.RegisterHealthChecks(
		[]health.Checker{natsConn},
		[]health.Checker{natsConn}, // Readiness checks
	)
	// Register ASCII handlers
	srv.HandleFunc("/ascii/terminal", handleAsciiTerminal)
	srv.HandleFunc("/ascii/html", handleAsciiHTML)
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

	// Set up browser pool cleanup
	go func() {
		<-stop
		logger.Info(ctx, "Cleaning up browser pool")
		browserPool.Cleanup(ctx)
	}()
	// Wait for termination signal
	<-stop

	// Shut down gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
	defer shutdownCancel()

	logger.Info(shutdownCtx, "Shutting down enqueuer service")
}
