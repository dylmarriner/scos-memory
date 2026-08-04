FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/transparency-projection ./services/transparency-projection
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/transparency-projection ./services/transparency-projection/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/transparency-projection /usr/local/bin/transparency-projection
COPY dashboard/index.html /usr/local/share/scos/dashboard/index.html
USER scos
EXPOSE 8097
ENTRYPOINT ["/usr/local/bin/transparency-projection"]
