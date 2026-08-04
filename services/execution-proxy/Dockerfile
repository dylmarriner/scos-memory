FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/execution-proxy ./services/execution-proxy
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
COPY packages/database ./packages/database
COPY packages/service-state ./packages/service-state
COPY packages/telemetry ./packages/telemetry
COPY services/readiness ./services/readiness
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/execution-proxy ./services/execution-proxy/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos \
    && mkdir -p /var/lib/scos/execution-proxy/raw /var/lib/scos/execution-proxy/state \
    && chown -R scos:scos /var/lib/scos
COPY --from=build /out/execution-proxy /usr/local/bin/execution-proxy
VOLUME ["/var/lib/scos/execution-proxy/raw", "/var/lib/scos/execution-proxy/state"]
USER scos
EXPOSE 8085
ENTRYPOINT ["/usr/local/bin/execution-proxy"]
