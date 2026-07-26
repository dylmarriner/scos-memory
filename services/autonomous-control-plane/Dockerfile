FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/autonomous-control-plane ./services/autonomous-control-plane
RUN CGO_ENABLED=0 go build -o /out/autonomous-control-plane ./services/autonomous-control-plane/cmd/server

FROM alpine:3.20
RUN adduser -D -H -u 10001 scos
USER scos
COPY --from=build /out/autonomous-control-plane /usr/local/bin/autonomous-control-plane
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/autonomous-control-plane"]
