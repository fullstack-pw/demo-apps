FROM golang:1.23.1

WORKDIR /app

# Copy go.mod and go.sum
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the remaining source code
COPY . .

# Build the application
RUN go build -o main .

# Expose port 8080
EXPOSE 8080

# Command to run the application
CMD ["./main"]
