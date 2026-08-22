FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages/service-auth ./packages/service-auth
COPY services/okf-synchroniser ./services/okf-synchroniser
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/okf-synchroniser ./services/okf-synchroniser/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/okf-synchroniser /usr/local/bin/okf-synchroniser
USER scos
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/okf-synchroniser"]
