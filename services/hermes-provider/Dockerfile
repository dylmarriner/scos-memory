FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/hermes-provider ./services/hermes-provider
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
COPY packages/database ./packages/database
COPY packages/service-state ./packages/service-state
COPY services/readiness ./services/readiness
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hermes-provider ./services/hermes-provider/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/hermes-provider /usr/local/bin/hermes-provider
USER scos
EXPOSE 8088
ENTRYPOINT ["/usr/local/bin/hermes-provider"]
