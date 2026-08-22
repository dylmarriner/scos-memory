FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages/service-auth ./packages/service-auth
COPY services/execution-proxy ./services/execution-proxy
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/execution-proxy ./services/execution-proxy/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos \
    && mkdir -p /var/lib/scos/execution-proxy/raw \
    && chown -R scos:scos /var/lib/scos
COPY --from=build /out/execution-proxy /usr/local/bin/execution-proxy
VOLUME ["/var/lib/scos/execution-proxy/raw"]
USER scos
EXPOSE 8085
ENTRYPOINT ["/usr/local/bin/execution-proxy"]
