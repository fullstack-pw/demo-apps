package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// SearchRequest represents the search request with URL
type SearchRequest struct {
	URL string `json:"url"`
}

// Property represents a real estate property
type Property struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Price       float64  `json:"price"` // in euros
	Location    string   `json:"location"`
	Bedrooms    int      `json:"bedrooms"`
	Bathrooms   int      `json:"bathrooms"`
	Area        float64  `json:"area_m2"`
	Description string   `json:"description"`
	Images      []string `json:"images"`
	URL         string   `json:"url"`
	Source      string   `json:"source"` // idealista or imovirtual
	ScrapedAt   string   `json:"scraped_at"`
}

// SearchResponse represents the search response
type SearchResponse struct {
	Properties []Property `json:"properties"`
	Total      int        `json:"total"`
	Message    string     `json:"message,omitempty"`
	TraceID    string     `json:"trace_id"`
}

// Global variables
var (
	natsConn      *connections.NATSConnection
	logger        *logging.Logger
	tracer        = tracing.GetTracer("realstate-scrapper")
	serviceLocked bool
	lockMutex     sync.RWMutex
)

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

// determineSite determines the source site from URL
func determineSite(url string) string {
	if strings.Contains(url, "idealista.pt") {
		return "idealista"
	}
	if strings.Contains(url, "imovirtual.com") {
		return "imovirtual"
	}
	return ""
}

// scrapeProperties scrapes properties from the given URL
func scrapeProperties(ctx context.Context, searchURL, source string) ([]Property, error) {
	switch source {
	case "idealista":
		return scrapeIdealista(ctx, searchURL)
	case "imovirtual":
		return scrapeImovirtual(ctx, searchURL)
	default:
		return nil, fmt.Errorf("unsupported source: %s", source)
	}
}

// handleSearch handles the /search endpoint
func handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger.Debug(ctx, "Handling search request", "method", r.Method, "path", r.URL.Path)

	// Check if service is locked
	lockMutex.RLock()
	locked := serviceLocked
	lockMutex.RUnlock()

	if locked {
		logger.Warn(ctx, "Service is locked due to bot detection")
		http.Error(w, "Service is locked due to bot detection. Please call /unlock to resume.", http.StatusServiceUnavailable)
		return
	}

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

	// Parse request
	var req SearchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		logger.Error(ctx, "Invalid JSON payload", "error", err)
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		logger.Error(ctx, "Missing URL in request")
		http.Error(w, "URL is required", http.StatusBadRequest)
		return
	}

	// Determine site from URL
	source := determineSite(req.URL)
	if source == "" {
		logger.Error(ctx, "Unsupported site URL", "url", req.URL)
		http.Error(w, "Unsupported site. Only idealista.pt and imovirtual.com are supported", http.StatusBadRequest)
		return
	}

	// Scrape properties
	properties, err := scrapeProperties(ctx, req.URL, source)
	if err != nil {
		if strings.Contains(err.Error(), "bot detection") {
			// Lock the service
			lockMutex.Lock()
			serviceLocked = true
			lockMutex.Unlock()

			logger.Error(ctx, "Bot detection triggered, locking service", "error", err)
			http.Error(w, "Bot detection triggered. Service is now locked. Please call /unlock to resume.", http.StatusServiceUnavailable)
			return
		}

		logger.Error(ctx, "Failed to scrape properties", "error", err)
		http.Error(w, fmt.Sprintf("Failed to scrape properties: %v", err), http.StatusInternalServerError)
		return
	}

	// Get current trace ID for correlation
	traceID := tracing.GetTraceID(ctx)

	// Build response
	response := SearchResponse{
		Properties: properties,
		Total:      len(properties),
		TraceID:    traceID,
	}

	if len(properties) == 0 {
		response.Message = "No properties found for the given search criteria"
	}

	// Return response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Error(ctx, "Error encoding response", "error", err)
		return
	}

	logger.Info(ctx, "Search completed successfully",
		"url", req.URL,
		"source", source,
		"properties_found", len(properties))
}

