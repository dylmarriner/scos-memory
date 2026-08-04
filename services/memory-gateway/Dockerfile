FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY services/memory-gateway ./services/memory-gateway
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
COPY packages/database ./packages/database
RUN go mod download
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/memory-gateway ./services/memory-gateway/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/memory-gateway /usr/local/bin/memory-gateway
USER scos
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/memory-gateway"]
