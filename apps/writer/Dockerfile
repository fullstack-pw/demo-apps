FROM golang:1.23.1-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

COPY shared/go.mod shared/go.sum ./shared/
COPY writer/go.mod writer/go.sum ./writer/

WORKDIR /build/writer
RUN go mod download

WORKDIR /build
COPY shared ./shared

COPY writer/main.go ./writer/

WORKDIR /build/writer
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

FROM alpine:3.19

RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    && rm -rf /var/cache/apk/*

WORKDIR /app

COPY --from=builder /build/writer/main .

EXPOSE 8080

CMD ["./main"]