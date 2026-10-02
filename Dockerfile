FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o qr-logger .

FROM alpine:3.23
WORKDIR /app
RUN mkdir -p /app/data
COPY --from=builder /app/qr-logger .
EXPOSE 8080
CMD ["./qr-logger"]
