FROM golang:1.23.1

# Install Python and required packages for ASCII art conversion
RUN apt-get update && apt-get install -y \
    python3 \
    python3-pip \
    python3-venv \
    python3-full \
    && rm -rf /var/lib/apt/lists/*

# Create a virtual environment and install the ASCII art packages
RUN python3 -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# Install the ascii_magic Python package in the virtual environment
RUN pip install ascii_magic pillow

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

# Create scripts directory for the ASCII converter
RUN mkdir -p scripts

# Expose port 8080
EXPOSE 8080

# Run the application
CMD ["./main"]