# Build stage for Go application
FROM golang:1.23.1-alpine AS go-builder

# Install build dependencies
RUN apk add --no-cache git

WORKDIR /build

# Copy go.mod and go.sum files first for better layer caching
COPY shared/go.mod shared/go.sum ./shared/
COPY memorizer/go.mod memorizer/go.sum ./memorizer/

# Download dependencies (will be cached if go.mod/sum don't change)
WORKDIR /build/memorizer
RUN go mod download

# Copy shared module
WORKDIR /build
COPY shared ./shared

# Copy memorizer application code
COPY memorizer/main.go ./memorizer/

# Build the application
WORKDIR /build/memorizer
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

# Runtime stage with Python for ASCII art conversion
FROM python:3.11-slim

# Install only required packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-venv \
    && rm -rf /var/lib/apt/lists/*

# Set up virtual environment
RUN python3 -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# Install Python packages in a single layer
RUN pip install --no-cache-dir \
    ascii_magic \
    pillow

WORKDIR /app

# Create scripts directory for the ASCII converter
RUN mkdir -p scripts

# Copy the compiled application from builder stage
COPY --from=go-builder /build/memorizer/main .

# Expose port
EXPOSE 8080

# Run the application
CMD ["./main"]