// handleUnlock handles the /unlock endpoint
func handleUnlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger.Debug(ctx, "Handling unlock request", "method", r.Method, "path", r.URL.Path)

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	lockMutex.Lock()
	serviceLocked = false
	lockMutex.Unlock()

	logger.Info(ctx, "Service unlocked successfully")

	response := map[string]string{
		"status":  "unlocked",
		"message": "Service is now ready to receive requests",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func scrapeIdealista(ctx context.Context, searchURL string) ([]Property, error) {
	chromePath := "/usr/bin/chromium-browser"

	ctx, span := tracer.Start(ctx, "idealista.scrape", trace.WithAttributes(
		attribute.String("search.url", searchURL),
		attribute.String("search.source", "idealista"),
	))
	defer span.End()

	logger.Info(ctx, "Initializing Chromium browser instance", "path", chromePath)

	l := launcher.New().Headless(true).Bin(chromePath).NoSandbox(true).MustLaunch()
	browser := rod.New().ControlURL(l).MustConnect().MustPage(searchURL)
	defer browser.MustClose()

	logger.Info(ctx, "Search Page created successfully for Idealista")

	// Handle cookie consent dialogs specific to Idealista
	consentButtonSelectors := []string{
		"#didomi-notice-agree-button",
		"button[aria-label='Accept']",
		".sui-AtomButton--primary",
		"#didomi-notice-agree-button",
		".didomi-notice-agree-button",
		"button[id*='agree']",
		"button[class*='accept']",
	}

	for _, selector := range consentButtonSelectors {
		err := rod.Try(func() {
			el := browser.MustElement(selector)
			el.MustClick()
			logger.Info(ctx, "Clicked Idealista consent button", "selector", selector)
		})
		if err == nil {
			break
		}
	}

	// Wait for the page to load
	time.Sleep(2 * time.Second)
	err := rod.Try(func() {
		browser.MustWaitLoad()
	})

	if err != nil {
		logger.Error(ctx, "Failed to wait for Idealista page load", "error", fmt.Sprintf("%v", err))
	}

	var properties []Property

	err = rod.Try(func() {
		html, err := browser.HTML()
		if err != nil {
			return
		}

		// Check for bot detection
		if strings.Contains(html, "captcha") || strings.Contains(html, "blocked") || strings.Contains(html, "robot") {
			logger.Error(ctx, "Bot detection detected on Idealista")
			panic(fmt.Errorf("bot detection triggered"))
		}

		properties = extractIdealistaProperties(ctx, html)
		logger.Info(ctx, "Extracted properties from Idealista", "count", len(properties))
	})

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to scrape Idealista")
		return nil, err
	}

	span.SetStatus(codes.Ok, "Successfully scraped Idealista")
	return properties, nil
}

func scrapeImovirtual(ctx context.Context, searchURL string) ([]Property, error) {
	chromePath := "/usr/bin/chromium-browser"

	ctx, span := tracer.Start(ctx, "imovirtual.scrape", trace.WithAttributes(
		attribute.String("search.url", searchURL),
		attribute.String("search.source", "imovirtual"),
	))
	defer span.End()

	logger.Info(ctx, "Initializing Chromium browser instance", "path", chromePath)

	l := launcher.New().Headless(true).Bin(chromePath).NoSandbox(true).MustLaunch()
	browser := rod.New().ControlURL(l).MustConnect().MustPage(searchURL)
	defer browser.MustClose()

	logger.Info(ctx, "Search Page created successfully for Imovirtual")

	// Handle cookie consent dialogs specific to Imovirtual
	consentButtonSelectors := []string{
		"#onetrust-accept-btn-handler",
		".accept-cookies",
		"button[title='Accept']",
		"button[data-cy='accept-consent']",
		".consent-accept-all",
		"button[class*='accept']",
	}

	for _, selector := range consentButtonSelectors {
		err := rod.Try(func() {
			el := browser.MustElement(selector)
			el.MustClick()
			logger.Info(ctx, "Clicked Imovirtual consent button", "selector", selector)
		})
		if err == nil {
			break
		}
	}

	// Wait for the page to load
	time.Sleep(2 * time.Second)
	err := rod.Try(func() {
		browser.MustWaitLoad()
	})

	if err != nil {
		logger.Error(ctx, "Failed to wait for Imovirtual page load", "error", fmt.Sprintf("%v", err))
	}

	var properties []Property

	err = rod.Try(func() {
		html, err := browser.HTML()
		if err != nil {
			return
		}

		// Check for bot detection
		if strings.Contains(html, "captcha") || strings.Contains(html, "blocked") || strings.Contains(html, "robot") {
			logger.Error(ctx, "Bot detection detected on Imovirtual")
			panic(fmt.Errorf("bot detection triggered"))
		}

		properties = extractImovirtualProperties(ctx, html)
		logger.Info(ctx, "Extracted properties from Imovirtual", "count", len(properties))
	})

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to scrape Imovirtual")
		return nil, err
	}

	span.SetStatus(codes.Ok, "Successfully scraped Imovirtual")
	return properties, nil
}

