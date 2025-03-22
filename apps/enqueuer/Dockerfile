FROM golang:1.23.1

WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker cache
COPY main.go go.mod go.sum ./

# Download dependencies
RUN go mod tidy

# Build the application
RUN go build -o main .

# Expose port 8080
EXPOSE 8080

# Run the application
CMD ["./main"]