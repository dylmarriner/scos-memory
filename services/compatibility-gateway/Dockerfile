FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages/service-auth ./packages/service-auth
COPY services/compatibility-gateway ./services/compatibility-gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/compatibility-gateway ./services/compatibility-gateway/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/compatibility-gateway /usr/local/bin/compatibility-gateway
USER scos
EXPOSE 8087
ENTRYPOINT ["/usr/local/bin/compatibility-gateway"]