func extractIdealistaProperties(ctx context.Context, html string) []Property {
	var properties []Property

	// Idealista property regex patterns (these will need testing and refinement)
	propertyPattern := `<article[^>]*class="[^"]*item[^"]*"[^>]*>(.*?)</article>`
	titlePattern := `<a[^>]*class="[^"]*item-link[^"]*"[^>]*>([^<]+)</a>`
	pricePattern := `<span[^>]*class="[^"]*item-price[^"]*"[^>]*>([^<]+)</span>`
	locationPattern := `<span[^>]*class="[^"]*item-detail[^"]*"[^>]*>([^<]+)</span>`
	urlPattern := `<a[^>]*class="[^"]*item-link[^"]*"[^>]*href="([^"]+)"`
	imagePattern := `<img[^>]*src="([^"]+)"`

	// Extract properties using regex (following enqueuer pattern)
	re := regexp.MustCompile(propertyPattern)
	matches := re.FindAllStringSubmatch(html, -1)

	for i, match := range matches {
		if len(match) > 1 {
			propertyHTML := match[1]

			property := Property{
				ID:        fmt.Sprintf("idealista-%d-%d", time.Now().Unix(), i),
				Source:    "idealista",
				ScrapedAt: time.Now().Format(time.RFC3339),
			}

			// Extract title
			if titleRe := regexp.MustCompile(titlePattern); titleRe != nil {
				if titleMatch := titleRe.FindStringSubmatch(propertyHTML); len(titleMatch) > 1 {
					property.Title = strings.TrimSpace(titleMatch[1])
				}
			}

			// Extract price
			if priceRe := regexp.MustCompile(pricePattern); priceRe != nil {
				if priceMatch := priceRe.FindStringSubmatch(propertyHTML); len(priceMatch) > 1 {
					priceStr := strings.ReplaceAll(strings.ReplaceAll(priceMatch[1], "€", ""), ".", "")
					priceStr = strings.ReplaceAll(priceStr, " ", "")
					if price, err := strconv.ParseFloat(priceStr, 64); err == nil {
						property.Price = price
					}
				}
			}

			// Extract location
			if locationRe := regexp.MustCompile(locationPattern); locationRe != nil {
				if locationMatch := locationRe.FindStringSubmatch(propertyHTML); len(locationMatch) > 1 {
					property.Location = strings.TrimSpace(locationMatch[1])
				}
			}

			// Extract URL
			if urlRe := regexp.MustCompile(urlPattern); urlRe != nil {
				if urlMatch := urlRe.FindStringSubmatch(propertyHTML); len(urlMatch) > 1 {
					property.URL = "https://www.idealista.pt" + urlMatch[1]
				}
			}

			// Extract images
			if imageRe := regexp.MustCompile(imagePattern); imageRe != nil {
				imageMatches := imageRe.FindAllStringSubmatch(propertyHTML, -1)
				for _, imgMatch := range imageMatches {
					if len(imgMatch) > 1 && strings.HasPrefix(imgMatch[1], "http") {
						property.Images = append(property.Images, imgMatch[1])
					}
				}
			}

			// Only add if we have essential data
			if property.Title != "" && property.Price > 0 {
				properties = append(properties, property)
			}
		}
	}

	logger.Info(ctx, "Extracted Idealista properties", "count", len(properties))
	return properties
}

