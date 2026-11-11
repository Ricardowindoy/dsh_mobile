# Multi-stage build for dsh-tunnel.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dsh-tunnel ./cmd/tunnel

FROM alpine:3.20
RUN adduser -D -H -u 10001 app
COPY --from=build /out/dsh-tunnel /usr/local/bin/dsh-tunnel
USER app
ENTRYPOINT ["/usr/local/bin/dsh-tunnel"]
