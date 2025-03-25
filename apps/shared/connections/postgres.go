package connections

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"shared/tracing"

	_ "github.com/lib/pq" // PostgreSQL driver
)

// PostgresConnection wraps a PostgreSQL connection with additional functionality
type PostgresConnection struct {
	db         *sql.DB
	connString string
	maxRetries int
}

// PostgresConfig holds configuration for the PostgreSQL connection
type PostgresConfig struct {
	Host       string
	Port       string
	Database   string
	User       string
	Password   string
	SSLMode    string
	MaxRetries int
}

// DefaultPostgresConfig returns default PostgreSQL configuration from environment
func DefaultPostgresConfig() PostgresConfig {
	// Get DB connection details from environment
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	sslMode := os.Getenv("DB_SSLMODE")

	// Set defaults if not provided
	if dbHost == "" {
		dbHost = "postgres.fullstack.pw"
	}
	if dbPort == "" {
		dbPort = "5432"
	}
	if dbName == "" {
		dbName = "postgres"
	}
	if dbUser == "" {
		dbUser = "admin"
	}
	if sslMode == "" {
		sslMode = "disable"
	}

	return PostgresConfig{
		Host:       dbHost,
		Port:       dbPort,
		Database:   dbName,
		User:       dbUser,
		Password:   dbPassword,
		SSLMode:    sslMode,
		MaxRetries: 5,
	}
}

// NewPostgresConnection creates a new PostgreSQL connection manager
func NewPostgresConnection(cfg PostgresConfig) *PostgresConnection {
	// Construct connection string
	connStr := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode,
	)

	return &PostgresConnection{
		connString: connStr,
		maxRetries: cfg.MaxRetries,
	}
}

// Connect establishes a connection to PostgreSQL with retry logic
func (p *PostgresConnection) Connect(ctx context.Context) error {
	ctx, span := tracing.GetTracer("postgres").Start(ctx, "postgres.connect")
	defer span.End()

	traceID := tracing.GetTraceID(ctx)
	fmt.Printf("TraceID=%s Connecting to PostgreSQL with connection string: %s\n",
		traceID, p.sanitizeConnString(p.connString))

	var err error

	for attempt := 1; attempt <= p.maxRetries; attempt++ {
		fmt.Printf("TraceID=%s Attempt %d: Connecting to PostgreSQL\n", traceID, attempt)

		// Attempt database connection
		p.db, err = sql.Open("postgres", p.connString)
		if err != nil {
			fmt.Printf("TraceID=%s Connection configuration error: %v\n", traceID, err)
			if attempt == p.maxRetries {
				return fmt.Errorf("failed to configure database connection after %d attempts: %w", p.maxRetries, err)
			}

			select {
			case <-time.After(time.Duration(attempt*2) * time.Second):
				// Continue to next attempt
			case <-ctx.Done():
				return fmt.Errorf("context canceled while connecting to PostgreSQL: %w", ctx.Err())
			}

			continue
		}

		// Set connection pool parameters
		p.db.SetMaxOpenConns(25)
		p.db.SetMaxIdleConns(5)
		p.db.SetConnMaxLifetime(5 * time.Minute)

		// Test the connection
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = p.db.PingContext(ctxWithTimeout)
		cancel()

		if err == nil {
			fmt.Printf("TraceID=%s Successfully connected to PostgreSQL\n", traceID)
			return nil
		}

		fmt.Printf("TraceID=%s Database ping failed: %v\n", traceID, err)
		_ = p.db.Close()

		if attempt == p.maxRetries {
			return fmt.Errorf("failed to ping database after %d attempts: %w", p.maxRetries, err)
		}

		select {
		case <-time.After(time.Duration(attempt*2) * time.Second):
			// Continue to next attempt
		case <-ctx.Done():
			return fmt.Errorf("context canceled while pinging PostgreSQL: %w", ctx.Err())
		}
	}

	return fmt.Errorf("exhausted all connection attempts to PostgreSQL")
}

