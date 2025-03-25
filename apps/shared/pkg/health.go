package pkg

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/nats-io/nats.go"
)

// HealthCheckService provides health check endpoints for microservices
type HealthCheckService struct {
	Port        string
	NatsConn    *nats.Conn
	RedisClient *redis.Client
	PostgresDB  interface {
		Ping() error
	}
}

// NewHealthCheckService creates a new health check service
func NewHealthCheckService() *HealthCheckService {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	return &HealthCheckService{
		Port: port,
	}
}

// Start initiates the health check HTTP server
func (h *HealthCheckService) Start() {
	go func() {
		// Basic health check endpoint
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("Handling health check: %s %s\n", r.Method, r.URL.Path)
			_, err := w.Write([]byte("OK"))
			if err != nil {
				fmt.Printf("Error writing health check response: %v\n", err)
			}
		})

		// NATS connection check
		if h.NatsConn != nil {
			http.HandleFunc("/natscheck", func(w http.ResponseWriter, r *http.Request) {
				fmt.Printf("Handling NATS check: %s %s\n", r.Method, r.URL.Path)

				if h.NatsConn == nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, err := w.Write([]byte("NATS not connected"))
					if err != nil {
						fmt.Printf("Error writing NATS check response: %v\n", err)
					}
					return
				}

				if !h.NatsConn.IsConnected() {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, err := w.Write([]byte("NATS connection lost"))
					if err != nil {
						fmt.Printf("Error writing NATS check response: %v\n", err)
					}
					return
				}

				_, err := w.Write([]byte("NATS connection OK"))
				if err != nil {
					fmt.Printf("Error writing NATS check response: %v\n", err)
				}
			})
		}

		// Redis connection check
		if h.RedisClient != nil {
			http.HandleFunc("/redischeck", func(w http.ResponseWriter, r *http.Request) {
				fmt.Printf("Handling Redis check: %s %s\n", r.Method, r.URL.Path)

				if h.RedisClient == nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, err := w.Write([]byte("Redis not connected"))
					if err != nil {
						fmt.Printf("Error writing Redis check response: %v\n", err)
					}
					return
				}

				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()

				_, err := h.RedisClient.Ping(ctx).Result()
				if err != nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, writeErr := fmt.Fprintf(w, "Redis connection error: %v", err)
					if writeErr != nil {
						fmt.Printf("Error writing Redis check response: %v\n", writeErr)
					}
					return
				}

				_, err = w.Write([]byte("Redis connection OK"))
				if err != nil {
					fmt.Printf("Error writing Redis check response: %v\n", err)
				}
			})
		}

		// Database connection check
		if h.PostgresDB != nil {
			http.HandleFunc("/dbcheck", func(w http.ResponseWriter, r *http.Request) {
				fmt.Printf("Handling DB check: %s %s\n", r.Method, r.URL.Path)

				if h.PostgresDB == nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, err := w.Write([]byte("PostgreSQL not connected"))
					if err != nil {
						fmt.Printf("Error writing DB check response: %v\n", err)
					}
					return
				}

				err := h.PostgresDB.Ping()
				if err != nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, writeErr := fmt.Fprintf(w, "PostgreSQL connection error: %v", err)
					if writeErr != nil {
						fmt.Printf("Error writing DB check response: %v\n", writeErr)
					}
					return
				}

				_, err = w.Write([]byte("PostgreSQL connection OK"))
				if err != nil {
					fmt.Printf("Error writing DB check response: %v\n", err)
				}
			})
		}

		fmt.Printf("Starting health check server on port %s...\n", h.Port)
		if err := http.ListenAndServe(":"+h.Port, nil); err != nil {
			fmt.Printf("HTTP server error: %v\n", err)
		}
	}()
}
