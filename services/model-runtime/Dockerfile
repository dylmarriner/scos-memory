FROM golang:1.26.6-alpine AS build
ENV CGO_ENABLED=0 GOFLAGS=-buildvcs=false
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages ./packages
COPY services/model-runtime ./services/model-runtime
RUN go build -trimpath -ldflags="-s -w" -o /out/model-runtime ./services/model-runtime/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && \
    addgroup -S -g 101 scos && adduser -S -D -H -u 100 -G scos scos
COPY --from=build /out/model-runtime /usr/local/bin/model-runtime
USER scos
ENTRYPOINT ["model-runtime"]