// Close closes the PostgreSQL connection
func (p *PostgresConnection) Close() error {
	if p.db != nil {
		fmt.Println("Closing PostgreSQL connection...")
		err := p.db.Close()
		if err != nil {
			return fmt.Errorf("error closing PostgreSQL connection: %w", err)
		}
		fmt.Println("PostgreSQL connection closed")
	}
	return nil
}

// DB returns the underlying sql.DB instance
func (p *PostgresConnection) DB() *sql.DB {
	return p.db
}

// Check implements the health.Checker interface
func (p *PostgresConnection) Check(ctx context.Context) error {
	if p.db == nil {
		return fmt.Errorf("PostgreSQL connection not initialized")
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	err := p.db.PingContext(ctxWithTimeout)
	if err != nil {
		return fmt.Errorf("PostgreSQL ping failed: %w", err)
	}

	return nil
}

// Name implements the health.Checker interface
func (p *PostgresConnection) Name() string {
	return "postgres"
}

// Ping pings the PostgreSQL server
func (p *PostgresConnection) Ping() error {
	if p.db == nil {
		return fmt.Errorf("PostgreSQL connection not initialized")
	}

	return p.db.Ping()
}

// ExecuteWithTracing executes a SQL query with tracing
func (p *PostgresConnection) ExecuteWithTracing(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	ctx, span := tracing.GetTracer("postgres").Start(ctx, "postgres.execute")
	defer span.End()

	if p.db == nil {
		return nil, fmt.Errorf("PostgreSQL connection not initialized")
	}

	return p.db.ExecContext(ctx, query, args...)
}

// QueryWithTracing executes a SQL query with tracing and returns rows
func (p *PostgresConnection) QueryWithTracing(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	ctx, span := tracing.GetTracer("postgres").Start(ctx, "postgres.query")
	defer span.End()

	if p.db == nil {
		return nil, fmt.Errorf("PostgreSQL connection not initialized")
	}

	return p.db.QueryContext(ctx, query, args...)
}

// QueryRowWithTracing executes a SQL query with tracing and returns a single row
func (p *PostgresConnection) QueryRowWithTracing(ctx context.Context, query string, args ...interface{}) *sql.Row {
	ctx, span := tracing.GetTracer("postgres").Start(ctx, "postgres.query_row")
	defer span.End()

	if p.db == nil {
		return nil
	}

	return p.db.QueryRowContext(ctx, query, args...)
}

// BeginTx starts a transaction with tracing
func (p *PostgresConnection) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	ctx, span := tracing.GetTracer("postgres").Start(ctx, "postgres.begin_transaction")
	defer span.End()

	if p.db == nil {
		return nil, fmt.Errorf("PostgreSQL connection not initialized")
	}

	return p.db.BeginTx(ctx, opts)
}

// EnsureTable ensures a table exists with the given schema
func (p *PostgresConnection) EnsureTable(ctx context.Context, tableName, schema string) error {
	ctx, span := tracing.GetTracer("postgres").Start(ctx, "postgres.ensure_table")
	defer span.End()

	if p.db == nil {
		return fmt.Errorf("PostgreSQL connection not initialized")
	}

	query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", tableName, schema)
	_, err := p.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to create table %s: %w", tableName, err)
	}

	return nil
}

// sanitizeConnString removes the password from the connection string for logging
func (p *PostgresConnection) sanitizeConnString(connStr string) string {
	// This is a simple implementation - a more robust one would use regexp
	sanitized := ""
	inPassword := false
	for _, part := range connStr {
		if inPassword {
			if part == ' ' {
				inPassword = false
				sanitized += "[REDACTED] "
			}
		} else if part == 'p' && len(connStr) >= len(sanitized)+9 && connStr[len(sanitized):len(sanitized)+9] == "password=" {
			sanitized += "password="
			inPassword = true
		} else {
			sanitized += string(part)
		}
	}

	if inPassword {
		sanitized += "[REDACTED]"
	}

	return sanitized
}