func extractImovirtualProperties(ctx context.Context, html string) []Property {
	var properties []Property

	// Imovirtual property regex patterns (these will need testing and refinement)
	propertyPattern := `<div[^>]*data-cy="[^"]*listing-item[^"]*"[^>]*>(.*?)</div>`
	titlePattern := `<span[^>]*data-cy="[^"]*listing-item-title[^"]*"[^>]*>([^<]+)</span>`
	pricePattern := `<span[^>]*data-cy="[^"]*listing-item-price[^"]*"[^>]*>([^<]+)</span>`
	locationPattern := `<span[^>]*data-cy="[^"]*listing-item-location[^"]*"[^>]*>([^<]+)</span>`
	urlPattern := `<a[^>]*href="([^"]+)"[^>]*data-cy="[^"]*listing-item-link[^"]*"`
	imagePattern := `<img[^>]*src="([^"]+)"`

	// Extract properties using regex (following enqueuer pattern)
	re := regexp.MustCompile(propertyPattern)
	matches := re.FindAllStringSubmatch(html, -1)

	for i, match := range matches {
		if len(match) > 1 {
			propertyHTML := match[1]

			property := Property{
				ID:        fmt.Sprintf("imovirtual-%d-%d", time.Now().Unix(), i),
				Source:    "imovirtual",
				ScrapedAt: time.Now().Format(time.RFC3339),
			}

			// Extract title
			if titleRe := regexp.MustCompile(titlePattern); titleRe != nil {
				if titleMatch := titleRe.FindStringSubmatch(propertyHTML); len(titleMatch) > 1 {
					property.Title = strings.TrimSpace(titleMatch[1])
				}
			}

			// Extract price
			if priceRe := regexp.MustCompile(pricePattern); priceRe != nil {
				if priceMatch := priceRe.FindStringSubmatch(propertyHTML); len(priceMatch) > 1 {
					priceStr := strings.ReplaceAll(strings.ReplaceAll(priceMatch[1], "€", ""), " ", "")
					priceStr = strings.ReplaceAll(priceStr, ".", "")
					if price, err := strconv.ParseFloat(priceStr, 64); err == nil {
						property.Price = price
					}
				}
			}

			// Extract location
			if locationRe := regexp.MustCompile(locationPattern); locationRe != nil {
				if locationMatch := locationRe.FindStringSubmatch(propertyHTML); len(locationMatch) > 1 {
					property.Location = strings.TrimSpace(locationMatch[1])
				}
			}

			// Extract URL
			if urlRe := regexp.MustCompile(urlPattern); urlRe != nil {
				if urlMatch := urlRe.FindStringSubmatch(propertyHTML); len(urlMatch) > 1 {
					property.URL = "https://www.imovirtual.com" + urlMatch[1]
				}
			}

			// Extract images
			if imageRe := regexp.MustCompile(imagePattern); imageRe != nil {
				imageMatches := imageRe.FindAllStringSubmatch(propertyHTML, -1)
				for _, imgMatch := range imageMatches {
					if len(imgMatch) > 1 && strings.HasPrefix(imgMatch[1], "http") {
						property.Images = append(property.Images, imgMatch[1])
					}
				}
			}

			// Only add if we have essential data
			if property.Title != "" && property.Price > 0 {
				properties = append(properties, property)
			}
		}
	}

	logger.Info(ctx, "Extracted Imovirtual properties", "count", len(properties))
	return properties
}

func main() {
	// Create context for the application
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize the logger
	logger = logging.NewLogger("realstate-scrapper",
		logging.WithMinLevel(logging.Info),
		logging.WithJSONFormat(true),
	)

	// Initialize the tracer
	tracerCfg := tracing.DefaultConfig("realstate-scrapper")
	tp, err := tracing.InitTracer(tracerCfg)
	if err != nil {
		logger.Fatal(ctx, "Failed to initialize tracer", "error", err)
	}
	defer func() {
		if err := tracing.ShutdownTracer(ctx, tp); err != nil {
			log.Printf("Error shutting down tracer: %v", err)
		}
	}()

	logger.Info(ctx, "Successfully connected to global Chrome browser instance")

	// Create a server with logging middleware
	srv := server.NewServer("realstate-scrapper",
		server.WithLogger(logger),
	)

	// Use middleware
	srv.UseMiddleware(corsMiddleware, server.LoggingMiddleware(logger))

	// Register handlers
	srv.HandleFunc("/search", handleSearch)
	srv.HandleFunc("/unlock", handleUnlock)

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
		logger.Info(ctx, "Starting realstate-scrapper service", "port", 8080)
		if err := srv.Start(); err != nil {
			logger.Fatal(ctx, "Server failed", "error", err)
		}
	}()

	// Wait for termination signal
	<-stop

	// Shut down gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
	defer shutdownCancel()

	logger.Info(shutdownCtx, "Shutting down realstate-scrapper service")
}
