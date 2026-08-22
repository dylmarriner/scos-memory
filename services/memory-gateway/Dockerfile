FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY packages/service-auth ./packages/service-auth
COPY services/memory-gateway ./services/memory-gateway
RUN go mod download
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/memory-gateway ./services/memory-gateway/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/memory-gateway /usr/local/bin/memory-gateway
USER scos
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/memory-gateway"]
