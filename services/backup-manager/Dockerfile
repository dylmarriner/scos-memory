FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/backup-manager ./services/backup-manager
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./services/backup-manager/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos \
    && mkdir -p /var/lib/scos/backups /var/lib/scos/wal-archive \
    && chown -R scos:scos /var/lib/scos
COPY --from=build /out/server /usr/local/bin/server
VOLUME ["/var/lib/scos/backups", "/var/lib/scos/wal-archive"]
USER scos
EXPOSE 8095
ENTRYPOINT ["/usr/local/bin/server"]
