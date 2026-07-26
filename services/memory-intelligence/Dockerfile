FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/memory-intelligence ./services/memory-intelligence
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/memory-intelligence ./services/memory-intelligence/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/memory-intelligence /usr/local/bin/memory-intelligence
USER scos
EXPOSE 8089
ENTRYPOINT ["/usr/local/bin/memory-intelligence"]
