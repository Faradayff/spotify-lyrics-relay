# Build stage
FROM golang:1.22-alpine AS build
WORKDIR /src

# Cargar dependencias primero (caché)
COPY go.mod ./
# No hay dependencias externas, pero mantenemos el paso para compatibilidad
RUN go mod download

# Compilar
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /relay .

# Runtime stage
FROM alpine:3.20
RUN addgroup -S relay && adduser -S relay -G relay
WORKDIR /app
COPY --from=build /relay /usr/local/bin/relay
# Estado (tokens.json) fuera del binario
RUN mkdir -p /data && chown relay:relay /data
ENV STATE_DIR=/data
USER relay
EXPOSE 8899
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -q --spider http://127.0.0.1:8899/ || exit 1
ENTRYPOINT ["/usr/local/bin/relay"]
