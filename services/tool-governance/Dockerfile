FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages/service-auth ./packages/service-auth
COPY services/tool-governance ./services/tool-governance
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./services/tool-governance/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/server /usr/local/bin/server
USER scos
EXPOSE 8092
ENTRYPOINT ["/usr/local/bin/server"]
