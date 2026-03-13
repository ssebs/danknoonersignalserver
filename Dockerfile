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
FROM alpine:latest

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/danknoonersignalserver .

# Expose the default port
EXPOSE 9080

# Run the server
ENTRYPOINT ["./danknoonersignalserver"]
CMD ["-addr", ":9080"]
