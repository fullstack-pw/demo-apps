# Build stage
FROM golang:1.23.1-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git

WORKDIR /build

# Copy go.mod and go.sum files first for better layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY enqueuer/go.mod enqueuer/go.sum ./enqueuer/

# Download dependencies (will be cached if go.mod/sum don't change)
WORKDIR /build/enqueuer
RUN go mod download

# Copy shared module
WORKDIR /build
COPY shared ./shared

# Copy enqueuer application code
COPY enqueuer/main.go ./enqueuer/

# Build the application
WORKDIR /build/enqueuer
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

# Runtime stage with Chrome
FROM alpine:3.19

# Install Chrome and dependencies with minimal layers
RUN apk update && apk add --no-cache \
    chromium \
    nss \
    freetype \
    freetype-dev \
    harfbuzz \
    ca-certificates \
    ttf-freefont \
    && rm -rf /var/cache/apk/*

# Set environment variables for Chrome
ENV CHROME_PATH=/usr/bin/chromium-browser \
    CHROME_BIN=/usr/bin/chromium-browser

# Copy the compiled application from builder stage
COPY --from=builder /build/enqueuer/main /app/main

WORKDIR /app

# Expose port
EXPOSE 8080

# Run the application
CMD ["./main"]