# ============================================================
# Multi-stage Dockerfile for aws-cur-scheduler
# Base image: golang:1.25-alpine
# Final image: distroless/static for minimal attack surface
# ============================================================

# ── Stage 1: Build ────────────────────────────────────────
FROM golang:1.25-alpine AS builder

# Install ca-certificates for HTTPS calls and git for go mod
RUN apk add --no-cache ca-certificates git tzdata

WORKDIR /app

# Cache dependencies first (layer caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
      -o /app/aws-cur-scheduler \
      ./cmd/scheduler

# ── Stage 2: Final ────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

# Copy timezone data and CA certs from builder
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy binary
COPY --from=builder /app/aws-cur-scheduler /aws-cur-scheduler

# Copy only example configs to avoid baking secrets into the image
# In production, mount secrets via environment variables or k8s Secrets/IRSA.
COPY --from=builder /app/configs/*.example /configs/

USER nonroot:nonroot

ENTRYPOINT ["/aws-cur-scheduler"]
