FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages/service-auth ./packages/service-auth
COPY services/transparency-projection ./services/transparency-projection
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/transparency-projection ./services/transparency-projection/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/transparency-projection /usr/local/bin/transparency-projection
USER scos
EXPOSE 8097
ENTRYPOINT ["/usr/local/bin/transparency-projection"]
