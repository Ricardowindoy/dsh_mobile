# Multi-stage build for dsh-relay.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dsh-relay ./cmd/relay

FROM alpine:3.20
RUN adduser -D -H -u 10001 app
COPY --from=build /out/dsh-relay /usr/local/bin/dsh-relay
USER app
ENTRYPOINT ["/usr/local/bin/dsh-relay"]
