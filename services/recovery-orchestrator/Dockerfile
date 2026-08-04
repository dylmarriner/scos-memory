FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/recovery-orchestrator ./services/recovery-orchestrator
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
COPY packages/database ./packages/database
COPY packages/telemetry ./packages/telemetry
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./services/recovery-orchestrator/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/server /usr/local/bin/server
USER scos
EXPOSE 8094
ENTRYPOINT ["/usr/local/bin/server"]
