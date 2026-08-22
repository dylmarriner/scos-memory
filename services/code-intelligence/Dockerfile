FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages/service-auth ./packages/service-auth
COPY services/code-intelligence ./services/code-intelligence
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/code-intelligence ./services/code-intelligence/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/code-intelligence /usr/local/bin/code-intelligence
USER scos
EXPOSE 8084
ENTRYPOINT ["/usr/local/bin/code-intelligence"]
