FROM golang:1.23.1

WORKDIR /app

COPY apps/share/ ./

COPY ./apps/enqueuer/main.go ./apps/enqueuer/go.mod ./apps/enqueuer/go.sum ./

# Download dependencies
RUN go mod tidy

# Build the application
RUN go build -o main .

# Expose port 8080
EXPOSE 8080

# Run the application
CMD ["./main"]
#