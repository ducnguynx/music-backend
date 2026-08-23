# Stage 1: Build the Go binary
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY main.go .
RUN go build -o qr-logger main.go

# Stage 2: Create the minimal production image
FROM alpine:latest
WORKDIR /app

# Create the data directory for the log file
RUN mkdir -p /app/data

# Copy the binary from the builder stage
COPY --from=builder /app/qr-logger .

# Expose the port
EXPOSE 8080

# Run the binary
CMD ["./qr-logger"]