FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o peercast-mi .

FROM alpine:3.21

RUN addgroup -S peercast && adduser -S peercast -G peercast

WORKDIR /app

COPY --from=builder /app/peercast-mi .

USER peercast

EXPOSE 1935
EXPOSE 7144

ENTRYPOINT ["./peercast-mi"]
CMD ["-config", "/config/config.toml"]
