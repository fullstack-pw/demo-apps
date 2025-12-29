FROM golang:1.23.1-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

COPY shared/go.mod shared/go.sum ./shared/
COPY enqueuer/go.mod enqueuer/go.sum ./enqueuer/

WORKDIR /build/enqueuer
RUN go mod download

WORKDIR /build
COPY shared ./shared

COPY enqueuer/main.go ./enqueuer/

WORKDIR /build/enqueuer
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

FROM alpine:3.19

RUN apk update && apk add --no-cache \
    chromium \
    nss \
    freetype \
    freetype-dev \
    harfbuzz \
    ca-certificates \
    ttf-freefont \
    && rm -rf /var/cache/apk/*

ENV CHROME_PATH=/usr/bin/chromium-browser \
    CHROME_BIN=/usr/bin/chromium-browser

COPY --from=builder /build/enqueuer/main /app/main

WORKDIR /app

EXPOSE 8080

CMD ["./main"]
