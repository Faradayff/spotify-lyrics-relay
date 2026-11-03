# Build stage
FROM golang:1.22-alpine AS build
WORKDIR /src

# Load dependencies first (layer caching)
COPY go.mod ./
# No external dependencies, kept for future-proofing
RUN go mod download

# Compile
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /relay .

# Runtime stage
FROM alpine:3.20
RUN addgroup -S relay && adduser -S relay -G relay
WORKDIR /app
COPY --from=build /relay /usr/local/bin/relay
# State (tokens.json + lyrics cache) kept outside the binary
RUN mkdir -p /data && chown relay:relay /data
# Lyrics cache dir pre-created so the (named) state volume inherits ownership
RUN mkdir -p /data/lyrics && chown relay:relay /data/lyrics
ENV STATE_DIR=/data
USER relay
EXPOSE 8899
# Liveness probe: /healthz never calls out to Spotify, so it stays fast
# (a 5 s budget is plenty) regardless of RELAY_BASE_PATH.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -q --spider http://127.0.0.1:8899/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/relay"]
