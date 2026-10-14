# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o danknoonersignalserver .

# Runtime stage
FROM alpine:3.24

# Unprivileged user with a fixed UID so runAsNonRoot-style checks can verify it
RUN addgroup -S -g 10001 signal && adduser -S -D -H -u 10001 -G signal signal

WORKDIR /app

# Copy the binary from builder (root-owned, so the server can't modify itself)
COPY --from=builder /app/danknoonersignalserver .

USER 10001:10001

# Expose the default port
EXPOSE 9080

# Run the server
ENTRYPOINT ["./danknoonersignalserver"]
CMD ["-addr", ":9080"]
