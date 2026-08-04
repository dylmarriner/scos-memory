FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/security-gate ./services/security-gate
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
COPY packages/database ./packages/database
COPY packages/service-state ./packages/service-state
COPY services/readiness ./services/readiness
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./services/security-gate/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/server /usr/local/bin/server
USER scos
EXPOSE 8091
ENTRYPOINT ["/usr/local/bin/server"]
