FROM golang:1.23.1

WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker cache
COPY enqueuer/main.go enqueuer/go.mod enqueuer/go.sum  share/ ./

# Download dependencies
RUN go mod tidy

# Build the application
RUN go build -o main .

# Expose port 8080
EXPOSE 8080

# Run the application
CMD ["./main"]
#