FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages/service-auth ./packages/service-auth
COPY services/backup-manager ./services/backup-manager
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./services/backup-manager/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/server /usr/local/bin/server
USER scos
EXPOSE 8095
ENTRYPOINT ["/usr/local/bin/server"]
