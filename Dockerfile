# Stage 1: Build binary with Go
FROM golang:1.22-alpine AS builder

WORKDIR /src

COPY . .

RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /movistar-uploader ./cmd/uploader

# Stage 2: Minimal final runtime image (~22 MB)
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /movistar-uploader /app/movistar-uploader

VOLUME ["/data", "/app/tokens"]

EXPOSE 5000

ENTRYPOINT ["/app/movistar-uploader"]
