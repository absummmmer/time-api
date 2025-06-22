# Build stage
FROM golang:1.20-alpine AS builder

WORKDIR /app

# Copy go mod and sum files
COPY go.mod ./

# Download all dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o time-api .

# Final stage
FROM alpine:latest

# Install tzdata package for timezone support
RUN apk add --no-cache tzdata

WORKDIR /root/

# Copy the binary from builder
COPY --from=builder /app/time-api .

# Expose port 8080
EXPOSE 8080

# Command to run the executable
CMD ["./time-api"]
