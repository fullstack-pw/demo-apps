# Build stage
FROM golang:1.23.1-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git

WORKDIR /build

# Copy go.mod and go.sum files first for better layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY writer/go.mod writer/go.sum ./writer/

# Download dependencies (will be cached if go.mod/sum don't change)
WORKDIR /build/writer
RUN go mod download

# Copy shared module
WORKDIR /build
COPY shared ./shared

# Copy writer application code
COPY writer/main.go ./writer/

# Build the application
WORKDIR /build/writer
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

# Runtime stage
FROM alpine:3.19

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    && rm -rf /var/cache/apk/*

WORKDIR /app

# Copy the compiled application from builder stage
COPY --from=builder /build/writer/main .

# Expose port
EXPOSE 8080

# Run the application
CMD ["./main"]