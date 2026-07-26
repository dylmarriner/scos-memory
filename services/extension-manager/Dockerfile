FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/extension-manager ./services/extension-manager
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/extension-manager ./services/extension-manager/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/extension-manager /usr/local/bin/extension-manager
USER scos
EXPOSE 8086
ENTRYPOINT ["/usr/local/bin/extension-manager"]
