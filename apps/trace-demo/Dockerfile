FROM golang:1.23.1 AS builder

WORKDIR /app

# Copy source code
COPY . .

# Initialize Go module and set up dependencies
RUN go mod init trace-demo \
    && go mod edit -require github.com/lib/pq@v1.10.9 \
    && go mod edit -require go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.60.0 \
    && go mod edit -require go.opentelemetry.io/otel@v1.35.0 \
    && go mod edit -require go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.35.0 \
    && go mod edit -require go.opentelemetry.io/otel/sdk@v1.35.0 \
    && go mod tidy \
    && go mod download

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o trace-demo .

# Use a smaller base image for the final image
FROM alpine:3.18

# Install CA certificates and other dependencies
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy the binary from the builder stage
COPY --from=builder /app/trace-demo .

# Expose application port
EXPOSE 8080

# Set environment variables
ENV PORT=8080

# Run the application
CMD ["/app/trace-demo"]