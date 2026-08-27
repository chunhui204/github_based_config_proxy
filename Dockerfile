FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /dist/config-server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /dist/local-verify ./cmd/local_verify

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl
ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /dist/config-server /app/config-server
COPY --from=builder /dist/local-verify /app/local-verify
COPY config/ /app/config/

EXPOSE 8080

ENTRYPOINT ["/app/config-server"]
CMD ["-config", "/app/config/server.json"]
