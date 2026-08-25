FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/punk ./cmd/punk

FROM alpine:3.21
RUN adduser -D -H punk
USER punk
WORKDIR /app
COPY --from=build /out/punk /usr/local/bin/punk
COPY specs /app/specs
COPY punk.example.yaml /app/config.yaml
EXPOSE 9090
ENTRYPOINT ["punk"]
CMD ["serve", "--config", "/app/config.yaml"]
