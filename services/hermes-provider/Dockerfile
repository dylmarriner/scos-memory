FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages/service-auth ./packages/service-auth
COPY services/hermes-provider ./services/hermes-provider
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hermes-provider ./services/hermes-provider/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/hermes-provider /usr/local/bin/hermes-provider
USER scos
EXPOSE 8088
ENTRYPOINT ["/usr/local/bin/hermes-provider"]
