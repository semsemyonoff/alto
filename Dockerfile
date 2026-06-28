# Stage 1: Build
FROM golang:1.26-alpine AS builder

WORKDIR /build

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /alto ./cmd/alto

# Stage 2: Runtime
FROM alpine:3.21

RUN apk add --no-cache ffmpeg

WORKDIR /app
COPY --from=builder /alto /app/alto
COPY --from=builder /build/web /app/web

EXPOSE 8080

ENTRYPOINT ["/app/alto"]
