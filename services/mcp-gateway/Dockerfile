FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/mcp-gateway ./services/mcp-gateway
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mcp-gateway ./services/mcp-gateway/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/mcp-gateway /usr/local/bin/mcp-gateway
USER scos
EXPOSE 8098
ENTRYPOINT ["/usr/local/bin/mcp-gateway"]
