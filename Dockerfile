FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/punk ./cmd/punk

FROM alpine:3.21
# /data is the default volume mount point (PUNK_DB_DSN=/data/punk.db in
# docker-compose.yml). Docker initializes named volumes from the image,
# including ownership — creating it here owned by `punk` means a fresh
# `docker compose up` gets a writable /data instead of a root-owned one.
RUN adduser -D -H punk && mkdir -p /data && chown punk:punk /data
USER punk
WORKDIR /app
COPY --from=build /out/punk /usr/local/bin/punk
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY specs /app/specs
COPY punk.example.yaml /app/config.yaml
EXPOSE 9090
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["serve", "--config", "/app/config.yaml"]
