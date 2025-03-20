FROM golang:1.23.1 AS builder

WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker caching
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the application source code
COPY . .

# Build the application with proper settings for tracing
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