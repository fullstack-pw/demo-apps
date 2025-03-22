FROM golang:1.23.1 AS builder

WORKDIR /app

# Copy go.mod and go.sum
COPY go.mod ./

# Download dependencies
RUN go mod tidy

# Copy the source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o enqueuer .

# Use a minimal alpine image for the final stage
FROM alpine:3.19

WORKDIR /app

# Install CA certificates for HTTPS connections
RUN apk --no-cache add ca-certificates

# Copy the binary from the builder stage
COPY --from=builder /app/enqueuer .

# Expose the application port
EXPOSE 8080

# Run the application
CMD ["./enqueuer"]