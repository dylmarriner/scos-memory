FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/context-optimisation ./services/context-optimisation
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/context-optimisation ./services/context-optimisation/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/context-optimisation /usr/local/bin/context-optimisation
USER scos
EXPOSE 8083
ENTRYPOINT ["/usr/local/bin/context-optimisation"]
