package connections

import (
	"context"
	"fmt"
	"os"
	"time"

	"fullstack.pw/shared/tracing"
	"github.com/go-redis/redis/v8"
)

// RedisConnection wraps a Redis client with additional functionality
type RedisConnection struct {
	client     *redis.Client
	addr       string
	password   string
	db         int
	maxRetries int
}

// RedisConfig holds configuration for the Redis connection
type RedisConfig struct {
	Host       string
	Port       string
	Password   string
	DB         int
	MaxRetries int
}

// DefaultRedisConfig returns default Redis configuration
func DefaultRedisConfig() RedisConfig {
	// Get Redis connection details from environment
	redisHost := os.Getenv("REDIS_HOST")
	redisPort := os.Getenv("REDIS_PORT")
	redisPassword := os.Getenv("REDIS_PASSWORD")

	// Set defaults if not provided
	if redisHost == "" {
		redisHost = "redis.fullstack.pw"
	}
	if redisPort == "" {
		redisPort = "6379"
	}

	return RedisConfig{
		Host:       redisHost,
		Port:       redisPort,
		Password:   redisPassword,
		DB:         0,
		MaxRetries: 5,
	}
}

// NewRedisConnection creates a new Redis connection manager
func NewRedisConnection(cfg RedisConfig) *RedisConnection {
	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)

	return &RedisConnection{
		addr:       addr,
		password:   cfg.Password,
		db:         cfg.DB,
		maxRetries: cfg.MaxRetries,
	}
}

// Connect establishes a connection to Redis with retry logic
func (r *RedisConnection) Connect(ctx context.Context) error {
	ctx, span := tracing.GetTracer("redis").Start(ctx, "redis.connect")
	defer span.End()

	traceID := tracing.GetTraceID(ctx)
	fmt.Printf("TraceID=%s Connecting to Redis at %s\n", traceID, r.addr)

	// Create Redis client
	r.client = redis.NewClient(&redis.Options{
		Addr:     r.addr,
		Password: r.password,
		DB:       r.db,
	})

	// Test the connection with multiple attempts
	var err error

	for attempt := 1; attempt <= r.maxRetries; attempt++ {
		fmt.Printf("TraceID=%s Attempt %d: Testing Redis connection\n", traceID, attempt)

		ctxWithTimeout, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = r.client.Ping(ctxWithTimeout).Result()
		cancel()

		if err == nil {
			fmt.Printf("TraceID=%s Successfully connected to Redis\n", traceID)
			return nil
		}

		fmt.Printf("TraceID=%s Redis connection error: %v\n", traceID, err)
		if attempt == r.maxRetries {
			return fmt.Errorf("failed to connect to Redis after %d attempts: %w", r.maxRetries, err)
		}

		select {
		case <-time.After(time.Duration(attempt*2) * time.Second):
			// Continue to next attempt
		case <-ctx.Done():
			return fmt.Errorf("context canceled while connecting to Redis: %w", ctx.Err())
		}
	}

	return fmt.Errorf("exhausted all connection attempts to Redis")
}

// Close closes the Redis connection
func (r *RedisConnection) Close() error {
	if r.client != nil {
		fmt.Println("Closing Redis connection...")
		err := r.client.Close()
		if err != nil {
			return fmt.Errorf("error closing Redis connection: %w", err)
		}
		fmt.Println("Redis connection closed")
	}
	return nil
}

// Client returns the underlying Redis client
func (r *RedisConnection) Client() *redis.Client {
	return r.client
}

// Check implements the health.Checker interface
func (r *RedisConnection) Check(ctx context.Context) error {
	if r.client == nil {
		return fmt.Errorf("Redis client not initialized")
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err := r.client.Ping(ctxWithTimeout).Result()
	if err != nil {
		return fmt.Errorf("Redis ping failed: %w", err)
	}

	return nil
}

// Name implements the health.Checker interface
func (r *RedisConnection) Name() string {
	return "redis"
}

// Ping pings the Redis server
func (r *RedisConnection) Ping(ctx context.Context) error {
	ctx, span := tracing.GetTracer("redis").Start(ctx, "redis.ping")
	defer span.End()

	if r.client == nil {
		return fmt.Errorf("Redis client not initialized")
	}

	_, err := r.client.Ping(ctx).Result()
	return err
}

// GetWithTracing gets a value from Redis with tracing
func (r *RedisConnection) GetWithTracing(ctx context.Context, key string) (string, error) {
	ctx, span := tracing.GetTracer("redis").Start(ctx, "redis.get")
	defer span.End()

	if r.client == nil {
		return "", fmt.Errorf("Redis client not initialized")
	}

	return r.client.Get(ctx, key).Result()
}

// SetWithTracing sets a value in Redis with tracing
func (r *RedisConnection) SetWithTracing(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	ctx, span := tracing.GetTracer("redis").Start(ctx, "redis.set")
	defer span.End()

	if r.client == nil {
		return fmt.Errorf("Redis client not initialized")
	}

	return r.client.Set(ctx, key, value, expiration).Err()
}

// SetupRedisNotifications ensures Redis keyspace notifications are enabled
func (r *RedisConnection) SetupRedisNotifications(ctx context.Context) error {
	ctx, span := tracing.GetTracer("redis").Start(ctx, "redis.setup_notifications")
	defer span.End()

	if r.client == nil {
		return fmt.Errorf("Redis client not initialized")
	}

	// Enable keyspace notifications for all events
	err := r.client.ConfigSet(ctx, "notify-keyspace-events", "KA").Err()
	if err != nil {
		return fmt.Errorf("failed to enable Redis keyspace notifications: %w", err)
	}

	return nil
}

// SubscribeToKeyspace subscribes to keyspace events
func (r *RedisConnection) SubscribeToKeyspace(ctx context.Context, patterns ...string) *redis.PubSub {
	if len(patterns) == 0 {
		patterns = []string{"__keyevent@0__:set", "__keyevent@0__:hset", "__keyspace@0__:*"}
	}

	return r.client.PSubscribe(ctx, patterns...)
}
