# syntax=docker/dockerfile:1

# ── Build stage ────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/sync ./cmd/sync

# ── Runtime stage ──────────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget && \
    adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/sync /app/sync

USER app
EXPOSE 50052

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=5 \
    CMD wget -qO- http://localhost:50052/_apicorex/health || exit 1

ENTRYPOINT ["/app/sync"]
