# Stage 1: Build binary with Go
FROM golang:1.24-alpine AS builder

WORKDIR /src

COPY . .

RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /movistar-uploader ./cmd/uploader

# Stage 2: Minimal final runtime image (~22 MB)
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata su-exec && \
    adduser -D -u 10001 app && \
    mkdir -p /app/tokens /data && \
    chown -R app:app /app

WORKDIR /app

COPY --from=builder /movistar-uploader /app/movistar-uploader
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

VOLUME ["/data", "/app/tokens"]

EXPOSE 5000

ENTRYPOINT ["/app/docker-entrypoint.sh"]
