FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/agent-registry ./services/agent-registry
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
COPY packages/database ./packages/database
COPY packages/service-state ./packages/service-state
COPY services/readiness ./services/readiness
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./services/agent-registry/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos \
    && mkdir -p /var/lib/scos/agent-registry/state \
    && chown -R scos:scos /var/lib/scos
COPY --from=build /out/server /usr/local/bin/server
USER scos
EXPOSE 8093
ENTRYPOINT ["/usr/local/bin/server"]
