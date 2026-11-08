# Build stage
FROM golang:1.21-alpine AS builder

# Prefer the China mirror, then the official proxy, then direct downloads.
# The pipe separator makes the go command fall back on any network error.
ENV GOPROXY=https://goproxy.cn|https://proxy.golang.org|direct

WORKDIR /app

# Copy go.mod and go.sum so dependency downloads can be verified against the
# committed checksums and cached by Docker.
COPY go.mod go.sum ./

# Download all dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o time-api .

# Final stage
FROM alpine:latest

# Install tzdata for timezone support and CA certificates so that the DUT1
# provider can fetch the upstream HTTPS data source.
RUN apk add --no-cache tzdata ca-certificates

WORKDIR /root/

# Copy the binary from builder
COPY --from=builder /app/time-api .

# Expose port 8080
EXPOSE 8080

# Command to run the executable
CMD ["./time-api"]
