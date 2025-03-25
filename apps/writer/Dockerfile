FROM golang:1.23.1

WORKDIR /app

# Copy shared module first
COPY shared ./shared

# Copy writer app files
COPY writer/main.go writer/go.mod writer/go.sum ./writer/

# Change to writer directory
WORKDIR /app/writer

# Download dependencies
RUN go mod tidy

# Build the application
RUN go build -o main .

# Expose port 8080
EXPOSE 8080

# Run the application
CMD ["./main"]