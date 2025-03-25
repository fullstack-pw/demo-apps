FROM golang:1.23.1

WORKDIR /app

COPY shared ./shared

COPY enqueuer/main.go enqueuer/go.mod enqueuer/go.sum ./enqueuer/


WORKDIR /app/enqueuer

# Download dependencies
RUN go mod tidy

# Build the application
RUN go build -o main .

# Expose port 8080
EXPOSE 8080

# Run the application
CMD ["./main"]
#