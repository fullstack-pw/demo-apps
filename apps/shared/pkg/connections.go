package pkg

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/nats-io/nats.go"
)

// NATSConfig contains configuration for NATS connection
type NATSConfig struct {
	URL        string
	MaxRetries int
	RetryDelay time.Duration
}

// RedisConfig contains configuration for Redis connection
type RedisConfig struct {
	Host       string
	Port       string
	Password   string
	MaxRetries int
	RetryDelay time.Duration
}

// PostgresConfig contains configuration for PostgreSQL connection
type PostgresConfig struct {
	Host       string
	Port       string
	Database   string
	User       string
	Password   string
	MaxRetries int
	RetryDelay time.Duration
}

// DefaultNATSConfig returns a config with sensible defaults
func DefaultNATSConfig() NATSConfig {
	return NATSConfig{
		URL:        os.Getenv("NATS_URL"),
		MaxRetries: 5,
		RetryDelay: 2 * time.Second,
	}
}

// DefaultRedisConfig returns a config with sensible defaults
func DefaultRedisConfig() RedisConfig {
	return RedisConfig{
		Host:       os.Getenv("REDIS_HOST"),
		Port:       os.Getenv("REDIS_PORT"),
		Password:   os.Getenv("REDIS_PASSWORD"),
		MaxRetries: 5,
		RetryDelay: 2 * time.Second,
	}
}

// DefaultPostgresConfig returns a config with sensible defaults
func DefaultPostgresConfig() PostgresConfig {
	return PostgresConfig{
		Host:       os.Getenv("DB_HOST"),
		Port:       os.Getenv("DB_PORT"),
		Database:   os.Getenv("DB_NAME"),
		User:       os.Getenv("DB_USER"),
		Password:   os.Getenv("DB_PASSWORD"),
		MaxRetries: 5,
		RetryDelay: 2 * time.Second,
	}
}

// ConnectNATS establishes a connection to NATS with retries
func ConnectNATS(config NATSConfig) (*nats.Conn, error) {
	fmt.Println("Initializing NATS connection...")

	// Apply defaults
	if config.URL == "" {
		config.URL = "nats://nats.fullstack.pw:4222"
	}
	fmt.Printf("Using NATS server: %s\n", config.URL)

	// Multiple connection attempts with exponential backoff
	var natsConn *nats.Conn
	var err error

	for attempt := 1; attempt <= config.MaxRetries; attempt++ {
		fmt.Printf("Attempt %d: Connecting to NATS at %s\n", attempt, config.URL)

		// Attempt NATS connection
		natsConn, err = nats.Connect(config.URL)
		if err != nil {
			fmt.Printf("NATS connection error: %v\n", err)
			if attempt == config.MaxRetries {
				return nil, fmt.Errorf("failed to connect to NATS after %d attempts: %w", config.MaxRetries, err)
			}
			time.Sleep(time.Duration(attempt) * config.RetryDelay)
			continue
		}

		fmt.Println("Successfully connected to NATS")
		return natsConn, nil
	}

	return nil, fmt.Errorf("exhausted all connection attempts to NATS")
}

// ConnectRedis establishes a connection to Redis with retries
func ConnectRedis(config RedisConfig) (*redis.Client, error) {
	fmt.Println("Initializing Redis connection...")

	// Apply defaults
	if config.Host == "" {
		config.Host = "redis.fullstack.pw"
	}
	if config.Port == "" {
		config.Port = "6379"
	}

	redisAddr := fmt.Sprintf("%s:%s", config.Host, config.Port)
	fmt.Printf("Connecting to Redis at %s\n", redisAddr)

	// Create Redis client
	redisClient := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: config.Password,
		DB:       0, // use default DB
	})

	// Multiple connection attempts with exponential backoff
	for attempt := 1; attempt <= config.MaxRetries; attempt++ {
		fmt.Printf("Attempt %d: Testing Redis connection\n", attempt)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := redisClient.Ping(ctx).Result()
		cancel()

		if err == nil {
			fmt.Println("Successfully connected to Redis")
			return redisClient, nil
		}

		fmt.Printf("Redis connection error: %v\n", err)
		if attempt == config.MaxRetries {
			return nil, fmt.Errorf("failed to connect to Redis after %d attempts: %w", config.MaxRetries, err)
		}
		time.Sleep(time.Duration(attempt) * config.RetryDelay)
	}

	return nil, fmt.Errorf("exhausted all connection attempts to Redis")
}

// ConnectPostgres establishes a connection to PostgreSQL with retries
func ConnectPostgres(config PostgresConfig) (*sql.DB, error) {
	fmt.Println("Initializing PostgreSQL connection...")

	// Apply defaults
	if config.Host == "" {
		config.Host = "postgres.fullstack.pw"
	}
	if config.Port == "" {
		config.Port = "5432"
	}
	if config.Database == "" {
		config.Database = "postgres"
	}
	if config.User == "" {
		config.User = "admin"
	}

	// Construct connection string
	connStr := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		config.Host, config.Port, config.User, config.Password, config.Database,
	)

	// Multiple connection attempts with exponential backoff
	var db *sql.DB
	var err error

	for attempt := 1; attempt <= config.MaxRetries; attempt++ {
		fmt.Printf("Attempt %d: Connecting to PostgreSQL at %s:%s/%s\n",
			attempt, config.Host, config.Port, config.Database)

		// Attempt database connection
		db, err = sql.Open("postgres", connStr)
		if err != nil {
			fmt.Printf("Connection configuration error: %v\n", err)
			if attempt == config.MaxRetries {
				return nil, fmt.Errorf("failed to configure database connection after %d attempts", config.MaxRetries)
			}
			time.Sleep(time.Duration(attempt) * config.RetryDelay)
			continue
		}

		// Test the connection
		err = db.Ping()
		if err != nil {
			fmt.Printf("Database ping failed: %v\n", err)
			_ = db.Close()
			if attempt == config.MaxRetries {
				return nil, fmt.Errorf("failed to ping database after %d attempts", config.MaxRetries)
			}
			time.Sleep(time.Duration(attempt) * config.RetryDelay)
			continue
		}

		// Successfully connected
		fmt.Printf("Successfully connected to PostgreSQL at %s:%s/%s\n",
			config.Host, config.Port, config.Database)
		return db, nil
	}

	return nil, fmt.Errorf("exhausted all connection attempts to PostgreSQL")
}

// EnsureMessagesTable ensures the messages table exists in PostgreSQL
func EnsureMessagesTable(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id VARCHAR(255) PRIMARY KEY,
			content TEXT NOT NULL,
			source VARCHAR(255),
			headers JSONB,
			trace_id VARCHAR(255),
			created_at TIMESTAMPTZ DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create messages table: %w", err)
	}

	fmt.Println("Database table 'messages' verified/created successfully")
	return nil
}
