FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/autonomous-control-plane ./services/autonomous-control-plane
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
COPY packages/database ./packages/database
COPY packages/telemetry ./packages/telemetry
COPY packages/capability-registry ./packages/capability-registry
COPY services/readiness ./services/readiness
RUN CGO_ENABLED=0 go build -o /out/autonomous-control-plane ./services/autonomous-control-plane/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos
USER scos
COPY --from=build /out/autonomous-control-plane /usr/local/bin/autonomous-control-plane
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/autonomous-control-plane"]
