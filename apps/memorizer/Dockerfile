FROM golang:1.23.1

WORKDIR /app

# Copy shared module first
COPY shared ./shared

# Copy memorizer app files
COPY memorizer/main.go memorizer/go.mod memorizer/go.sum ./memorizer/

# Change to memorizer directory
WORKDIR /app/memorizer

# Download dependencies
RUN go mod tidy

# Build the application
RUN go build -o main .

# Expose port 8080
EXPOSE 8080

# Run the application
CMD ["./main"]