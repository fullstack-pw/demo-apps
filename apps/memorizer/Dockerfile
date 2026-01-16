FROM golang:1.23.1-alpine AS go-builder

RUN apk add --no-cache git

WORKDIR /build

COPY shared/go.mod shared/go.sum ./shared/
COPY memorizer/go.mod memorizer/go.sum ./memorizer/

WORKDIR /build/memorizer
RUN go mod download

WORKDIR /build
COPY shared ./shared

COPY memorizer/main.go ./memorizer/

WORKDIR /build/memorizer
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

FROM python:3.11-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-venv \
    && rm -rf /var/lib/apt/lists/*

RUN python3 -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

RUN pip install --no-cache-dir \
    ascii_magic \
    pillow

WORKDIR /app

RUN mkdir -p scripts

COPY --from=go-builder /build/memorizer/main .

EXPOSE 8080

CMD ["./main"]

