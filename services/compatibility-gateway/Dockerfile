FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/compatibility-gateway ./services/compatibility-gateway
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
COPY packages/database ./packages/database
COPY packages/telemetry ./packages/telemetry
COPY services/readiness ./services/readiness
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/compatibility-gateway ./services/compatibility-gateway/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/compatibility-gateway /usr/local/bin/compatibility-gateway
USER scos
EXPOSE 8087
ENTRYPOINT ["/usr/local/bin/compatibility-gateway"]
