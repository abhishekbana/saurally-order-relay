# ----------------------------------------------------
# Build stage
# ----------------------------------------------------
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Install CA certificates for HTTPS
RUN apk add --no-cache ca-certificates git

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" \
    -o saurally-order-relay

# ----------------------------------------------------
# Runtime stage
# ----------------------------------------------------
FROM gcr.io/distroless/base-debian12

WORKDIR /app

# Copy binary and certs
COPY --from=builder /app/saurally-order-relay /app/saurally-order-relay
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Non-root user (security)
USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/app/saurally-order-relay"]
